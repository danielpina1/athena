package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// nowFile is the short, hand-kept list of what is happening and what comes next. It is git-ignored: it changes far
// more often than anything worth a commit.
const nowFile = ".superpowers/now.md"

// Section is one heading of the now file with its list items.
type Section struct {
	Title string
	Items []Item
}

// Item is one list item; Done items are written "- [x] text".
type Item struct {
	Text string
	Done bool
}

// ReadNow parses the now file: "# Heading" starts a section and "- text" adds an item to it. Anything else is ignored.
func ReadNow(root string) ([]Section, time.Time) {
	path := filepath.Join(root, nowFile)
	var modified time.Time
	if st, err := os.Stat(path); err == nil {
		modified = st.ModTime()
	}
	var sections []Section
	for _, raw := range lines(path) {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "#"):
			sections = append(sections, Section{Title: strings.TrimSpace(strings.TrimLeft(line, "#"))})
		case strings.HasPrefix(line, "- ") && len(sections) > 0:
			text := strings.TrimSpace(line[2:])
			item := Item{Text: text}
			for _, mark := range []string{"[x] ", "[X] "} {
				if strings.HasPrefix(text, mark) {
					item = Item{Text: strings.TrimSpace(text[len(mark):]), Done: true}
				}
			}
			text = strings.TrimPrefix(item.Text, "[ ] ")
			item.Text = text
			last := &sections[len(sections)-1]
			last.Items = append(last.Items, item)
		}
	}
	return sections, modified
}

// PR is an open pull request and the state of its checks.
type PR struct {
	Number                int
	Title, Branch         string
	Passed, Failed, Other int
}

// Checks says how a PR's checks stand, in a few words.
func (p PR) Checks() string {
	switch {
	case p.Failed > 0:
		return plain(p.Failed, "check failing")
	case p.Other > 0:
		return plain(p.Other, "check running")
	case p.Passed > 0:
		return plain(p.Passed, "check passed")
	}
	return "no checks"
}

func plain(n int, what string) string {
	s := strings.Replace(what, "check", "checks", 1)
	if n == 1 {
		s = what
	}
	return itoa(n) + " " + s
}

// parsePRs reads the JSON of `gh pr list --json number,title,headRefName,statusCheckRollup`.
func parsePRs(data []byte) ([]PR, error) {
	var raw []struct {
		Number            int    `json:"number"`
		Title             string `json:"title"`
		HeadRefName       string `json:"headRefName"`
		StatusCheckRollup []struct {
			Conclusion string `json:"conclusion"` // check runs
			State      string `json:"state"`      // commit statuses
		} `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	prs := make([]PR, 0, len(raw))
	for _, r := range raw {
		p := PR{Number: r.Number, Title: r.Title, Branch: r.HeadRefName}
		for _, c := range r.StatusCheckRollup {
			result := c.Conclusion
			if result == "" {
				result = c.State
			}
			switch result {
			case "SUCCESS", "NEUTRAL", "SKIPPED":
				p.Passed++
			case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
				p.Failed++
			default:
				p.Other++
			}
		}
		prs = append(prs, p)
	}
	return prs, nil
}

// fetchPRs asks GitHub for the open pull requests. ok is false when gh could not answer, so the caller keeps what it
// last knew instead of showing that everything has closed.
func fetchPRs(root string) (prs []PR, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--state", "open", "--limit", "20",
		"--json", "number,title,headRefName,statusCheckRollup")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	prs, err = parsePRs(out)
	return prs, err == nil
}
