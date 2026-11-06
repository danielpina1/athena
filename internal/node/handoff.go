package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/redact"
	"github.com/danielpina1/athena/internal/render"
)

// When a session stops at its usage limit, the Ticket continues in a new session, most often on the other runtime,
// which knows nothing of the work: its first message carries a handoff note after the worker preamble. It goes to a new
// worker session whose previous worker Assignment ended at its usage limit, and to a session that resumes after
// another runtime worked on the Ticket in between (that runtime's work is news to it); never to a review, an ordinary
// retry or a crash recovery (preflight F-27).

const (
	// handoffOpening opens the note after a usage limit (%s: the runtime it stopped on); handoffMovedOpening after
	// another runtime worked on the Ticket for another reason.
	handoffOpening      = "You are continuing this Ticket from a session on %s that stopped at its usage limit. Its work in progress is in your directory; nothing was lost. What it knew follows."
	handoffMovedOpening = "You are continuing this Ticket after a session on %s worked on it. Its work is in your directory. What it knew follows."
	// handoffInputs is how many of the Ticket's latest Inputs the note repeats; handoffReports how many Reports, each
	// cut to handoffReportMax.
	handoffInputs    = 20
	handoffReports   = 3
	handoffReportMax = 2 << 10
	// handoffGitMax caps the git log and the diff stat each; handoffLastTurnMax the previous session's last message,
	// which is looked for in the transcript's last lastTurnScan bytes.
	handoffGitMax      = 4 << 10
	handoffLastTurnMax = 4 << 10
	lastTurnScan       = 1 << 20
	// handoffGitTimeout bounds the git calls that read the worktree's state for a note.
	handoffGitTimeout = 30 * time.Second
	// handoffMax caps the whole note: the Inputs get what the rest leaves.
	handoffMax = 32 << 10
	// cutMark marks where the note cut something.
	cutMark = "[…]"
)

// Handoff is what a handoff note says: the Ticket, the previous worker Assignment (Runtime: the one it ran on), the
// Ticket's latest Inputs as renderInputs renders them, the Ticket's Reports oldest first, the branch's commits since
// its base (git log --oneline), its uncommitted changes (git diff --stat HEAD), the number of its untracked files and
// the previous session's last message.
type Handoff struct {
	Ticket                      core.Ticket
	Previous                    core.Assignment
	Inputs                      string
	Reports                     []core.Report
	GitLog, DiffStat, Untracked string
	LastTurn                    string
}

// handoffNote is the note h says, at most handoffMax bytes. Everything but its own lines is quoted (quoteText), so
// nothing in it reads as an Input header the node writes; cuts are marked cutMark. A cut that keeps a start leaves
// every line's start as it was; one that keeps an end is quoted again (quotedEnd), since its first line starts
// wherever the cut fell.
func handoffNote(h Handoff) string {
	runtime := h.Previous.Runtime
	if runtime == "" {
		runtime = ranOn(h.Ticket, h.Previous)
	}
	opening := handoffMovedOpening
	if h.Previous.EndedReason == core.ReasonUsageLimit {
		opening = handoffOpening
	}
	var rest strings.Builder
	rest.WriteString("\n\nIts latest reports:\n")
	reports := h.Reports[max(len(h.Reports)-handoffReports, 0):]
	if len(reports) == 0 {
		rest.WriteString("(none)\n")
	}
	for _, r := range reports {
		fmt.Fprintf(&rest, "- %s: %s\n", r.State, keepStart(quoteText(redact.String(strings.TrimSpace(r.Summary))), handoffReportMax))
	}
	untracked := h.Untracked
	if untracked == "" {
		untracked = "0"
	}
	fmt.Fprintf(&rest, "\nThe branch's commits since its base (git log --oneline):\n%s\nUncommitted changes (git diff --stat HEAD):\n%s\nUntracked files: %s\n",
		orNone(keepStart(quoteText(redact.String(h.GitLog)), handoffGitMax)),
		orNone(keepStart(quoteText(redact.String(h.DiffStat)), handoffGitMax)), quoteText(untracked))
	fmt.Fprintf(&rest, "\nWhat it said last:\n%s\n", orNone(quotedEnd(quoteText(redact.String(strings.TrimSpace(h.LastTurn))), handoffLastTurnMax)))

	head := fmt.Sprintf(opening, core.RuntimeLabel(runtime)) + "\n\nThe Ticket's latest messages, quoted:\n"
	room := handoffMax - len(head) - rest.Len()
	inputs := orNone(quoteText(strings.TrimRight(h.Inputs, "\n")))
	return keepStart(head+quotedEnd(inputs, max(room, len(cutMark)+1))+rest.String(), handoffMax)
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

// keepStart is s cut to at most n bytes on a rune boundary, keeping its start; a cut ends with cutMark.
func keepStart(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := max(n-len(cutMark), 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + cutMark
}

// quotedEnd is quoted text s cut to at most n bytes keeping its end (keepEnd), its cut line quoted again: a cut can
// fall between a quoted line's leading space and what it quotes, and the mark itself starts like a header.
func quotedEnd(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return quoteText(keepEnd(s, n-1)) // only the first line can need the space
}

// keepEnd is s cut to at most n bytes on a rune boundary, keeping its end; a cut starts with cutMark.
func keepEnd(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - max(n-len(cutMark), 0)
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return cutMark + s[cut:]
}

// handoffFor is the handoff note of the first message of a new process of the runner's Assignment on runtime, which
// resumes session resumeID ("" for a new session), or "" when it gets none (handoffFrom). failover reports a limit's
// move to another runtime: the process's marker says so.
func (r *runner) handoffFor(runtime, resumeID string) (note string, failover bool) {
	s, n := r.s, r.n
	if s.a.Role != "" {
		return "", false
	}
	t, _, err := r.eng.Ticket(s.t.ID)
	if err != nil {
		r.log.Warn("read ticket for the handoff", "error", err)
		t = s.t
	}
	prev, failover := handoffFrom(t, s.a, runtime, resumeID)
	if prev == nil {
		return "", false
	}
	h := Handoff{Ticket: t, Previous: *prev}
	ids := make([]string, 0, handoffInputs)
	for _, in := range t.Inputs[max(len(t.Inputs)-handoffInputs, 0):] {
		ids = append(ids, in.ID)
	}
	h.Inputs = renderInputs(t, ids, r.trigger, r.n.trustOf)
	for _, a := range t.Assignments {
		if a.Report != nil && a.ID != s.a.ID {
			h.Reports = append(h.Reports, *a.Report)
		}
	}
	h.GitLog, h.DiffStat, h.Untracked = n.handoffGit(s)
	trPath := n.transcriptPath(t.ID)
	if trPath == "" {
		trPath = filepath.Join(s.place.workDir, ".athena", "transcript.jsonl")
	}
	h.LastTurn = lastSay(trPath)
	return handoffNote(h), failover
}

// handoffFrom is the previous worker Assignment whose work a session of Assignment a on runtime (resuming resumeID, ""
// for a new session) hears of, its Runtime the one it ran on; nil when the session gets no handoff: a is no worker, no
// earlier worker Assignment ran, a new session's previous one did not end at its usage limit or, on another runtime,
// for a review (a retry, a reopened Ticket), or a resumed session's previous one ran on its own runtime (it remembers
// that work). failover reports a limit's move to another runtime.
func handoffFrom(t core.Ticket, a core.Assignment, runtime, resumeID string) (prev *core.Assignment, failover bool) {
	if a.Role != "" {
		return nil, false
	}
	for i := len(t.Assignments) - 1; i >= 0 && prev == nil; i-- {
		if p := t.Assignments[i]; p.ID != a.ID && p.Role == "" && ranOn(t, p) != "" {
			prev = &p
			prev.Runtime = ranOn(t, p)
		}
	}
	if prev == nil {
		return nil, false
	}
	limited, moved := prev.EndedReason == core.ReasonUsageLimit, prev.Runtime != runtime
	// A review's changes that reach a new session on another runtime (the implementer's was limited) carry its work too.
	handsOver := limited || (prev.EndedReason == core.ReasonReview && moved)
	if (resumeID == "" && !handsOver) || (resumeID != "" && !moved) {
		return nil, false
	}
	return prev, limited && moved
}

// handoffGit is the git state of the session's worktree for a handoff note: the branch's commits since its base, the
// uncommitted changes and the count of untracked files; all "" outside a git worktree or when git fails (logged).
func (n *Node) handoffGit(s *session) (log, stat, untracked string) {
	p := s.place
	if p.repo == "" {
		return "", "", ""
	}
	env, err := n.worktreeEnv(p, s.t)
	if err != nil {
		s.log.Warn("handoff: no git state", "error", err)
		return "", "", ""
	}
	ctx, cancel := context.WithTimeout(n.ctx, handoffGitTimeout)
	defer cancel()
	git := func(args ...string) string {
		out, err := gitInCtx(ctx, p.workDir, env, args...)
		if err != nil {
			s.log.Warn("handoff: git", "args", strings.Join(args, " "), "error", err)
		}
		return out
	}
	if base := n.branchBase(ctx, p, env); base != "" {
		log = git("log", "--oneline", base+"..HEAD", "-20")
	}
	stat = git("diff", "--stat", "HEAD")
	untracked = strconv.Itoa(strings.Count(git("ls-files", "--others", "--exclude-standard", "-z"), "\x00"))
	return log, stat, untracked
}

// branchBase is the commit a Ticket's branch is measured from: for a GitHub Ticket the clone's origin/HEAD, else (or
// without it) the HEAD of the repository the worktree comes from; "" when neither resolves.
func (n *Node) branchBase(ctx context.Context, p placement, wenv []string) string {
	if p.github != "" {
		if out, err := gitInCtx(ctx, p.workDir, wenv, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/HEAD^{commit}"); err == nil {
			return out
		}
	}
	common := p.common
	if common == "" {
		var err error
		if common, err = repoGitDir(p.repo); err != nil {
			return ""
		}
	}
	base := nodeGitBaseEnv()
	if p.github != "" {
		base = cloneGitEnv(p.repo, p.github)
	}
	out, err := gitInCtx(ctx, p.repo, repoGitEnv(base, common, p.repo), "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return ""
	}
	return out
}

// lastSay is the text of the last "say" item (render.Items) of an implementer in the transcript's last lastTurnScan
// bytes: what the previous worker session said last, redacted; "" when there is none. What follows a review's session
// marker (role review) is the reviewer's, until the next marker of another role.
func lastSay(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	off := max(st.Size()-lastTurnScan, 0)
	buf, err := io.ReadAll(io.NewSectionReader(f, off, st.Size()-off))
	if err != nil {
		return ""
	}
	lines := bytes.Split(buf, []byte("\n"))
	if off > 0 {
		lines = lines[1:] // the first line is cut
	}
	last, review := "", false
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if role, ok := markerRole(line); ok {
			review = role == core.RoleReview
			continue
		}
		if review {
			continue
		}
		for _, it := range render.Items(line, 0) {
			if it.Kind == "say" && strings.TrimSpace(it.Text) != "" {
				last = it.Text
			}
		}
	}
	return last
}

// markerRole is the role of a session marker line (render.MarkerLine); ok is false for any other line. Only a top-level
// "athena":"session" is a marker, as for render.
func markerRole(line []byte) (role string, ok bool) {
	if !bytes.Contains(line, []byte(`"athena"`)) {
		return "", false
	}
	var m struct {
		Athena json.RawMessage `json:"athena"`
		Role   string          `json:"role"`
	}
	if json.Unmarshal(line, &m) != nil || string(m.Athena) != `"session"` {
		return "", false
	}
	return m.Role, true
}
