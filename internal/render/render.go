package render

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/redact"
	"github.com/danielpina1/athena/internal/runtime/claude"
)

// LogLine is one rendered transcript event: when it happened, its kind (init, say, tool, input, result or other) and a short
// text. It never carries a raw transcript line.
type LogLine struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

const (
	sayMaxRunes     = 160
	summaryMaxRunes = 80
	redacted        = "***"
)

// primaryKeys are tool input keys whose value alone says what a tool call does, in order of preference.
var primaryKeys = []string{"command", "file_path", "notebook_path", "path", "pattern", "url", "query", "description", "prompt"}

// Renderer turns transcript lines into LogLines. Stream-json lines carry a timestamp only on messages; a line without
// one takes the last timestamp seen, or the fallback before any.
type Renderer struct {
	last time.Time
}

// New returns a Renderer that dates lines before the first timestamp with fallback.
func New(fallback time.Time) *Renderer { return &Renderer{last: fallback} }

// Render returns the LogLines of one transcript line: one per event (an echoed message: one per Input in it).
func (r *Renderer) Render(line []byte) []LogLine {
	var head struct {
		Type      string          `json:"type"`
		Subtype   string          `json:"subtype"`
		Timestamp string          `json:"timestamp"`
		At        string          `json:"at"`
		Codex     json.RawMessage `json:"codex"`
		Athena    json.RawMessage `json:"athena"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return []LogLine{{At: r.last, Kind: "other", Text: "unreadable transcript line"}}
	}
	if head.Timestamp != "" {
		if at, err := time.Parse(time.RFC3339Nano, head.Timestamp); err == nil {
			r.last = at
		}
	}
	if len(head.Codex) > 0 || len(head.Athena) > 0 {
		return r.renderOther(line, head.At, len(head.Codex) > 0, head.Codex)
	}
	evs, err := claude.ParseEvents(line)
	if err != nil {
		return []LogLine{{At: r.last, Kind: "other", Text: "unreadable transcript line"}}
	}
	out := make([]LogLine, 0, len(evs))
	for _, ev := range evs {
		if e, ok := ev.(claude.UserEcho); ok {
			// A message the session took in: one line per Input in it, who sent it and its first words.
			for _, it := range inputItems(e.Text) {
				out = append(out, LogLine{At: r.last, Kind: "input", Text: it.Tool + ": " + Clip(it.Text, sayMaxRunes)})
			}
			continue
		}
		subtype := head.Subtype
		if subtype == "" {
			subtype = blockTypes(head.Message.Content)
		}
		kind, text := describe(ev, head.Type, subtype)
		// Values were masked before they were cut; a last pass catches anything a cut put back together.
		out = append(out, LogLine{At: r.last, Kind: kind, Text: redact.String(text)})
	}
	return out
}

// renderOther renders a Codex envelope or an Athena line from the items the dashboard shows, in the shorter words of the
// log: a tool call is its tool and what it did, a tool result only whether it worked.
func (r *Renderer) renderOther(line []byte, at string, isCodex bool, codexLine json.RawMessage) []LogLine {
	if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
		r.last = t
	}
	var items []*athenav1.StreamItem
	if isCodex {
		items = codexItems(codexLine)
	} else {
		var m struct {
			Athena string `json:"athena"`
			At     string `json:"at"`
		}
		items = athenaItems(line)
		if json.Unmarshal(line, &m) == nil && m.Athena == "session" && len(items) == 1 {
			// a marker: "<runtime> session: <model> · <reason>"
			items[0].Kind, items[0].Text = "other", items[0].Tool+" session: "+items[0].Text
		}
	}
	out := make([]LogLine, 0, len(items))
	for _, it := range items {
		var kind, text string
		switch it.Kind {
		case "init":
			kind, text = "init", it.Text
		case "say":
			kind, text = "say", Clip(it.Text, sayMaxRunes)
		case "thinking":
			kind, text = "other", "thinking: "+Clip(it.Text, sayMaxRunes)
		case "tool_call":
			kind, text = "tool", strings.TrimSpace(it.Tool+" "+Clip(it.Text, summaryMaxRunes))
		case "tool_result":
			kind, text = "other", "tool result: ok"
			if it.IsError {
				text = "tool result: error"
			}
		case "user":
			kind, text = "input", it.Tool+": "+Clip(it.Text, sayMaxRunes)
		case "result":
			kind, text = "result", Clip(it.Text, sayMaxRunes)
			if it.IsError {
				text += " (error)"
			}
		default:
			kind, text = "other", Clip(it.Text, sayMaxRunes)
		}
		out = append(out, LogLine{At: r.last, Kind: kind, Text: redact.String(text)})
	}
	return out
}

// blockTypes names the content block types of a message, such as "thinking", joined by commas.
func blockTypes(content json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var types []string
	for _, b := range blocks {
		if b.Type != "" && !slices.Contains(types, b.Type) {
			types = append(types, b.Type)
		}
	}
	return strings.Join(types, ",")
}

func describe(ev claude.Event, typ, subtype string) (kind, text string) {
	switch e := ev.(type) {
	case claude.Init:
		return "init", strings.TrimSpace(fmt.Sprintf("session %s model %s", e.SessionID, e.Model))
	case claude.AssistantText:
		return "say", Clip(e.Text, sayMaxRunes)
	case claude.ToolUse:
		if s := summarizeInput(e.Input, summaryMaxRunes); s != "" {
			return "tool", e.Name + " " + s
		}
		return "tool", e.Name
	case claude.ToolResult:
		if e.IsError {
			return "other", "tool result: error"
		}
		return "other", "tool result: ok"
	case claude.Result:
		s := fmt.Sprintf("%s turns=%d cost=$%.4f", e.Subtype, e.NumTurns, e.CostUSD)
		if e.IsError {
			s += " (error)"
		}
		return "result", s
	}
	if subtype != "" {
		return "other", oneLine(typ + "/" + subtype)
	}
	if typ == "" {
		return "other", "untyped line"
	}
	return "other", oneLine(typ)
}

// summarizeInput says in at most max runes what a tool input is: the value of its primary key, or its
// scalar fields as key=value. Values of env-like keys are never shown.
func summarizeInput(raw json.RawMessage, max int) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil || len(in) == 0 {
		return ""
	}
	for _, k := range primaryKeys {
		if v, ok := in[k].(string); ok && v != "" && !sensitiveKey(k) {
			return Clip(v, max)
		}
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		switch {
		case sensitiveKey(k):
			parts = append(parts, k+"="+redacted)
		default:
			switch v := in[k].(type) {
			case string:
				parts = append(parts, k+"="+redact.String(oneLine(v)))
			case float64, bool:
				parts = append(parts, fmt.Sprintf("%s=%v", k, v))
			case nil:
				parts = append(parts, k+"=null")
			default:
				parts = append(parts, k+"=…")
			}
		}
	}
	return truncate(strings.Join(parts, " "), max)
}

// sensitiveKey reports whether a key is env-like: it holds environment variables ("env", "environment") or its name
// looks secret (redact.SecretName: token, password, key, ... on word boundaries).
func sensitiveKey(k string) bool {
	switch strings.ToLower(k) {
	case "env", "environ", "environment":
		return true
	}
	return redact.SecretName(k)
}

// Clip makes s one line, masks the secrets in it and cuts it to n runes.
func Clip(s string, n int) string { return truncate(redact.String(oneLine(s)), n) }

// oneLine collapses all whitespace runs, newlines included, into single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// truncate cuts s to at most n runes, the last one an ellipsis when cut.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
