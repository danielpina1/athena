package node

import "encoding/json"

// The node socket speaks JSON lines: one request line, one response line per
// connection. These types are shared by `athena hook` and `athena mcp`
// (clients) and the node's socket server.

// OpHook is the op of a hook request.
const OpHook = "hook"

// Verdicts carried in HookResponse.Verdict. `athena hook` only ever acts on
// allow and deny; the node resolves escalate into an Approval before replying.
const (
	VerdictAllow    = "allow"
	VerdictDeny     = "deny"
	VerdictEscalate = "escalate"
)

// HookRequest asks the node for a decision about one hook event of a runtime (Claude Code or Codex).
type HookRequest struct {
	Op         string `json:"op"`
	Event      string `json:"event"`
	Ticket     string `json:"ticket"`
	Assignment string `json:"assignment"`
	// Role is RoleRouter for the router session's hooks, which carry the router start's Token instead of a Ticket and
	// Assignment; empty for a worker's.
	Role  string `json:"role,omitempty"`
	Token string `json:"token,omitempty"`
	// Runtime is the runtime whose hook this is (`athena hook --runtime`): "claude" or "codex", whose tool names and
	// inputs differ. Empty is Claude Code's.
	Runtime string `json:"runtime,omitempty"`
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

// Ops other than hook.
const (
	// OpReport files a worker's Report; `athena mcp` sends it for the report tool.
	OpReport = "report"
	// OpPing checks that the node answers.
	OpPing = "ping"
)

// ReportRequest files a Report for the session of Ticket and Assignment.
type ReportRequest struct {
	Op         string `json:"op"`
	Ticket     string `json:"ticket"`
	Assignment string `json:"assignment"`
	Status     string `json:"status"`
	Summary    string `json:"summary"`
	Question   string `json:"question,omitempty"`
	// Pull is the number of the Ticket's pull request in its repository, bound to the Ticket as its own Thread.
	Pull int `json:"pull,omitempty"`
}

// PingRequest asks the node whether it answers.
type PingRequest struct {
	Op string `json:"op"`
}

// Response is the node's answer to every op but hook: OK, or the Error that refused it.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// RoleRouter is the role of the router session: its hooks and tool calls carry the token of the current router start
// (ATHENA_ROUTER_TOKEN), never Ticket or Assignment ids.
const RoleRouter = "router"

// OpRouter runs one of the router's tools; `athena mcp --role router` sends it.
const OpRouter = "router"

// RouterRequest asks the node to run one router tool with its JSON arguments.
type RouterRequest struct {
	Op    string          `json:"op"`
	Token string          `json:"token"`
	Tool  string          `json:"tool"`
	Args  json.RawMessage `json:"args,omitempty"`
}

// RouterResponse is a router tool's JSON result, or the Error that refused the call.
type RouterResponse struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}
