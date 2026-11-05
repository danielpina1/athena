package render

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/redact"
)

const (
	// SayMaxBytes caps the text of a say or thinking item, ResultMaxBytes that of a tool result, and
	// toolInputMaxRunes the shown input of a tool call.
	SayMaxBytes       = 16 << 10
	ResultMaxBytes    = 4 << 10
	toolInputMaxRunes = 400
	// DetailValueMaxRunes clips each string of a tool call's detail, DetailMaxBytes the whole detail.
	DetailValueMaxRunes = 2000
	DetailMaxBytes      = 8 << 10
)

// Items renders one transcript line (without its newline), which starts at offset in the transcript file, into the
// StreamItems a dashboard shows: one per content block. Every item carries the line's offset. Text is redacted before
// it is cut, so a cut never leaves part of a secret. A nil line stands for one too big to keep.
//
// The session's bookkeeping renders to no items: system lines other than init (task progress, thinking tokens,
// hooks, background tasks, ...), rate limit events, tool progress heartbeats, command lifecycle lines and thinking
// blocks without text (thinking is redacted upstream). A message the session was sent (echoed back) renders as one
// "user" item per Input in it (inputItems).
// Three formats share a transcript, told apart by the line's top-level keys: Claude stream-json (the rest of this
// comment), a Codex envelope {"at","codex"} (codexItems) and an Athena line {"athena"} (a session marker, which renders to
// one "runtime" item, or a note of the session). A line of an unknown type renders to one "other" item that names it.
func Items(line []byte, offset int64) []*athenav1.StreamItem {
	other := func(text string) []*athenav1.StreamItem {
		return []*athenav1.StreamItem{{Offset: offset, Kind: "other", Text: text}}
	}
	if line == nil {
		return other("line too large to show")
	}
	var l struct {
		Type      string  `json:"type"`
		Subtype   string  `json:"subtype"`
		Timestamp string  `json:"timestamp"`
		SessionID string  `json:"session_id"`
		Model     string  `json:"model"`
		IsError   bool    `json:"is_error"`
		NumTurns  int     `json:"num_turns"`
		CostUSD   float64 `json:"total_cost_usd"`
		Duration  int64   `json:"duration_ms"`
		IsReplay  bool    `json:"isReplay"`
		// A line of the other formats, told apart by its top-level keys alone (codex.go, marker.go).
		At      string          `json:"at"`
		Codex   json.RawMessage `json:"codex"`
		Athena  json.RawMessage `json:"athena"`
		Result  string          `json:"result"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil {
		return other("unreadable transcript line")
	}
	var at *timestamppb.Timestamp
	if t, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
		at = timestamppb.New(t)
	}
	if len(l.Codex) > 0 || len(l.Athena) > 0 {
		if t, err := time.Parse(time.RFC3339Nano, l.At); err == nil {
			at = timestamppb.New(t)
		}
		items := codexItems(l.Codex, offset)
		if len(l.Codex) == 0 {
			items = athenaItems(line)
		}
		for _, it := range items {
			it.Offset, it.At = offset, at
		}
		return items
	}
	var out []*athenav1.StreamItem
	add := func(it *athenav1.StreamItem) {
		it.Offset, it.At = offset, at
		out = append(out, it)
	}

	// silent is set when the line rendered to nothing on purpose.
	silent := false
	switch l.Type {
	case "system":
		if l.Subtype == "init" {
			add(&athenav1.StreamItem{Kind: "init", Text: redact.String(strings.TrimSpace(fmt.Sprintf("session %s model %s", l.SessionID, l.Model)))})
		}
		silent = true
	case "rate_limit_event", "tool_progress", "command_lifecycle":
		silent = true
	case "result":
		s := fmt.Sprintf("%s turns=%d cost=$%.4f duration_ms=%d", l.Subtype, l.NumTurns, l.CostUSD, max(l.Duration, 0))
		if l.Result != "" {
			s += "\n" + l.Result
		}
		add(&athenav1.StreamItem{Kind: "result", Text: redactCut(s, SayMaxBytes), IsError: l.IsError})
	case "assistant", "user":
		if l.Type == "user" {
			if text, ok := userText(l.Message.Content, l.IsReplay); ok {
				for _, it := range inputItems(text) {
					add(it)
				}
				silent = true
				break
			}
		}
		var blocks []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(l.Message.Content, &blocks)
		for _, b := range blocks {
			switch {
			case l.Type == "assistant" && b.Type == "text":
				add(&athenav1.StreamItem{Kind: "say", Text: redactCut(b.Text, SayMaxBytes)})
			case l.Type == "assistant" && b.Type == "thinking":
				if strings.TrimSpace(b.Thinking) == "" {
					silent = true
					break
				}
				add(&athenav1.StreamItem{Kind: "thinking", Text: redactCut(b.Thinking, SayMaxBytes)})
			case l.Type == "assistant" && b.Type == "tool_use":
				add(&athenav1.StreamItem{Kind: "tool_call", Tool: b.Name, ToolUseId: b.ID, Text: summarizeInput(b.Input, toolInputMaxRunes),
					Detail: toolDetail(b.Input)})
			case l.Type == "user" && b.Type == "tool_result":
				add(&athenav1.StreamItem{Kind: "tool_result", ToolUseId: b.ToolUseID, IsError: b.IsError,
					Text: redactCut(resultText(b.Content), ResultMaxBytes)})
			}
		}
	}
	if len(out) == 0 && !silent {
		label := l.Type
		if l.Subtype != "" {
			label += "/" + l.Subtype
		}
		if label == "" {
			label = "untyped line"
		}
		add(&athenav1.StreamItem{Kind: "other", Text: redact.String(oneLine(label))})
	}
	return out
}

// userText is the text of a message the session was sent: its string content, or (for an echo) its text blocks.
func userText(raw json.RawMessage, echo bool) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, s != ""
	}
	if !echo {
		return "", false
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n"), len(parts) > 0
}

// resultText is the text of a tool result's content: a string, or the text blocks of an array joined by newlines.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// cut limits s to n bytes, at a rune boundary, ending in an ellipsis when it cut.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const ell = "…"
	end := n - len(ell)
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + ell
}

// redactCut masks the secrets in all of s, then cuts it to n bytes: a cut never leaves part of a secret. The cost is
// bounded by the line cap of the stream (256 KiB).
func redactCut(s string, n int) string { return cut(redact.String(s), n) }
