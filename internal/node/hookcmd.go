package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

const (
	denyUnreachable = "athena node unreachable"
	dialTimeout     = 5 * time.Second
)

// Events handled by `athena hook`, spelled as Claude Code spells them.
const (
	EventPreToolUse        = "PreToolUse"
	EventPermissionRequest = "PermissionRequest"
	EventPostToolUse       = "PostToolUse"
	EventStop              = "Stop"
	EventSessionStart      = "SessionStart"
)

// HookMain implements `athena hook [--runtime claude|codex] <event>` (RunHookFor). The runtime is claude when it is not
// named, so the settings Claude Code sessions already have keep working. A malformed command line exits 2.
func HookMain(args []string, stdin io.Reader, stdout, stderr io.Writer, socket string) int {
	runtime := core.RuntimeClaude
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--runtime" && i+1 < len(args):
			runtime = args[i+1]
			i++
		case strings.HasPrefix(a, "--runtime="):
			runtime = strings.TrimPrefix(a, "--runtime=")
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 || (runtime != core.RuntimeClaude && runtime != core.RuntimeCodex) {
		fmt.Fprintln(stderr, "usage: athena hook [--runtime claude|codex] <event>")
		return 2
	}
	return RunHookFor(runtime, rest[0], stdin, stdout, stderr, socket)
}

// RunHook is RunHookFor Claude Code.
func RunHook(event string, stdin io.Reader, stdout, stderr io.Writer, socket string) int {
	return RunHookFor(core.RuntimeClaude, event, stdin, stdout, stderr, socket)
}

// RunHookFor implements `athena hook --runtime <runtime> <event>`. It reads the hook input from
// stdin, asks the node over the Unix socket for a decision and writes the
// hook output the runtime expects for that event. It returns the exit code.
// Both runtimes take Claude Code's output shapes, except that Codex refuses a
// PreToolUse allow (so it gets no output: Codex goes on as without the hook)
// and a deny without a reason (a deny always carries one).
//
// PreToolUse and PermissionRequest fail closed: any problem reaching the node
// yields a deny (printed as JSON, exit 0). The other events must not break the
// session, so a failure there prints the error to stderr and exits 0 silently.
//
// There is deliberately no timeout on the answer: an escalation blocks until a
// person decides.
func RunHookFor(runtime, event string, stdin io.Reader, stdout, stderr io.Writer, socket string) int {
	decisionEvent := false
	switch event {
	case EventPreToolUse, EventPermissionRequest:
		decisionEvent = true
	case EventPostToolUse, EventStop, EventSessionStart:
	default:
		fmt.Fprintf(stderr, "athena hook: unknown event %q\n", event)
		return 2
	}

	fail := func(err error, reason string) int {
		fmt.Fprintf(stderr, "athena hook %s: %v\n", event, err)
		if decisionEvent {
			writeOutput(stdout, stderr, runtime, event, HookResponse{Verdict: VerdictDeny, Reason: reason})
		}
		return 0
	}

	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fail(fmt.Errorf("read stdin: %w", err), "athena hook could not read its input")
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || !json.Valid(raw) {
		return fail(errors.New("hook input is not valid JSON"), "athena hook received invalid input")
	}

	resp, err := askNode(socket, HookRequest{
		Op:         OpHook,
		Event:      event,
		Ticket:     os.Getenv("ATHENA_TICKET"),
		Assignment: os.Getenv("ATHENA_ASSIGNMENT"),
		Role:       os.Getenv("ATHENA_ROLE"),
		Token:      os.Getenv("ATHENA_ROUTER_TOKEN"),
		Runtime:    runtime,
		Input:      raw,
	})
	if err != nil {
		return fail(err, denyUnreachable)
	}
	if resp.Verdict != VerdictAllow && resp.Verdict != VerdictDeny {
		// The node resolves escalate before answering; anything else is a bug.
		return fail(fmt.Errorf("unexpected verdict %q from node", resp.Verdict), "athena node returned an unusable verdict")
	}
	writeOutput(stdout, stderr, runtime, event, resp)
	return 0
}

// askNode sends one request and waits for one response line.
func askNode(socket string, req HookRequest) (HookResponse, error) {
	var resp HookResponse
	err := callNode(socket, req, &resp)
	return resp, err
}

// writeOutput renders resp in the output shape of the runtime's event (pre-tool-use.command.output and
// permission-request.command.output for Codex, which follow Claude Code's).
func writeOutput(stdout, stderr io.Writer, runtime, event string, resp HookResponse) {
	reason := resp.Reason
	if resp.Verdict == VerdictDeny && reason == "" {
		reason = "denied by athena"
	}
	var out any
	switch event {
	case EventPreToolUse:
		if runtime == core.RuntimeCodex && resp.Verdict != VerdictDeny {
			// Codex 0.160.1 refuses permissionDecision "allow" ("unsupported"): no decision lets the call go on.
			break
		}
		hso := map[string]any{
			"hookEventName":      event,
			"permissionDecision": resp.Verdict,
		}
		if reason != "" {
			hso["permissionDecisionReason"] = reason
		}
		out = map[string]any{"hookSpecificOutput": hso}
	case EventPermissionRequest:
		d := map[string]any{"behavior": resp.Verdict}
		if resp.Verdict == VerdictDeny {
			d["message"] = reason
		}
		out = map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": event,
			"decision":      d,
		}}
	case EventPostToolUse, EventStop:
		if resp.Verdict == VerdictDeny {
			out = map[string]any{"decision": "block", "reason": reason}
		}
	case EventSessionStart:
		if resp.AdditionalContext != "" {
			out = map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName":     event,
				"additionalContext": resp.AdditionalContext,
			}}
		}
	}
	if out == nil {
		return
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		fmt.Fprintf(stderr, "athena hook %s: write output: %v\n", event, err)
	}
}
