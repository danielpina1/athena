// Package claude drives a headless Claude Code session over stream-json
// stdin/stdout and generates the per-session settings and MCP config files.
package claude

import (
	"encoding/json"
	"strings"
)

// Event is one thing a Claude Code session reported on stdout.
type Event interface{ isEvent() }

// Init is the session's first system message.
type Init struct {
	SessionID string
	Model     string
	CWD       string
	Tools     []string
}

// AssistantText is a block of text from the assistant.
type AssistantText struct{ Text string }

// ToolUse is the assistant asking to run a tool.
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult reports the outcome of a ToolUse. Only the outcome is kept; the
// raw line is in the transcript.
type ToolResult struct {
	ToolUseID string
	IsError   bool
}

// UserEcho is a user message the session took into its conversation, echoed back with --replay-user-messages
// (checked against Claude Code 2.1.280: a message sent mid-turn is echoed only when a turn takes it in, not when it is
// read). UUID is the uuid the message was sent with (Session.SendMessage), empty when the echo carries none; Text is
// its content, text blocks joined by newlines.
type UserEcho struct {
	Text string
	UUID string
}

// Result ends a turn. A session emits one Result per turn (messages that
// arrive mid-turn are folded into the running turn).
type Result struct {
	SessionID       string
	Subtype         string
	IsError         bool
	CostUSD         float64
	InputTokens     int
	OutputTokens    int
	CacheReadTokens int
	NumTurns        int
	DurationMS      int
	// Text is the final assistant message of the turn ("result" in the CLI).
	Text string
	// Errors says why an error result failed ("errors" in the CLI, e.g. "Reached maximum number of turns (1)").
	Errors []string
}

// Unknown is any line the driver does not model. Raw is the complete line.
type Unknown struct {
	Type string
	Raw  json.RawMessage
}

func (Init) isEvent()          {}
func (AssistantText) isEvent() {}
func (ToolUse) isEvent()       {}
func (ToolResult) isEvent()    {}
func (UserEcho) isEvent()      {}
func (Result) isEvent()        {}
func (Unknown) isEvent()       {}

type envelope struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	IsReplay  bool   `json:"isReplay"`
	UUID      string `json:"uuid"`
	Message   *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

// ParseEvent parses one stdout line into a single Event: the first event
// ParseEvents yields. Lines of unmodelled types are Unknown, never an error;
// only text that is not a JSON object is.
func ParseEvent(line []byte) (Event, error) {
	evs, err := ParseEvents(line)
	if err != nil {
		return nil, err
	}
	return evs[0], nil
}

// ParseEvents parses one stdout line. An assistant or user message can carry
// several content blocks, each of which becomes an event; every other line is
// exactly one event. The result is never empty when err is nil.
func ParseEvents(line []byte) ([]Event, error) {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, err
	}
	unknown := func() []Event {
		return []Event{Unknown{Type: env.Type, Raw: append(json.RawMessage(nil), line...)}}
	}

	switch env.Type {
	case "system":
		if env.Subtype != "init" {
			return unknown(), nil
		}
		var in struct {
			SessionID string   `json:"session_id"`
			Model     string   `json:"model"`
			CWD       string   `json:"cwd"`
			Tools     []string `json:"tools"`
		}
		if err := json.Unmarshal(line, &in); err != nil {
			return unknown(), nil
		}
		return []Event{Init{SessionID: in.SessionID, Model: in.Model, CWD: in.CWD, Tools: in.Tools}}, nil

	case "assistant", "user":
		if env.Type == "user" && env.IsReplay && env.Message != nil && len(env.Message.Content) > 0 {
			if env.Message.Content[0] == '"' {
				var text string
				if json.Unmarshal(env.Message.Content, &text) == nil {
					return []Event{UserEcho{Text: text, UUID: env.UUID}}, nil
				}
			} else if env.UUID != "" {
				// Content in blocks: an echo all the same, known by its uuid.
				var texts []string
				for _, b := range contentBlocks(env) {
					if b.Type == "text" {
						texts = append(texts, b.Text)
					}
				}
				return []Event{UserEcho{Text: strings.Join(texts, "\n"), UUID: env.UUID}}, nil
			}
		}
		blocks := contentBlocks(env)
		var evs []Event
		for _, b := range blocks {
			switch {
			case env.Type == "assistant" && b.Type == "text":
				evs = append(evs, AssistantText{Text: b.Text})
			case env.Type == "assistant" && b.Type == "tool_use":
				evs = append(evs, ToolUse{ID: b.ID, Name: b.Name, Input: b.Input})
			case env.Type == "user" && b.Type == "tool_result":
				evs = append(evs, ToolResult{ToolUseID: b.ToolUseID, IsError: b.IsError})
			}
		}
		if len(evs) == 0 {
			return unknown(), nil
		}
		return evs, nil

	case "result":
		var r struct {
			SessionID  string   `json:"session_id"`
			Subtype    string   `json:"subtype"`
			IsError    bool     `json:"is_error"`
			CostUSD    float64  `json:"total_cost_usd"`
			NumTurns   int      `json:"num_turns"`
			DurationMS int      `json:"duration_ms"`
			Result     string   `json:"result"`
			Errors     []string `json:"errors"`
			Usage      struct {
				InputTokens     int `json:"input_tokens"`
				OutputTokens    int `json:"output_tokens"`
				CacheReadTokens int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(line, &r); err != nil {
			return unknown(), nil
		}
		return []Event{Result{
			SessionID:       r.SessionID,
			Subtype:         r.Subtype,
			IsError:         r.IsError,
			CostUSD:         r.CostUSD,
			InputTokens:     r.Usage.InputTokens,
			OutputTokens:    r.Usage.OutputTokens,
			CacheReadTokens: r.Usage.CacheReadTokens,
			NumTurns:        r.NumTurns,
			DurationMS:      r.DurationMS,
			Text:            r.Result,
			Errors:          r.Errors,
		}}, nil
	}
	return unknown(), nil
}

// contentBlocks returns the typed content blocks of a message. A plain string
// content (a user prompt echo) has none.
func contentBlocks(env envelope) []contentBlock {
	if env.Message == nil || len(env.Message.Content) == 0 || env.Message.Content[0] != '[' {
		return nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(env.Message.Content, &blocks); err != nil {
		return nil
	}
	return blocks
}
