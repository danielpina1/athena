package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
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

// RunHook implements `athena hook <event>`. It reads the hook input from
// stdin, asks the node over the Unix socket for a decision and writes the
// hook output Claude Code expects for that event. It returns the exit code.
//
// PreToolUse and PermissionRequest fail closed: any problem reaching the node
// yields a deny (printed as JSON, exit 0). The other events must not break the
// session, so a failure there prints the error to stderr and exits 0 silently.
//
// There is deliberately no timeout on the answer: an escalation blocks until a
// person decides.
func RunHook(event string, stdin io.Reader, stdout, stderr io.Writer, socket string) int {
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
			writeOutput(stdout, stderr, event, HookResponse{Verdict: VerdictDeny, Reason: reason})
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
		Input:      raw,
	})
	if err != nil {
		return fail(err, denyUnreachable)
	}
	if resp.Verdict != VerdictAllow && resp.Verdict != VerdictDeny {
		// The node resolves escalate before answering; anything else is a bug.
		return fail(fmt.Errorf("unexpected verdict %q from node", resp.Verdict), "athena node returned an unusable verdict")
	}
	writeOutput(stdout, stderr, event, resp)
	return 0
}

// askNode sends one request and waits for one response line.
func askNode(socket string, req HookRequest) (HookResponse, error) {
	var resp HookResponse
	err := callNode(socket, req, &resp)
	return resp, err
}

// writeOutput renders resp in the output shape of the event.
func writeOutput(stdout, stderr io.Writer, event string, resp HookResponse) {
	reason := resp.Reason
	if resp.Verdict == VerdictDeny && reason == "" {
		reason = "denied by athena"
	}
	var out any
	switch event {
	case EventPreToolUse:
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
