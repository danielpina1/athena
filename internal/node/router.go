package node

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// The router role of the node socket: the router session's hooks and tool calls. They name no Ticket or Assignment;
// each carries the token of the current router start, which only the router's own processes have (its MCP config and
// environment), and one without it is refused.

const denyNotRouter = "not the running athena router: the router token is missing or stale"

// RouterHandler is the router's side of the router role, the router host.
type RouterHandler interface {
	// ValidToken reports whether token is the current router start's.
	ValidToken(token string) bool
	// CallTool runs one router tool with its JSON arguments and returns its result, or the error the model reads.
	CallTool(ctx context.Context, tool string, args json.RawMessage) (any, error)
}

type routerRef struct{ h RouterHandler }

// SetRouter sets the router that serves the router role; until it is set, every router op is refused.
func (n *Node) SetRouter(h RouterHandler) { n.router.Store(&routerRef{h}) }

func (n *Node) routerHandler(token string) RouterHandler {
	if r := n.router.Load(); r != nil && r.h != nil && r.h.ValidToken(token) {
		return r.h
	}
	return nil
}

// handleRouterHook decides a hook of the router session: a Stop and the other events that decide nothing are allowed,
// and a tool call is judged by RouterRules. A hook without the current token is denied (a Stop is still allowed).
func (n *Node) handleRouterHook(req HookRequest) HookResponse {
	start := time.Now()
	if req.Event == EventStop {
		n.observeHook(req.Event, VerdictAllow, start)
		return allowed("")
	}
	if n.routerHandler(req.Token) == nil {
		n.log.Warn("router hook without the router token", "event", req.Event)
		n.observeHook(req.Event, VerdictDeny, start)
		return denied(denyNotRouter)
	}
	switch req.Event {
	case EventPostToolUse, EventSessionStart:
		n.observeHook(req.Event, VerdictAllow, start)
		return allowed("")
	case EventPreToolUse, EventPermissionRequest:
		var in struct {
			ToolName string `json:"tool_name"`
		}
		_ = json.Unmarshal(req.Input, &in)
		d := RouterRules(in.ToolName)
		n.log.Info("router hook decision", "event", req.Event, "tool", in.ToolName, "verdict", d.Verdict)
		n.observeHook(req.Event, d.Verdict, start)
		if d.Verdict == VerdictAllow {
			return allowed("")
		}
		return denied(d.Reason)
	default:
		n.observeHook(req.Event, VerdictDeny, start)
		return denied(fmt.Sprintf("unknown hook event %q", req.Event))
	}
}

// handleRouter runs one router tool for the router session that holds the current token.
func (n *Node) handleRouter(ctx context.Context, req RouterRequest) RouterResponse {
	h := n.routerHandler(req.Token)
	if h == nil {
		n.log.Warn("router op without the router token", "tool", req.Tool)
		return RouterResponse{Error: denyNotRouter}
	}
	// The tool name becomes a metric label only once it is known to be one of the router's.
	label := "router_unknown"
	known := slices.Contains(RouterTools(), req.Tool)
	if known {
		label = "router_" + req.Tool
	}
	if n.reg != nil {
		start := time.Now()
		defer func() { n.reg.Histogram("mcp_tool_seconds", "tool", label).Observe(time.Since(start)) }()
	}
	if !known {
		return RouterResponse{Error: fmt.Sprintf("unknown router tool %q", req.Tool)}
	}
	res, err := h.CallTool(ctx, req.Tool, req.Args)
	if err != nil {
		return RouterResponse{Error: err.Error()}
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return RouterResponse{Error: "the router's result does not encode: " + err.Error()}
	}
	return RouterResponse{OK: true, Result: raw}
}
