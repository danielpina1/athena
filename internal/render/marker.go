package render

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// Marker says in a transcript which runtime the lines after it come from: the node writes one at the start of every
// session (start, resume, failover or review), so a Ticket's transcript that moved between runtimes shows where.
//
//	{"athena":"session","at":…,"runtime":"claude|codex","model":…,"assignment":…,"role":"implement|review","reason":"start|resume|failover|review"}
type Marker struct {
	At         time.Time
	Runtime    string
	Model      string
	Assignment string
	Role       string
	Reason     string
}

// MarkerLine is the transcript line for m, without its newline.
func MarkerLine(m Marker) []byte {
	b, _ := json.Marshal(struct {
		Athena     string `json:"athena"`
		At         string `json:"at"`
		Runtime    string `json:"runtime"`
		Model      string `json:"model"`
		Assignment string `json:"assignment"`
		Role       string `json:"role"`
		Reason     string `json:"reason"`
	}{"session", m.At.UTC().Format(time.RFC3339Nano), m.Runtime, m.Model, m.Assignment, m.Role, m.Reason})
	return b
}

// athenaLine is a line with a top-level "athena" key: a session marker (a string, "session") or a note of the session
// itself (an object; today {"declined","id","reason"}). Only the top-level key decides, so text in a Codex or Claude
// line (nested) can never forge either.
type athenaLine struct {
	Athena json.RawMessage `json:"athena"`
	At     string          `json:"at"`

	Runtime string `json:"runtime"`
	Model   string `json:"model"`
	Role    string `json:"role"`
	Reason  string `json:"reason"`
}

// athenaItems renders an Athena line to its items (no offset or time).
func athenaItems(line []byte) []*athenav1.StreamItem {
	var l athenaLine
	if json.Unmarshal(line, &l) != nil {
		return []*athenav1.StreamItem{{Kind: "other", Text: "unreadable transcript line", IsError: true}}
	}
	var kind string
	if json.Unmarshal(l.Athena, &kind) == nil {
		if kind == "session" {
			text := l.Reason
			if l.Model != "" {
				text = l.Model + " · " + l.Reason
			}
			d, _ := json.Marshal(map[string]string{"model": l.Model, "reason": l.Reason, "role": l.Role})
			return []*athenav1.StreamItem{{Kind: "runtime", Tool: l.Runtime, Text: Clip(text, toolInputMaxRunes), Detail: toolDetail(d)}}
		}
		return []*athenav1.StreamItem{{Kind: "other", Text: Clip("athena/"+kind, toolInputMaxRunes)}}
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(l.Athena, &obj) == nil {
		if raw, ok := obj["declined"]; ok {
			var method, reason string
			_ = json.Unmarshal(raw, &method)
			_ = json.Unmarshal(obj["reason"], &reason)
			return []*athenav1.StreamItem{{Kind: "other", IsError: true, Text: Clip("Athena declined "+method+": "+reason, toolInputMaxRunes)}}
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return []*athenav1.StreamItem{{Kind: "other", Text: Clip("athena/"+strings.Join(keys, ","), toolInputMaxRunes)}}
	}
	return []*athenav1.StreamItem{{Kind: "other", Text: "athena line"}}
}
