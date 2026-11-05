package render

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/redact"
)

// A worker session echoes each message it takes in (--replay-user-messages). The message is what the node composed
// (internal/node: sendFirst, renderInputs, inputCard, the nudge and the resume notes): a preamble, then Input blocks
// headed "[input <id>: <what>]". inputItems turns it into one "user" item per block, its sender in Tool, its text
// without the protocol (ids, trigger ids, card lines), and for a GitHub card a Detail of who wrote it and where:
//
//	task    the request the Ticket started from (the first Input after the worker preamble)
//	owner   a message from the owner
//	github  a card of a Trigger from a Source's thread: Detail {"from","kind","url"}
//	athena  Athena's own messages: the report nudge, the resume notes, notes, a reopening, other messages
//
// A message with no Input header is the owner's (old transcripts, and the e2e fixtures). The node's tests
// (TestEchoedInputsRenderAsTheirSenders) hold this to the real formats.

const (
	workerPreamble = "You are the worker of Athena Ticket "
	nudgeText      = "Your turn ended without a report."
	recoveryNote   = "Your previous process ended unexpectedly"
	resumeNote     = "Your session was paused while it waited"
)

var (
	inputHeader = regexp.MustCompile(`(?m)^\[input [^\]\s]+: ([^\]\n]*)\]$`)
	cardLine    = regexp.MustCompile(`^(trigger_id|acknowledged|from|at|thread|url|replies_to|context|routing note): (.*)$`)
	cutLine     = regexp.MustCompile(`\n\[cut: [^\]\n]*\]\s*$`)
)

// githubFrom is a card's Detail.
type githubFrom struct {
	From string `json:"from"`
	Kind string `json:"kind"`
	URL  string `json:"url"`
}

// inputItems renders an echoed message: the items carry no offset or time (the caller adds them).
func inputItems(text string) []*athenav1.StreamItem {
	var out []*athenav1.StreamItem
	add := func(from, text, detail string) {
		out = append(out, &athenav1.StreamItem{Kind: "user", Tool: from, Text: redactCut(text, SayMaxBytes), Detail: detail})
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	trimmed := strings.TrimSpace(text)
	switch {
	case strings.HasPrefix(trimmed, nudgeText):
		add("athena", "Athena asked for a status report", "")
		return out
	case strings.HasPrefix(trimmed, recoveryNote):
		add("athena", "Athena restarted the session after its process ended", "")
	case strings.HasPrefix(trimmed, resumeNote):
		add("athena", "Athena resumed the paused session", "")
	}
	first := strings.HasPrefix(trimmed, workerPreamble)
	heads := inputHeader.FindAllStringSubmatchIndex(text, -1)
	if len(heads) == 0 {
		if !first && len(out) == 0 && trimmed != "" {
			add("owner", trimmed, "")
		}
		return out
	}
	for i, h := range heads {
		end := len(text)
		if i+1 < len(heads) {
			end = heads[i+1][0]
		}
		what, body := text[h[2]:h[3]], text[h[1]:end]
		from, msg, detail := inputBlock(what, body)
		if msg == "" {
			continue
		}
		if first && i == 0 && (from == "owner" || from == "github") {
			from = "task"
		}
		add(from, msg, detail)
	}
	return out
}

// inputBlock reads one Input block: what its header says it is, and the lines under it.
func inputBlock(what, body string) (from, text, detail string) {
	body = strings.Trim(body, "\n")
	switch {
	case what == "message from the owner (not on GitHub)":
		return "owner", strings.TrimSpace(body), ""
	case what == "note":
		return "athena", strings.TrimSpace(body), ""
	case what == "the Ticket was resumed by the message that follows":
		return "athena", "The Ticket was reopened", ""
	}
	lines := strings.Split(body, "\n")
	card := map[string]string{}
	n := 0
	for ; n < len(lines); n++ {
		m := cardLine.FindStringSubmatch(lines[n])
		if m == nil {
			break
		}
		card[m[1]] = m[2]
	}
	if _, ok := card["trigger_id"]; !ok {
		return "athena", strings.TrimSpace(body), ""
	}
	msg := cutLine.ReplaceAllString(strings.Join(lines[n:], "\n"), "")
	d, _ := json.Marshal(githubFrom{
		From: redact.String(card["from"]),
		Kind: strings.TrimSuffix(what, " from a person"),
		URL:  redact.String(card["url"]),
	})
	return "github", strings.TrimSpace(msg), string(d)
}
