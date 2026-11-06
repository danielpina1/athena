package render

import (
	"encoding/json"
	"regexp"
	"slices"
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
//	handoff the handoff note after the worker preamble of a session that continues another's work (internal/node
//	        handoffNote): its text unquoted, the Input headers it repeats read as what they are
//
// A message with no Input header is the owner's (old transcripts, and the e2e fixtures). The node's tests
// (TestEchoedInputsRenderAsTheirSenders) hold this to the real formats.

// githubFieldMaxRunes caps who wrote a card and its kind in the detail.
const githubFieldMaxRunes = 80

const (
	workerPreamble = "You are the worker of Athena Ticket "
	nudgeText      = "Your turn ended without a report."
	recoveryNote   = "Your previous process ended unexpectedly"
	resumeNote     = "Your session was paused while it waited"
	restartNote    = "Athena restarted while you were working"
	// handoffStart opens a handoff note (internal/node handoffOpening, handoffMovedOpening) on a line of its own.
	handoffStart = "\nYou are continuing this Ticket "
)

var (
	// inputHeader is an Input header with an id as the Engine makes them: 12 hex digits (newID), or a dashboard
	// command's <client id>:<ULID>.
	inputHeader = regexp.MustCompile(`(?m)^\[input ([0-9a-f]{12}|[A-Za-z0-9._-]{1,64}:[0-9A-HJKMNP-TV-Z]{26}): ([^\]\n]*)\]$`)
	anyHeader   = regexp.MustCompile(`(?m)^\[input [^\n]*\]\n?`)
	cardLine    = regexp.MustCompile(`^(trigger_id|acknowledged|from|at|thread|url|replies_to|context|routing note): (.*)$`)
	// quoted is a line the node quoted (internal/node quoteText): one space before what reads like a header.
	quoted  = regexp.MustCompile(`(?m)^ (\[|(?:trigger_id|acknowledged|from|at|thread|url|replies_to|context|routing note):)`)
	cutLine = regexp.MustCompile(`\n\[cut: [^\]\n]*\]\s*$`)
	// quotedHeader is an Input header a handoff note repeats (quoted), and quotedTrigger a trigger's id under one.
	quotedHeader  = regexp.MustCompile(`(?m)^ \[input [^\]\n]*?: ([^\]\n]*)\]$`)
	quotedTrigger = regexp.MustCompile(`(?m)^ (?:trigger_id|acknowledged): [^\n]*\n?`)
)

// cardKeys are a card's lines in the order inputCard writes them; each comes at most once.
var cardKeys = []string{"trigger_id", "acknowledged", "from", "at", "thread", "url", "replies_to", "context", "routing note"}

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
	case strings.HasPrefix(trimmed, restartNote):
		add("athena", "Athena restarted and resumed the session", "")
	case strings.HasPrefix(trimmed, resumeNote):
		add("athena", "Athena resumed the paused session", "")
	}
	first := strings.HasPrefix(trimmed, workerPreamble)
	heads := nodeHeaders(text)
	handoff := false
	if first {
		end := len(text)
		if len(heads) > 0 {
			end = heads[0][0]
		}
		if at := strings.Index(text[:end], handoffStart); at >= 0 {
			handoff = true
			add("handoff", handoffText(text[at+1:end]), "")
		}
	}
	if len(heads) == 0 {
		switch {
		case first || len(out) > 0 || trimmed == "":
		case anyHeader.MatchString(trimmed):
			// It looks like the node's but no header in it is: never the owner's words.
			add("athena", strings.TrimSpace(anyHeader.ReplaceAllString(trimmed, "")), "")
		default:
			add("owner", trimmed, "")
		}
		return out
	}
	for i, h := range heads {
		end := len(text)
		if i+1 < len(heads) {
			end = heads[i+1][0]
		}
		what, body := text[h[4]:h[5]], text[h[1]:end]
		from, msg, detail := inputBlock(what, body)
		if msg == "" {
			continue
		}
		if first && !handoff && i == 0 && (from == "owner" || from == "github") {
			from = "task"
		}
		add(from, msg, detail)
	}
	return out
}

// nodeHeaders finds the Input headers the node wrote: an Engine id, at the start of the message or after a blank line
// (every block the node writes ends in one), the first time the id appears. A line that only looks like one (in text
// from before the node quoted it) stays part of the block it is in.
func nodeHeaders(text string) [][]int {
	var out [][]int
	seen := map[string]bool{}
	for _, h := range inputHeader.FindAllStringSubmatchIndex(text, -1) {
		id := text[h[2]:h[3]]
		if seen[id] || (h[0] != 0 && (h[0] < 2 || text[h[0]-2:h[0]] != "\n\n")) {
			continue
		}
		seen[id] = true
		out = append(out, h)
	}
	return out
}

// handoffText is a handoff note as the session view shows it: the trigger ids of the Inputs it repeats go, their
// headers read as what they are ("message from the owner (not on GitHub):"), and the rest is unquoted.
func handoffText(note string) string {
	note = quotedTrigger.ReplaceAllString(note, "")
	note = quotedHeader.ReplaceAllString(note, "$1:")
	return strings.TrimSpace(unquote(note))
}

// unquote removes the space the node put before body lines that read like a header (internal/node quoteText).
func unquote(s string) string { return quoted.ReplaceAllString(s, "$1") }

// inputBlock reads one Input block: what its header says it is, and the lines under it.
func inputBlock(what, body string) (from, text, detail string) {
	body = strings.Trim(body, "\n")
	switch {
	case what == "message from the owner (not on GitHub)":
		return "owner", strings.TrimSpace(unquote(body)), ""
	case what == "note":
		return "athena", strings.TrimSpace(unquote(body)), ""
	case what == "the Ticket was resumed by the message that follows":
		return "athena", "The Ticket was reopened", ""
	}
	lines := strings.Split(body, "\n")
	card := map[string]string{}
	n, next := 0, 0
	for ; n < len(lines); n++ {
		m := cardLine.FindStringSubmatch(lines[n])
		if m == nil {
			break
		}
		// Once each, in order: a repeat or a key out of order is the body's.
		k := slices.Index(cardKeys, m[1])
		if k < next {
			break
		}
		card[m[1]], next = m[2], k+1
	}
	if _, ok := card["trigger_id"]; !ok {
		return "athena", strings.TrimSpace(unquote(body)), ""
	}
	msg := cutLine.ReplaceAllString(strings.Join(lines[n:], "\n"), "")
	d, _ := json.Marshal(githubFrom{
		From: truncate(redact.String(card["from"]), githubFieldMaxRunes),
		Kind: truncate(strings.TrimSuffix(what, " from a person"), githubFieldMaxRunes),
		URL:  redact.String(card["url"]),
	})
	return "github", strings.TrimSpace(unquote(msg)), string(d)
}
