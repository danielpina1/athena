package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/runtime/rt"
)

// Translator turns the messages of one `codex app-server` session into rt events. It keeps what a turn's end needs
// (its token usage, its non-retried errors, the latest quota windows), so there is one per session, and it is used
// from one goroutine.
type Translator struct {
	threadID string
	// usage is the last thread/tokenUsage/updated of the turn: Codex's `last` breakdown (this turn's), not `total`.
	usage *tokenBreakdown
	// errors are the messages of this turn's `error` notifications that Codex will not retry.
	errors []string
	// lastText is the turn's last agent message.
	lastText string
	// windows are the quota windows of the latest account/rateLimits/updated.
	windows []rt.Window
	// open is the items whose ToolUse was sent and whose ToolResult was not.
	open map[string]bool
}

// NewTranslator returns a Translator for a new session (the zero value works too).
func NewTranslator() *Translator { return &Translator{open: map[string]bool{}} }

type tokenBreakdown struct {
	InputTokens       int64 `json:"inputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	CachedInputTokens int64 `json:"cachedInputTokens"`
}

// item is a thread item, with the fields of the kinds Athena models.
type item struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// agentMessage
	Text string `json:"text"`
	// userMessage
	ClientID string `json:"clientId"`
	Content  []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	// commandExecution
	Command  string `json:"command"`
	Cwd      string `json:"cwd"`
	ExitCode *int   `json:"exitCode"`
	// commandExecution, fileChange, mcpToolCall
	Status string `json:"status"`
	// fileChange
	Changes []struct {
		Path string `json:"path"`
		Kind struct {
			Type string `json:"type"`
		} `json:"kind"`
	} `json:"changes"`
	// mcpToolCall
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Error     json.RawMessage `json:"error"`
	// webSearch
	Query string `json:"query"`
}

// Translate returns the events a message from the server stands for (none for one that only feeds the turn's end).
// A message it cannot read, or that it does not model, is an Unknown event, never an error.
func (t *Translator) Translate(m Message) []rt.Event {
	switch m.Method {
	case "thread/started":
		var p struct {
			Thread struct{ ID, Model string } `json:"thread"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			break
		}
		t.threadID = p.Thread.ID
		return []rt.Event{rt.Init{SessionID: p.Thread.ID, Model: p.Thread.Model}}
	case "item/started", "item/completed":
		var p struct {
			Item item `json:"item"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			break
		}
		return t.item(p.Item, m.Method == "item/completed")
	case "thread/tokenUsage/updated":
		var p struct {
			TokenUsage struct {
				Last tokenBreakdown `json:"last"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			break
		}
		t.usage = &p.TokenUsage.Last
		return nil
	case "account/rateLimits/updated":
		var p struct {
			RateLimits rateLimitSnapshot `json:"rateLimits"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			break
		}
		t.windows = p.RateLimits.windows()
		return []rt.Event{rt.QuotaUpdate{Windows: t.windows}}
	case "error":
		var p struct {
			Error     struct{ Message string } `json:"error"`
			WillRetry bool                     `json:"willRetry"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.WillRetry {
			break
		}
		t.errors = append(t.errors, p.Error.Message)
		return nil
	case "turn/completed":
		return t.turnEnd(m.Params)
	}
	return []rt.Event{rt.Unknown{Kind: m.Method}}
}

// item translates an item/started (done false) or item/completed. Each tool item gives a ToolUse at its start and a
// ToolResult at its end; one whose start was never seen gets its ToolUse first, so a ToolResult always has one.
func (t *Translator) item(it item, done bool) []rt.Event {
	switch it.Type {
	case "agentMessage":
		if !done {
			break
		}
		t.lastText = it.Text
		return []rt.Event{rt.AssistantText{Text: it.Text}}
	case "userMessage":
		if !done {
			break
		}
		var parts []string
		for _, c := range it.Content {
			if c.Type == "text" {
				parts = append(parts, c.Text)
			}
		}
		return []rt.Event{rt.UserEcho{Text: strings.Join(parts, "\n"), UUID: it.ClientID}}
	case "commandExecution":
		return t.tool(it, done, "exec_command", mustJSON(map[string]string{"command": it.Command, "cwd": it.Cwd}),
			(it.ExitCode != nil && *it.ExitCode != 0) || it.Status == "failed" || it.Status == "declined")
	case "fileChange":
		type change struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		}
		changes := make([]change, 0, len(it.Changes))
		for _, c := range it.Changes {
			changes = append(changes, change{Path: c.Path, Kind: c.Kind.Type})
		}
		return t.tool(it, done, "apply_patch", mustJSON(map[string]any{"changes": changes}), it.Status != "completed")
	case "mcpToolCall":
		input := it.Arguments
		if len(input) == 0 {
			input = json.RawMessage("null")
		}
		failed := (len(it.Error) > 0 && string(it.Error) != "null") || it.Status == "failed"
		return t.tool(it, done, "mcp__"+it.Server+"__"+it.Tool, input, failed)
	case "webSearch":
		return t.tool(it, done, "web_search", mustJSON(map[string]string{"query": it.Query}), false)
	}
	return []rt.Event{rt.Unknown{Kind: it.Type}}
}

func (t *Translator) tool(it item, done bool, name string, input json.RawMessage, isError bool) []rt.Event {
	var evs []rt.Event
	if t.open == nil {
		t.open = map[string]bool{}
	}
	if !t.open[it.ID] {
		t.open[it.ID] = true
		evs = append(evs, rt.ToolUse{ID: it.ID, Name: name, Input: input})
	}
	if done {
		delete(t.open, it.ID)
		evs = append(evs, rt.ToolResult{ID: it.ID, IsError: isError})
	}
	return evs
}

func (t *Translator) turnEnd(params json.RawMessage) []rt.Event {
	var p struct {
		ThreadID string `json:"threadId"`
		Turn     struct {
			Status     string `json:"status"`
			DurationMs int64  `json:"durationMs"`
			Error      *struct {
				Message        string          `json:"message"`
				CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
			} `json:"error"`
		} `json:"turn"`
	}
	if json.Unmarshal(params, &p) != nil {
		return []rt.Event{rt.Unknown{Kind: "turn/completed"}}
	}
	end := rt.TurnEnd{
		SessionID: p.ThreadID,
		OK:        p.Turn.Status == "completed",
		Text:      t.lastText,
		Errors:    t.errors,
		NumTurns:  1,
		Duration:  time.Duration(p.Turn.DurationMs) * time.Millisecond,
	}
	if end.SessionID == "" {
		end.SessionID = t.threadID
	}
	if t.usage != nil {
		end.InputTokens, end.OutputTokens, end.CacheReadTokens = t.usage.InputTokens, t.usage.OutputTokens, t.usage.CachedInputTokens
	}
	code := ""
	if e := p.Turn.Error; e != nil {
		end.Text = e.Message
		code = errorCode(e.CodexErrorInfo)
		switch code {
		case "usageLimitExceeded":
			end.Limit = &rt.LimitHit{Runtime: "codex", ResetsAt: t.limitReset(), Message: e.Message}
		case "rateLimitExceeded":
			end.Limit = &rt.LimitHit{Runtime: "codex", Transient: true, Message: e.Message}
		}
	}
	switch {
	case p.Turn.Status == "interrupted":
		end.Subtype = "interrupted"
	case !end.OK:
		end.Subtype = code
		if end.Subtype == "" {
			end.Subtype = p.Turn.Status
		}
	}
	t.usage, t.errors, t.lastText = nil, nil, ""
	t.open = map[string]bool{}
	return []rt.Event{end}
}

// limitReset is when a used-up limit ends: the latest reset among the windows that are used up (a window reset does
// not help while another is still used up), nil when none is or none says when.
func (t *Translator) limitReset() *time.Time {
	var at *time.Time
	for _, w := range t.windows {
		if w.Used >= 1 && w.ResetsAt != nil && (at == nil || w.ResetsAt.After(*at)) {
			at = w.ResetsAt
		}
	}
	return at
}

// errorCode is the name of a codexErrorInfo: the string itself, or the one key of the object variants.
func errorCode(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o map[string]json.RawMessage
	if json.Unmarshal(raw, &o) == nil {
		for k := range o {
			return k
		}
	}
	return ""
}

type rateLimitWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins *int64  `json:"windowDurationMins"`
	ResetsAt           *int64  `json:"resetsAt"`
}

type rateLimitSnapshot struct {
	Primary              *rateLimitWindow `json:"primary"`
	Secondary            *rateLimitWindow `json:"secondary"`
	RateLimitReachedType *string          `json:"rateLimitReachedType"`
}

func (s rateLimitSnapshot) windows() []rt.Window {
	var ws []rt.Window
	for i, w := range []*rateLimitWindow{s.Primary, s.Secondary} {
		if w == nil {
			continue
		}
		name := [...]string{"primary", "secondary"}[i] // a window that does not say how long it is
		if w.WindowDurationMins != nil {
			name = windowName(*w.WindowDurationMins)
		}
		win := rt.Window{
			Name:    name,
			Used:    min(max(w.UsedPercent/100, 0), 1),
			Limited: s.RateLimitReachedType != nil && *s.RateLimitReachedType != "",
		}
		if w.ResetsAt != nil {
			at := time.Unix(*w.ResetsAt, 0).UTC()
			win.ResetsAt = &at
		}
		ws = append(ws, win)
	}
	return ws
}

// windowName names a window of mins minutes: 300 is "5h", 10080 "7d", any other "<n>m".
func windowName(mins int64) string {
	switch mins {
	case 300:
		return "5h"
	case 10080:
		return "7d"
	}
	return fmt.Sprintf("%dm", mins)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // the values are strings and slices of them
	}
	return b
}
