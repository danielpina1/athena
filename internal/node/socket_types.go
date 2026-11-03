package node

import "encoding/json"

// The node socket speaks JSON lines: one request line, one response line per
// connection. These types are shared by `athena hook` (client) and the node's
// socket server.

// OpHook is the op of a hook request.
const OpHook = "hook"

// Verdicts carried in HookResponse.Verdict. `athena hook` only ever acts on
// allow and deny; the node resolves escalate into an Approval before replying.
const (
	VerdictAllow    = "allow"
	VerdictDeny     = "deny"
	VerdictEscalate = "escalate"
)

// HookRequest asks the node for a decision about one Claude Code hook event.
type HookRequest struct {
	Op         string `json:"op"`
	Event      string `json:"event"`
	Ticket     string `json:"ticket"`
	Assignment string `json:"assignment"`
	// Input is the raw JSON the hook received on stdin, forwarded verbatim.
	Input json.RawMessage `json:"input"`
}

// HookResponse is the node's answer.
type HookResponse struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
	// AdditionalContext is optional and used for SessionStart.
	AdditionalContext string `json:"additionalContext,omitempty"`
}
