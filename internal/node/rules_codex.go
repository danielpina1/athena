package node

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/danielpina1/athena/internal/core"
)

// ---------------------------------------------------------------------------
// runtimes' tool calls

// ToolCall is one tool call in the rules' vocabulary (Claude Code's tool names and inputs: Bash{command}, Write{file_path}).
type ToolCall struct {
	Tool  string
	Input json.RawMessage
}

// codexShellTools are the Codex tools that run a shell command.
var codexShellTools = map[string]bool{"Bash": true, "shell": true, "local_shell": true, "exec_command": true, "unified_exec": true}

// NormalizeTool maps a runtime's tool call onto the rules' vocabulary. Claude Code's is one call, unchanged. Codex's:
//
//   - a shell tool (Bash, shell, local_shell, exec_command, unified_exec) is Bash{command}, an argv array joined with
//     POSIX shell quoting, with its "workdir" (or "cwd") as the Bash input's workdir; a command it cannot read is
//     Bash{command: ""}, which EvaluateCalls escalates. Codex applies a patch a shell command carries itself
//     (`apply_patch <<'EOF'…`, or the argv ["apply_patch", patch]), so a command that carries patch headers is also a
//     Write per path they name, relative paths under its workdir;
//   - apply_patch is one Write{file_path} per path its patch adds, updates, deletes or moves to;
//   - every other tool (write_stdin, mcp__…, web_search, the rest) is one call, unchanged.
//
// A patch whose paths cannot all be read (none, or a line naming a header without a path it reads) adds a Write with
// no path, which escalates.
func NormalizeTool(runtime, tool string, input json.RawMessage) []ToolCall {
	if runtime != core.RuntimeCodex {
		return []ToolCall{{Tool: tool, Input: input}}
	}
	switch {
	case codexShellTools[tool]:
		cmd, dir := codexCommand(input)
		in := map[string]string{"command": cmd}
		if dir != "" {
			in["workdir"] = dir
		}
		calls := []ToolCall{{Tool: "Bash", Input: mustJSON(in)}}
		if paths, bad := patchPaths(input); len(paths) > 0 || bad {
			for i, p := range paths {
				if dir != "" && !filepath.IsAbs(p) && !strings.HasPrefix(p, "~") {
					paths[i] = filepath.Join(dir, p)
				}
			}
			calls = append(calls, patchWrites(paths, bad)...)
		}
		return calls
	case tool == "apply_patch":
		paths, bad := patchPaths(input)
		return patchWrites(paths, bad || len(paths) == 0)
	}
	return []ToolCall{{Tool: tool, Input: input}}
}

// patchWrites are the Writes of a patch's paths, and with unread a Write with no path, which escalates.
func patchWrites(paths []string, unread bool) []ToolCall {
	calls := make([]ToolCall, 0, len(paths)+1)
	for _, p := range paths {
		calls = append(calls, ToolCall{Tool: "Write", Input: mustJSON(map[string]string{"file_path": p})})
	}
	if unread {
		calls = append(calls, ToolCall{Tool: "Write", Input: mustJSON(map[string]string{"file_path": ""})})
	}
	return calls
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// codexCommand reads a Codex shell call's command and working directory: the input itself (a string, or an argv
// array), or its "command" or "cmd" (either form), and its "workdir" or "cwd".
func codexCommand(input json.RawMessage) (cmd, dir string) {
	var v any
	if json.Unmarshal(input, &v) != nil {
		return "", ""
	}
	if m, ok := v.(map[string]any); ok {
		dir = str(m, "workdir")
		if dir == "" {
			dir = str(m, "cwd")
		}
		for _, k := range []string{"command", "cmd"} {
			if c, ok := commandText(m[k]); ok {
				return c, dir
			}
		}
		return "", dir
	}
	c, _ := commandText(v)
	return c, ""
}

// commandText is a command given as a string, or as an argv array joined with POSIX shell quoting.
func commandText(v any) (string, bool) {
	switch c := v.(type) {
	case string:
		return c, c != ""
	case []any:
		words := make([]string, 0, len(c))
		for _, w := range c {
			s, ok := w.(string)
			if !ok {
				return "", false
			}
			words = append(words, argvQuote(s))
		}
		return strings.Join(words, " "), len(words) > 0
	}
	return "", false
}

// argvQuote quotes an argv word for a POSIX shell; a word of only safe characters stays as it is.
func argvQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./_-", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// patchHeaders begin the lines of an apply_patch patch that name a path it writes; patchHeaderRe finds a line that
// mentions one, however spaced or cased.
var (
	patchHeaders  = []string{"*** Add File:", "*** Update File:", "*** Delete File:", "*** Move to:"}
	patchHeaderRe = regexp.MustCompile(`(?i)\*\*\*\s*(?:(?:add|update|delete)\s*file|move\s*to)`)
)

// patchPaths are the paths an apply_patch input writes, in order: those its patch's headers name, in whichever string
// of the input holds the patch (the input itself, or any field of it). Codex's parser trims the marker lines, so a
// header may be indented. bad reports a line that mentions a header the patch cannot be read from (no path, or text
// before it on the line): the patch may write a path this cannot name.
func patchPaths(input json.RawMessage) (paths []string, bad bool) {
	var v any
	if json.Unmarshal(input, &v) != nil {
		return nil, true
	}
	for _, s := range stringsIn(v, nil) {
		if !patchHeaderRe.MatchString(s) {
			continue
		}
		for _, line := range strings.Split(s, "\n") {
			if !patchHeaderRe.MatchString(line) {
				continue
			}
			line = strings.TrimSpace(line)
			read := false
			for _, h := range patchHeaders {
				if p, ok := strings.CutPrefix(line, h); ok && strings.TrimSpace(p) != "" {
					paths, read = append(paths, strings.TrimSpace(p)), true
					break
				}
			}
			bad = bad || !read
		}
	}
	return paths, bad
}

// stringsIn appends every string in v (a decoded JSON value), keys and values, to out.
func stringsIn(v any, out []string) []string {
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case []any:
		for _, e := range x {
			out = stringsIn(e, out)
		}
	case map[string]any:
		for k, e := range x {
			out = stringsIn(e, append(out, k))
		}
	}
	return out
}

// EvaluateCalls evaluates every call of one tool call of runtime (NormalizeTool) and returns the strictest decision
// (deny, then escalate, then allow; the first of equals) with the call it was made on. Codex calls are evaluated as
// their mapped tool: write_stdin by the Bash text rules (stdinRule); Claude Code's tool names, MCP tools and
// web_search by Evaluate; any other tool's input is searched, every string of it, for credential and state paths.
func EvaluateCalls(ctx RuleContext, runtime string, calls []ToolCall) (Decision, ToolCall) {
	best, at := allow(), ToolCall{}
	if len(calls) > 0 {
		at = calls[0]
	}
	for _, c := range calls {
		d := evaluateCall(ctx, runtime, c)
		if strictness(d.Verdict) > strictness(best.Verdict) {
			best, at = d, c
		}
		if best.Verdict == VerdictDeny {
			break
		}
	}
	return best, at
}

func strictness(verdict string) int {
	switch verdict {
	case VerdictDeny:
		return 2
	case VerdictEscalate:
		return 1
	}
	return 0
}

// claudeToolNames are Claude Code's tool names the rules know; a Codex call by one of them is evaluated as Claude's.
var claudeToolNames = map[string]bool{"Bash": true, "WebFetch": true, "WebSearch": true, "Task": true, "TodoWrite": true, "LS": true}

func evaluateCall(ctx RuleContext, runtime string, c ToolCall) Decision {
	if runtime != core.RuntimeCodex {
		return Evaluate(ctx, c.Tool, c.Input)
	}
	switch {
	case c.Tool == "write_stdin":
		return stdinRule(ctx, c.Input)
	case c.Tool == "Bash":
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(c.Input, &in) != nil || strings.TrimSpace(in.Command) == "" {
			return escalate(ruleInput, "the command cannot be read, ask the owner")
		}
		return Evaluate(ctx, c.Tool, c.Input)
	case fileTools[c.Tool], writeTools[c.Tool], claudeToolNames[c.Tool], strings.HasPrefix(c.Tool, "mcp__"), c.Tool == "web_search":
		return Evaluate(ctx, c.Tool, c.Input)
	}
	var v any
	if json.Unmarshal(c.Input, &v) != nil {
		return deny(ruleInput, "tool input is not JSON")
	}
	for _, s := range stringsIn(v, nil) {
		if reason := ctx.protectedText(s); reason != "" {
			return deny(ruleCredential, reason)
		}
	}
	return allow()
}

// stdinRule decides a write_stdin: what it types into a running process (an interactive shell or REPL the model
// started) is held to the Bash text rules, every string of it: a credential or state path, a token variable, a
// blocked command or an AI attribution denies; anything else is allowed (the process itself was allowed to start).
func stdinRule(ctx RuleContext, input json.RawMessage) Decision {
	var v any
	if json.Unmarshal(input, &v) != nil {
		return deny(ruleInput, "tool input is not JSON")
	}
	for _, s := range stringsIn(v, nil) {
		if d, ok := credentialRule(ctx, "Bash", map[string]any{"command": s}); ok {
			return d
		}
		if d, ok := blockedRule(ctx, s); ok {
			return d
		}
		if d, ok := attributionRule(ctx, s, ""); ok {
			return d
		}
	}
	return allow()
}
