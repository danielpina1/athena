package server

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/runtime/claude"
)

// LogLine is one rendered transcript event: when it happened, its kind (init, say, tool, result or other) and a short
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

// renderer turns transcript lines into LogLines. Stream-json lines carry a timestamp only on messages; a line without
// one takes the last timestamp seen, or the fallback before any.
type renderer struct {
	last time.Time
}

func newRenderer(fallback time.Time) *renderer { return &renderer{last: fallback} }

// render returns the LogLines of one transcript line: one per event, at least one.
func (r *renderer) render(line []byte) []LogLine {
	var head struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		Timestamp string `json:"timestamp"`
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
	evs, err := claude.ParseEvents(line)
	if err != nil {
		return []LogLine{{At: r.last, Kind: "other", Text: "unreadable transcript line"}}
	}
	out := make([]LogLine, 0, len(evs))
	for _, ev := range evs {
		subtype := head.Subtype
		if subtype == "" {
			subtype = blockTypes(head.Message.Content)
		}
		kind, text := describe(ev, head.Type, subtype)
		out = append(out, LogLine{At: r.last, Kind: kind, Text: text})
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
		return "say", truncate(oneLine(e.Text), sayMaxRunes)
	case claude.ToolUse:
		if s := summarizeInput(e.Input); s != "" {
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

// summarizeInput says in at most summaryMaxRunes runes what a tool input is: the value of its primary key, or its
// scalar fields as key=value. Values of env-like keys are never shown.
func summarizeInput(raw json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil || len(in) == 0 {
		return ""
	}
	for _, k := range primaryKeys {
		if v, ok := in[k].(string); ok && v != "" && !sensitiveKey(k) {
			return truncate(oneLine(v), summaryMaxRunes)
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
				parts = append(parts, k+"="+oneLine(v))
			case float64, bool:
				parts = append(parts, fmt.Sprintf("%s=%v", k, v))
			case nil:
				parts = append(parts, k+"=null")
			default:
				parts = append(parts, k+"=…")
			}
		}
	}
	return truncate(strings.Join(parts, " "), summaryMaxRunes)
}

// sensitiveWords mark a key whose value may be a secret.
var sensitiveWords = []string{"env", "token", "secret", "password", "passwd", "credential", "auth", "cookie", "key", "private"}

// sensitiveKey reports whether a key is env-like: it names environment variables or a secret.
func sensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	return slices.ContainsFunc(sensitiveWords, func(w string) bool { return strings.Contains(lk, w) })
}

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
