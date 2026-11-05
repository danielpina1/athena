package render

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/redact"
)

// A Codex session writes each notification or server request of its app-server as
// {"at":"<RFC3339Nano>","codex":<the line>} (internal/runtime/codex). codexItems renders the line to StreamItems the
// way Claude's lines render, so the dashboard shows one transcript whatever ran it. Tool names are Athena's:
// exec_command, apply_patch, web_search, update_plan and mcp__<server>__<tool>. Details follow the Claude tools':
// a tool_call's detail is its input; an exec_command tool_result's detail is {"exitCode":n}.

var (
	ansiCSI   = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	ansiOSC   = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	ansiOther = regexp.MustCompile(`\x1b[@-Z\\-_]`)
	listMark  = regexp.MustCompile(`^\s*(?:[-*]|\d+[.)])\s+`)
)

// stripANSI removes terminal escape sequences from s.
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	s = ansiOSC.ReplaceAllString(s, "")
	s = ansiCSI.ReplaceAllString(s, "")
	return ansiOther.ReplaceAllString(s, "")
}

// approvalRequests are the server requests that ask for an approval: the dashboard shows the Approval itself. The
// requests Athena refuses (item/tool/requestUserInput, mcpServer/elicitation/request, item/tool/call, ...) render as
// "other" items naming the method, which explains the decline that follows.
var approvalRequests = map[string]bool{
	"item/commandExecution/requestApproval": true, "item/fileChange/requestApproval": true,
	"item/permissions/requestApproval": true, "applyPatchApproval": true, "execCommandApproval": true,
}

type codexItem struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Summary          []string        `json:"summary"`
	Content          json.RawMessage `json:"content"`
	Command          string          `json:"command"`
	Cwd              string          `json:"cwd"`
	Status           string          `json:"status"`
	AggregatedOutput *string         `json:"aggregatedOutput"`
	ExitCode         *int            `json:"exitCode"`
	Changes          []struct {
		Path string          `json:"path"`
		Kind json.RawMessage `json:"kind"`
		Diff string          `json:"diff"`
	} `json:"changes"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    json.RawMessage `json:"result"`
	Error     json.RawMessage `json:"error"`
	Query     string          `json:"query"`
}

// codexItems renders one Codex line (the value of the envelope's "codex" key); the caller adds offset and time.
func codexItems(raw json.RawMessage, offset int64) []*athenav1.StreamItem {
	var m struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(raw, &m) != nil || m.Method == "" {
		return []*athenav1.StreamItem{{Kind: "other", Text: "codex line without a method"}}
	}
	switch {
	case m.Method == "turn/started", m.Method == "thread/tokenUsage/updated", m.Method == "account/rateLimits/updated",
		m.Method == "serverRequest/resolved", m.Method == "thread/status/changed", approvalRequests[m.Method], strings.HasPrefix(m.Method, "hook/"),
		strings.HasPrefix(m.Method, "item/") && (strings.HasSuffix(m.Method, "Delta") || strings.HasSuffix(m.Method, "/delta") || strings.HasSuffix(m.Method, "/summaryPartAdded")):
		return nil
	}
	switch m.Method {
	case "thread/started":
		var p struct {
			Thread struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			} `json:"thread"`
		}
		_ = json.Unmarshal(m.Params, &p)
		return []*athenav1.StreamItem{{Kind: "init", Text: redact.String(strings.TrimSpace(fmt.Sprintf("session %s model %s", p.Thread.ID, p.Thread.Model)))}}
	case "item/started", "item/completed":
		var p struct {
			Item codexItem `json:"item"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			return []*athenav1.StreamItem{{Kind: "other", Text: Clip(m.Method, toolInputMaxRunes)}}
		}
		return codexThreadItem(p.Item, m.Method == "item/completed")
	case "turn/plan/updated":
		var p struct {
			Plan []map[string]any `json:"plan"`
		}
		_ = json.Unmarshal(m.Params, &p)
		steps := make([]map[string]any, 0, len(p.Plan))
		for _, s := range p.Plan {
			steps = append(steps, map[string]any{"step": s["step"], "status": planStatus(s["status"])})
		}
		return planItems(fmt.Sprintf("plan-%d", offset), steps)
	case "turn/completed":
		var p struct {
			Turn struct {
				Status   string `json:"status"`
				Duration *int64 `json:"durationMs"`
				Error    *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(m.Params, &p)
		s := p.Turn.Status
		if p.Turn.Duration != nil {
			s += fmt.Sprintf(" duration_ms=%d", max(*p.Turn.Duration, 0))
		}
		if p.Turn.Error != nil && p.Turn.Error.Message != "" {
			s += "\n" + p.Turn.Error.Message
		}
		return []*athenav1.StreamItem{{Kind: "result", Text: redactCut(strings.TrimSpace(s), SayMaxBytes), IsError: p.Turn.Status != "completed"}}
	case "error":
		var p struct {
			WillRetry bool `json:"willRetry"`
			Error     struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.WillRetry {
			return nil
		}
		return []*athenav1.StreamItem{{Kind: "other", IsError: true, Text: redactCut("Codex error: "+p.Error.Message, SayMaxBytes)}}
	}
	return []*athenav1.StreamItem{{Kind: "other", Text: Clip(m.Method, toolInputMaxRunes)}}
}

func codexThreadItem(it codexItem, done bool) []*athenav1.StreamItem {
	one := func(x *athenav1.StreamItem) []*athenav1.StreamItem { return []*athenav1.StreamItem{x} }
	failed := it.Status == "failed" || it.Status == "declined"
	switch it.Type {
	case "agentMessage":
		if done && strings.TrimSpace(it.Text) != "" {
			return one(&athenav1.StreamItem{Kind: "say", Text: redactCut(it.Text, SayMaxBytes)})
		}
	case "reasoning":
		if text := strings.TrimSpace(strings.Join(it.Summary, "\n")); done && text != "" {
			return one(&athenav1.StreamItem{Kind: "thinking", Text: redactCut(text, SayMaxBytes)})
		}
	case "userMessage":
		if !done {
			return nil
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(it.Content, &parts)
		var texts []string
		for _, p := range parts {
			if p.Type == "text" {
				texts = append(texts, p.Text)
			}
		}
		if len(texts) == 0 {
			return nil
		}
		return inputItems(strings.Join(texts, "\n"))
	case "commandExecution":
		if !done {
			return one(&athenav1.StreamItem{Kind: "tool_call", Tool: "exec_command", ToolUseId: it.ID, Text: Clip(it.Command, toolInputMaxRunes),
				Detail: detailJSON(map[string]any{"command": it.Command, "cwd": it.Cwd})})
		}
		out := ""
		if it.AggregatedOutput != nil {
			out = *it.AggregatedOutput
		}
		r := &athenav1.StreamItem{Kind: "tool_result", ToolUseId: it.ID,
			Text:    redactCut(stripANSI(out), ResultMaxBytes),
			IsError: failed || (it.ExitCode != nil && *it.ExitCode != 0)}
		if r.Text == "" && r.IsError {
			r.Text = Clip(it.Status, toolInputMaxRunes)
		}
		if it.ExitCode != nil {
			r.Detail = fmt.Sprintf(`{"exitCode":%d}`, *it.ExitCode)
		}
		return one(r)
	case "fileChange":
		if done {
			return one(&athenav1.StreamItem{Kind: "tool_result", ToolUseId: it.ID, Text: Clip(it.Status, toolInputMaxRunes), IsError: failed})
		}
		changes := make([]map[string]any, 0, len(it.Changes))
		paths := make([]string, 0, len(it.Changes))
		for _, c := range it.Changes {
			var k struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(c.Kind, &k) != nil || k.Type == "" {
				_ = json.Unmarshal(c.Kind, &k.Type)
			}
			changes = append(changes, map[string]any{"path": c.Path, "kind": k.Type, "diff": c.Diff})
			paths = append(paths, c.Path)
		}
		return one(&athenav1.StreamItem{Kind: "tool_call", Tool: "apply_patch", ToolUseId: it.ID, Text: Clip(strings.Join(paths, ", "), toolInputMaxRunes),
			Detail: detailJSON(map[string]any{"changes": changes})})
	case "mcpToolCall":
		tool := "mcp__" + toolPart(it.Server) + "__" + toolPart(it.Tool)
		if !done {
			return one(&athenav1.StreamItem{Kind: "tool_call", Tool: tool, ToolUseId: it.ID, Text: summarizeInput(it.Arguments, toolInputMaxRunes), Detail: toolDetail(it.Arguments)})
		}
		text, isErr := "", failed
		if e := strings.TrimSpace(string(it.Error)); e != "" && e != "null" {
			isErr = true
			var o struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(it.Error, &o) != nil || o.Message == "" {
				_ = json.Unmarshal(it.Error, &text)
			} else {
				text = o.Message
			}
		} else {
			var res struct {
				Content json.RawMessage `json:"content"`
			}
			_ = json.Unmarshal(it.Result, &res)
			text = resultText(res.Content)
		}
		return one(&athenav1.StreamItem{Kind: "tool_result", ToolUseId: it.ID, IsError: isErr, Text: redactCut(stripANSI(text), ResultMaxBytes)})
	case "webSearch":
		// The query is known at the end (the start may carry none), so the call and its result come from the completed item.
		if done {
			return []*athenav1.StreamItem{
				{Kind: "tool_call", Tool: "web_search", ToolUseId: it.ID, Text: Clip(it.Query, toolInputMaxRunes), Detail: detailJSON(map[string]any{"query": it.Query})},
				{Kind: "tool_result", ToolUseId: it.ID, Text: "ok"},
			}
		}
	case "plan":
		if !done {
			return nil
		}
		steps := []map[string]any{}
		for _, l := range strings.Split(it.Text, "\n") {
			if l = strings.TrimSpace(listMark.ReplaceAllString(l, "")); l != "" {
				steps = append(steps, map[string]any{"step": l})
			}
		}
		return planItems(it.ID, steps)
	default:
		if done {
			return one(&athenav1.StreamItem{Kind: "other", Text: Clip("codex item "+it.Type, toolInputMaxRunes)})
		}
	}
	return nil
}

// planItems is the update_plan call and its result (Codex gives none, and a call without one shows as running).
func planItems(id string, steps []map[string]any) []*athenav1.StreamItem {
	return []*athenav1.StreamItem{
		{Kind: "tool_call", Tool: "update_plan", ToolUseId: id, Text: fmt.Sprintf("%d steps", len(steps)), Detail: detailJSON(map[string]any{"steps": steps})},
		{Kind: "tool_result", ToolUseId: id, Text: "ok"},
	}
}

// planStatus names a step's status as the dashboard's to-do marks do: pending, in_progress, completed.
func planStatus(v any) any {
	if v == "inProgress" {
		return "in_progress"
	}
	return v
}

// detailJSON is toolDetail of a map built here: masked, clipped and bounded like a Claude tool's input.
func detailJSON(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return toolDetail(b)
}

var badToolChar = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// toolPart makes a server or tool name safe to show as part of a tool name: word characters only, at most 64.
func toolPart(s string) string {
	s = badToolChar.ReplaceAllString(s, "_")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
