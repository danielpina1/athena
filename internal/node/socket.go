package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/redact"
	"github.com/danielpina1/athena/internal/runtime/rt"
	"github.com/danielpina1/athena/internal/sources/github"
)

const (
	// maxRequestBytes bounds one request line (a Write hook carries the whole file content).
	maxRequestBytes = 16 << 20
	// readRequestTimeout bounds how long a client may take to send its request line.
	readRequestTimeout = 30 * time.Second
	denyNoSession      = "not a live athena session"
	summaryMaxRunes    = 300
)

// Serve serves the node's Unix socket (mode 0600) until ctx is done: one JSON request line and one response line per
// connection. Ops: hook (the hook rules, Approvals for escalations, Stop as the end of a turn, SessionStart context),
// report (a worker's Report), review (a review session's verdict), worker (a worker's other tools), worker_tools (the
// tools a worker's MCP server offers), credential (a
// session's GitHub token, for its credential helper and gh wrapper), router (the router's tools) and ping. Every op names its Ticket and Assignment, and one that does not match a live
// session is refused: a hook is denied (a Stop is still allowed), a report fails.
func (n *Node) Serve(ctx context.Context) error {
	path := n.cfg.SocketPath
	if path == "" {
		return errors.New("node: SocketPath is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("node: socket dir: %w", err)
	}
	if err := removeStaleSocket(path); err != nil {
		return err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("node: listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return fmt.Errorf("node: chmod %s: %w", path, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	var wg sync.WaitGroup
	var acceptErr error
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() == nil {
				acceptErr = fmt.Errorf("node: accept: %w", err)
				cancel()
			}
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.handleConn(ctx, conn)
		}()
	}
	wg.Wait()
	_ = os.Remove(path)
	return acceptErr
}

// removeStaleSocket removes a socket file nobody listens on; one somebody answers on is an error.
func removeStaleSocket(path string) error {
	if _, err := os.Lstat(path); err != nil {
		return nil
	}
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return fmt.Errorf("node: another node is serving %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("node: remove stale socket: %w", err)
	}
	return nil
}

func (n *Node) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(readRequestTimeout))
	line, err := bufio.NewReader(io.LimitReader(conn, maxRequestBytes)).ReadBytes('\n')
	if err != nil && (err != io.EOF || len(line) == 0) {
		n.log.Warn("socket: read request", "error", err)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	var head struct {
		Op string `json:"op"`
	}
	var resp any
	if err := json.Unmarshal(line, &head); err != nil {
		resp = Response{Error: "request is not JSON"}
	} else {
		switch head.Op {
		case OpHook:
			var req HookRequest
			if err := json.Unmarshal(line, &req); err != nil {
				resp = HookResponse{Verdict: VerdictDeny, Reason: "malformed hook request"}
			} else {
				resp = n.handleHook(ctx, req)
			}
		case OpReport:
			var req ReportRequest
			if err := json.Unmarshal(line, &req); err != nil {
				resp = Response{Error: "malformed report request"}
			} else {
				resp = n.handleReport(req)
			}
		case OpRouter:
			var req RouterRequest
			if err := json.Unmarshal(line, &req); err != nil {
				resp = RouterResponse{Error: "malformed router request"}
			} else {
				resp = n.handleRouter(ctx, req)
			}
		case OpReview:
			var req ReviewRequest
			if err := json.Unmarshal(line, &req); err != nil {
				resp = Response{Error: "malformed review request"}
			} else {
				resp = n.handleReview(req)
			}
		case OpWorker:
			var req WorkerRequest
			if err := json.Unmarshal(line, &req); err != nil {
				resp = WorkerResponse{Error: "malformed worker request"}
			} else {
				resp = n.handleWorker(req)
			}
		case OpWorkerTools:
			var req WorkerRequest
			if err := json.Unmarshal(line, &req); err != nil {
				resp = WorkerToolsResponse{Error: "malformed worker tools request"}
			} else {
				resp = n.handleWorkerTools(req)
			}
		case OpCredential:
			var req CredentialRequest
			if err := json.Unmarshal(line, &req); err != nil {
				resp = CredentialResponse{Error: "malformed credential request"}
			} else {
				resp = n.handleCredential(ctx, req)
			}
		case OpPing:
			resp = Response{OK: true}
		default:
			resp = Response{Error: fmt.Sprintf("unknown op %q", head.Op)}
		}
	}
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		n.log.Warn("socket: write response", "op", head.Op, "error", err)
	}
}

func allowed(reason string) HookResponse { return HookResponse{Verdict: VerdictAllow, Reason: reason} }
func denied(reason string) HookResponse  { return HookResponse{Verdict: VerdictDeny, Reason: reason} }

func (n *Node) handleHook(ctx context.Context, req HookRequest) HookResponse {
	if req.Role == RoleRouter {
		return n.handleRouterHook(req)
	}
	start := time.Now()
	s := n.lookup(req.Ticket, req.Assignment)
	if req.Event == EventStop {
		// Never block a Stop: a blocking answer would loop the session.
		// A Stop that comes while a shutdown closes the session is no turn end: the turn was cut short, and recording
		// it would make the session look idle, never resumed.
		if s != nil && !s.stoppedByShutdown() {
			if err := n.engine().TurnEnded(req.Ticket, req.Assignment); err != nil {
				s.log.Error("record turn end", "error", err)
			}
		}
		n.observeHook(req.Event, VerdictAllow, start)
		return allowed("")
	}
	if s == nil {
		n.log.Warn("hook from an unknown session", "ticket", req.Ticket, "assignment", req.Assignment, "event", req.Event)
		n.observeHook(req.Event, VerdictDeny, start)
		return denied(denyNoSession)
	}
	switch req.Event {
	case EventPostToolUse:
		n.observeHook(req.Event, VerdictAllow, start)
		return allowed("")
	case EventSessionStart:
		resp := HookResponse{Verdict: VerdictAllow, AdditionalContext: n.sessionContext(s)}
		n.observeHook(req.Event, VerdictAllow, start)
		return resp
	case EventPreToolUse, EventPermissionRequest:
		runtime := hookRuntime(s)
		if req.Runtime != "" && req.Runtime != runtime {
			// The hook reads its input as another runtime's: whatever it asks about, it cannot be judged.
			s.log.Warn("hook names another runtime", "hook", req.Runtime, "session", runtime)
			n.observeHook(req.Event, VerdictDeny, start)
			s.noteDenial()
			n.recordUsage(s, core.UsageDelta{HookDenials: 1})
			return denied(fmt.Sprintf("this hook runs as %s, but the session runs on %s", req.Runtime, runtime))
		}
		resp := n.decide(ctx, s, req.Event, runtime, req.Input, start)
		if resp.Verdict == VerdictDeny {
			// A rule's, the owner's or an expiry's deny: the tool call fails, maybe beside a report in the same step.
			s.noteDenial()
			n.recordUsage(s, core.UsageDelta{HookDenials: 1})
		}
		return resp
	default:
		n.observeHook(req.Event, VerdictDeny, start)
		return denied(fmt.Sprintf("unknown hook event %q", req.Event))
	}
}

// observeHook records the latency of a hook decision made since start as hook_decision_seconds{event,verdict}. The
// wait for a person's answer to an escalation is not part of it.
func (n *Node) observeHook(event, verdict string, start time.Time) {
	if n.reg == nil {
		return
	}
	switch event {
	case EventPreToolUse, EventPermissionRequest, EventPostToolUse, EventStop, EventSessionStart:
	default:
		event = "other"
	}
	n.reg.Histogram("hook_decision_seconds", "event", event, "verdict", verdict).Observe(time.Since(start))
}

// recordUsage adds usage observed outside the session's runner to the Ticket's stats. It holds no node lock.
func (n *Node) recordUsage(s *session, u core.UsageDelta) {
	if err := n.engine().RecordUsage(s.t.ID, s.a.ID, u); err != nil {
		s.log.Error("record usage", "error", err)
	}
}

// hookRuntime is the runtime whose tool vocabulary and rules a hook's input gets: the session's own (the runtime its
// runner started). A hook that names another runtime (`athena hook --runtime`) is refused by handleHook.
func hookRuntime(s *session) string { return s.currentRuntime() }

// ruleContext is what the rules know of a session; a Codex session's protected paths are denied by the rules, since
// its sandbox cannot keep them read-only, a review session writes nothing, and a restricted one asks nobody.
func (n *Node) ruleContext(s *session, runtime string) RuleContext {
	home, _ := os.UserHomeDir()
	c := RuleContext{TicketDir: s.place.workDir, Blocked: n.cfg.Blocked, Home: home, StateDir: n.cfg.StateDir,
		AthenaBin: n.cfg.AthenaBin, VenvActive: false, ReadOnly: s.reviewing(), Restricted: s.restricted()}
	if runtime == core.RuntimeCodex {
		c.Protected = ProtectedPaths(s.place.workDir)
	}
	return c
}

// The doors through which Codex asks about running an action outside its sandbox: its PermissionRequest hook and its
// approval request (approveRequest). Asks through these doors share an escalation (escalateShared); a PreToolUse ask
// is about running the action in the sandbox, and shares nothing.
const (
	doorHook    = "hook"
	doorRequest = "request"
)

// decide applies the hook rules to one tool call of runtime (NormalizeTool); an escalation becomes an Approval the call
// waits for.
func (n *Node) decide(ctx context.Context, s *session, event, runtime string, raw json.RawMessage, start time.Time) HookResponse {
	var in struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
		// TurnID is Codex's: the turn the hook runs in.
		TurnID string `json:"turn_id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || in.ToolName == "" {
		n.observeHook(event, VerdictDeny, start)
		return denied("hook input names no tool")
	}
	door := ""
	if runtime == core.RuntimeCodex && event == EventPermissionRequest {
		door = doorHook
	}
	return n.judge(ctx, s, judgment{event: event, runtime: runtime, tool: in.ToolName, input: in.ToolInput,
		calls: NormalizeTool(runtime, in.ToolName, in.ToolInput), turn: in.TurnID, door: door}, start)
}

// judgment is one ask about a tool call: the hook event, the runtime, the tool and input as asked about, the calls the
// rules judge (NormalizeTool), the turn, the door it came through (doorHook, doorRequest, or none), and a note the
// asker added (a file approval's reason and write grant).
type judgment struct {
	event, runtime, tool string
	input                json.RawMessage
	calls                []ToolCall
	turn, door, note     string
}

// ruleCodexUnsandboxed escalates what Codex asks to run outside its sandbox, when the node sandboxes its workers.
const ruleCodexUnsandboxed = "codex_unsandboxed"

// judge applies the rules to the calls of one ask: the strictest verdict stands, and an escalation becomes an Approval
// (escalateShared). An ask through a door (Codex asking to run a command or write files outside its sandbox) on a node
// that sandboxes its workers escalates what the rules allow: a sandboxed Claude Code worker cannot run anything
// unsandboxed (allowUnsandboxedCommands: false), and a Codex one may only with a person's yes. Without a sandbox, Claude
// Code's commands run unsandboxed under the rules alone, and so do these; a restricted session always runs sandboxed,
// so its asks through a door are escalations too, which it may not make (escalateShared). A review session's ask
// through a door is denied, sandbox or not: it stays in its read-only sandbox.
func (n *Node) judge(ctx context.Context, s *session, j judgment, start time.Time) HookResponse {
	if j.door != "" && s.reviewing() {
		// A review runs in its read-only sandbox, whatever the rules would allow: an ask to run or write outside it is
		// refused, with or without a node sandbox, and asks no person.
		s.log.Info("hook decision", "event", j.event, "runtime", j.runtime, "tool", j.tool, "verdict", VerdictDeny, "rule", ruleReadOnly)
		n.observeHook(j.event, VerdictDeny, start)
		return denied("this review session is read-only (" + ruleReadOnly + "): it runs nothing outside its sandbox")
	}
	rc := n.ruleContext(s, j.runtime)
	d, at := EvaluateCalls(rc, j.runtime, j.calls)
	unsandboxed := d.Verdict == VerdictAllow && j.door != "" && (n.cfg.Sandbox != nil || rc.Restricted) && runsOrWrites(j.calls)
	if unsandboxed {
		d = escalate(ruleCodexUnsandboxed, "running outside the sandbox needs approval")
	}
	// Never log the tool input: it may name credential paths or carry secrets.
	s.log.Info("hook decision", "event", j.event, "runtime", j.runtime, "tool", j.tool, "verdict", d.Verdict, "rule", d.Rule)
	switch d.Verdict {
	case VerdictAllow:
		n.observeHook(j.event, VerdictAllow, start)
		return allowed("")
	case VerdictEscalate:
		var summary string
		if unsandboxed {
			summary = summaryOf("Run outside the sandbox", whats(j.calls), j.note)
		} else {
			// Every call the rules escalate, not only the first.
			var esc []ToolCall
			for _, c := range j.calls {
				if evaluateCall(rc, j.runtime, c).Verdict == VerdictEscalate {
					esc = append(esc, c)
				}
			}
			summary = summaryOf(d.Reason+" — "+at.Tool, whats(esc), j.note)
		}
		ask := approvalAsk{tool: j.tool, input: j.input, d: d, summary: summary}
		key := ""
		if j.door != "" {
			key = shareKey(j.turn, j.calls)
		}
		return n.escalateShared(ctx, s, j.event, key, j.door, ask, start)
	default:
		n.observeHook(j.event, VerdictDeny, start)
		return denied(d.Reason)
	}
}

// runsOrWrites reports whether calls run a command or write a file: what Codex's sandbox confines.
func runsOrWrites(calls []ToolCall) bool {
	for _, c := range calls {
		if c.Tool == "Bash" || writeTools[c.Tool] {
			return true
		}
	}
	return false
}

// approvalAsk is what an escalation shows a person: the tool call as asked about, the rule's decision, and a line
// saying what it would do.
type approvalAsk struct {
	tool    string
	input   json.RawMessage
	d       Decision
	summary string
}

// shareWindow is how long an escalation's answer waits for the other door's ask about the same action.
const shareWindow = 10 * time.Minute

// sharedVerdict is an escalation's answer, once done is closed, and the doors that took it.
type sharedVerdict struct {
	done  chan struct{}
	resp  HookResponse
	at    time.Time
	doors map[string]bool
}

// shareKey names what an ask is about in a turn, for escalateShared: the turn and every call's tool and subject,
// sorted; "" (nothing shared) without a turn.
func shareKey(turn string, calls []ToolCall) string {
	if turn == "" {
		return ""
	}
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, c.Tool+"\x00"+callSubject(c))
	}
	slices.Sort(parts)
	return turn + "\x01" + strings.Join(parts, "\x01")
}

// callSubject is what a call does, for shareKey: a command's exact text, a write's path, or the input. An input that
// does not decode is its raw bytes for every tool, so two asks share only when they are byte for byte the same.
func callSubject(c ToolCall) string {
	var in struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal(c.Input, &in) != nil {
		return string(c.Input)
	}
	switch c.Tool {
	case "Bash":
		return in.Command
	case "Write":
		return in.FilePath
	}
	return string(c.Input)
}

// escalateShared is escalate, once per action asked about through both doors: Codex may ask about one action outside
// its sandbox through its PermissionRequest hook and through an approval request, in either order. The first ask
// creates the Approval; the other door's ask with the same key, while it waits or within shareWindow of its answer,
// gets the same answer, and the answer is then dropped. The same door asking again (the action run again) asks anew.
// An empty key shares nothing. A review session's ask is denied: it asks no person; so is a restricted session's.
func (n *Node) escalateShared(ctx context.Context, s *session, event, key, door string, ask approvalAsk, start time.Time) HookResponse {
	if s.restricted() {
		// Work a stranger started asks the owner nothing: an Approval would let it put its asks before them.
		s.log.Info("restricted session's escalation denied", "rule", ask.d.Rule)
		n.observeHook(event, VerdictDeny, start)
		return denied(restrictedEscalateReason)
	}
	if s.reviewing() {
		// A review asks no person: an Approval would put the Ticket in a person stage, which a review never does.
		s.log.Info("review session's escalation denied", "rule", ask.d.Rule)
		n.observeHook(event, VerdictDeny, start)
		return denied("this review session is read-only and asks no one for approval (" + ask.d.Reason + "); review " +
			"without it, and say in a finding what you could not check")
	}
	if key == "" {
		return n.escalate(ctx, s, event, ask, start)
	}
	s.mu.Lock()
	if s.shared == nil {
		s.shared = map[string]*sharedVerdict{}
	}
	for k, v := range s.shared {
		if !v.at.IsZero() && time.Since(v.at) > shareWindow {
			delete(s.shared, k)
		}
	}
	if sv := s.shared[key]; sv != nil && !sv.doors[door] {
		sv.doors[door] = true
		if len(sv.doors) >= 2 {
			delete(s.shared, key)
		}
		s.mu.Unlock()
		n.observeHook(event, VerdictEscalate, start)
		s.log.Info("approval shared with an earlier ask", "rule", ask.d.Rule)
		select {
		case <-sv.done:
			return sv.resp
		case <-ctx.Done():
			return denied("the athena node is shutting down; do not retry the same action, find another way or report blocked")
		case <-s.stopCh:
			return denied("the session ended before the owner answered; do not retry the same action, find another way or report blocked")
		}
	}
	sv := &sharedVerdict{done: make(chan struct{}), doors: map[string]bool{door: true}}
	s.shared[key] = sv
	s.mu.Unlock()
	resp := n.escalate(ctx, s, event, ask, start)
	s.mu.Lock()
	sv.resp, sv.at = resp, time.Now()
	s.mu.Unlock()
	close(sv.done)
	return resp
}

// escalate asks a person through an Approval and waits up to ApprovalTimeout, or until the session stops, for the
// answer; no answer is a deny and the Approval expires.
func (n *Node) escalate(ctx context.Context, s *session, event string, ask approvalAsk, start time.Time) HookResponse {
	eng := n.engine()
	ap, err := eng.RequestApproval(core.Approval{
		TicketID: s.t.ID, AssignmentID: s.a.ID, Tool: ask.tool, Rule: ask.d.Rule, Input: ask.input, Summary: ask.summary,
	})
	n.observeHook(event, VerdictEscalate, start)
	if err != nil {
		s.log.Error("request approval", "error", err)
		return denied("approval could not be requested: " + err.Error())
	}
	s.log.Info("approval requested", "approval", ap.ID, "rule", ask.d.Rule)
	// The wait ends with the session too: nobody can act on an answer once it stopped (a retry, a stop, a shutdown).
	wctx, cancel := context.WithTimeout(ctx, n.cfg.ApprovalTimeout)
	go func() {
		select {
		case <-s.stopCh:
			cancel()
		case <-wctx.Done():
		}
	}()
	got, err := eng.WaitApproval(wctx, ap.ID)
	cancel()
	if err != nil {
		if xerr := eng.ExpireApproval(ap.ID); xerr != nil {
			s.log.Error("expire approval", "approval", ap.ID, "error", xerr)
		}
		// An answer may have landed just before the expiry; it wins. Look without waiting: a settled Approval
		// returns at once, and one still pending (the expiry failed) must not hold the hook.
		done, stop := context.WithCancel(context.Background())
		stop()
		if got, err = eng.WaitApproval(done, ap.ID); err != nil || got.State == core.ApprovalPending {
			return denied("approval not answered; do not retry the same action, find another way or report blocked")
		}
	}
	switch got.State {
	case core.ApprovalApproved:
		return allowed(got.Answer)
	case core.ApprovalDenied:
		reason := "denied by the owner"
		if got.Answer != "" {
			reason += ": " + got.Answer
		}
		return denied(reason)
	default:
		reason := fmt.Sprintf("no answer from the owner within %s", n.cfg.ApprovalTimeout)
		if got.Answer != "" {
			reason = "approval expired: " + got.Answer
		}
		if ctx.Err() != nil {
			reason = "the athena node is shutting down"
		} else if s.isStopped() {
			reason = "the session ended before the owner answered"
		}
		return denied(reason + "; do not retry the same action, find another way or report blocked")
	}
}

// approveRequest decides an approval Codex asks its client for (rt.StartOptions.Approve), as the hooks decide:
//
//   - command: the rules' verdict on Bash{command} (run in its Cwd), as for a PermissionRequest hook; on a node that
//     sandboxes its workers, a person's when the rules allow it (judge: it runs outside Codex's sandbox);
//   - file: the strictest verdict over a Write of each path, the same way;
//   - network: always a person's (rule network_access), "Network: <host>";
//   - permissions: always a person's (rule codex_permissions), with everything accepting grants.
//
// A restricted session asks nobody (escalateShared): what would ask a person is declined.
//
// An escalation becomes an Approval exactly as a hook's does (the timeout, the expiry, the end with the session), and
// a command or file ask about an action the PermissionRequest hook of the same turn already escalated shares that
// Approval (escalateShared). A deny counts as a hook denial.
func (n *Node) approveRequest(ctx context.Context, s *session, req rt.ApprovalRequest) rt.Verdict {
	start := time.Now()
	const runtime = core.RuntimeCodex
	var resp HookResponse
	switch req.Kind {
	case "command":
		in := map[string]string{"command": req.Command}
		if req.Cwd != "" {
			in["workdir"] = req.Cwd
		}
		call := ToolCall{Tool: "Bash", Input: mustJSON(in)}
		resp = n.judge(ctx, s, judgment{event: EventPermissionRequest, runtime: runtime, tool: call.Tool, input: call.Input,
			calls: []ToolCall{call}, turn: req.TurnID, door: doorRequest}, start)
	case "file":
		calls := make([]ToolCall, 0, len(req.Paths))
		for _, p := range req.Paths {
			calls = append(calls, ToolCall{Tool: "Write", Input: mustJSON(map[string]string{"file_path": p})})
		}
		if len(calls) == 0 {
			calls = append(calls, ToolCall{Tool: "Write", Input: mustJSON(map[string]string{"file_path": ""})})
		}
		input := mustJSON(map[string]any{"paths": req.Paths, "reason": req.Reason})
		resp = n.judge(ctx, s, judgment{event: EventPermissionRequest, runtime: runtime, tool: "apply_patch", input: input,
			calls: calls, turn: req.TurnID, door: doorRequest, note: req.Reason}, start)
	case "network":
		host := hostName(req.Host)
		ask := approvalAsk{tool: "network", input: mustJSON(map[string]string{"host": req.Host, "command": req.Command, "reason": req.Reason}),
			d: escalate(ruleNetworkAccess, "network access needs approval"), summary: clip(redact.String("Network: " + host))}
		resp = n.escalateShared(ctx, s, EventPermissionRequest, "", "", ask, start)
	case "permissions":
		var parts []string
		if len(req.Paths) > 0 {
			parts = append(parts, "write "+strings.Join(req.Paths, ", "))
		}
		if req.Host != "" {
			parts = append(parts, "network: "+hostName(req.Host))
		}
		if req.Reason != "" {
			parts = append(parts, req.Reason)
		}
		ask := approvalAsk{tool: "permissions", input: mustJSON(map[string]any{"paths": req.Paths, "host": req.Host, "reason": req.Reason}),
			d: escalate(ruleCodexPermissions, "more permissions need approval"), summary: clip(redact.String("Permissions: " + strings.Join(parts, "; ")))}
		resp = n.escalateShared(ctx, s, EventPermissionRequest, "", "", ask, start)
	default:
		n.observeHook(EventPermissionRequest, VerdictDeny, start)
		resp = denied("unknown approval kind " + req.Kind)
	}
	if resp.Verdict == VerdictDeny {
		s.noteDenial()
		n.recordUsage(s, core.UsageDelta{HookDenials: 1})
	}
	return rt.Verdict{Allow: resp.Verdict == VerdictAllow, Reason: resp.Reason}
}

// The rules of the approvals Codex asks for beyond tool calls.
const (
	ruleNetworkAccess    = "network_access"
	ruleCodexPermissions = "codex_permissions"
)

// hostName is how an approval names a host: "*" is any host.
func hostName(h string) string {
	if h == "*" {
		return "any host"
	}
	return h
}

// clip cuts s to summaryMaxRunes.
func clip(s string) string {
	if utf8.RuneCountInString(s) > summaryMaxRunes {
		r := []rune(s)
		s = string(r[:summaryMaxRunes-1]) + "…"
	}
	return s
}

// summarize says in a line what an escalated tool call would do, with secrets in it masked.
func summarize(tool string, input json.RawMessage, reason string) string {
	return summaryOf(reason+" — "+tool, whats([]ToolCall{{Tool: tool, Input: input}}), "")
}

// summaryOf is an Approval's summary line: head, then what each call would do, then the asker's note, redacted and cut
// to summaryMaxRunes.
func summaryOf(head string, whats []string, note string) string {
	s := head
	if len(whats) > 0 {
		s += ": " + strings.Join(whats, ", ")
	}
	if note != "" {
		s += " — " + note
	}
	return clip(redact.String(s))
}

// whats are what calls would do, for a summary: each one's command, path or URL, when it names one.
func whats(calls []ToolCall) []string {
	var out []string
	for _, c := range calls {
		var in map[string]any
		_ = json.Unmarshal(c.Input, &in)
		for _, k := range []string{"command", "file_path", "notebook_path", "url", "path"} {
			if v, ok := in[k].(string); ok && v != "" {
				out = append(out, v)
				break
			}
		}
	}
	return out
}

func (n *Node) handleReport(req ReportRequest) Response {
	if n.reg != nil {
		start := time.Now()
		defer func() { n.reg.Histogram("mcp_tool_seconds", "tool", "report").Observe(time.Since(start)) }()
	}
	s := n.lookup(req.Ticket, req.Assignment)
	if s == nil {
		return Response{Error: fmt.Sprintf("%s: ticket %q assignment %q", denyNoSession, req.Ticket, req.Assignment)}
	}
	if s.reviewing() {
		return Response{Error: "a review session gives its verdict with the review tool"}
	}
	if (req.Status == string(core.ReportReady) || req.Status == string(core.ReportDropped)) && s.hasUnseenFailure() {
		s.log.Warn("report refused", "state", req.Status, "error", "tool call failed in the same step")
		return Response{Error: "a tool call of yours failed in the step that filed this report, so you cannot have seen its result; " +
			"read the result, then report again"}
	}
	if req.Pull < 0 {
		return Response{Error: "pull must be a pull request number"}
	}
	if req.Pull > 0 {
		if s.t.Repo == "" {
			return Response{Error: "ticket " + s.t.ID + " works in no GitHub repository; omit pull"}
		}
		// Bound before the report is filed: a ready report of a Ticket that follows no pull request ends it at once,
		// while one with its own open pull request waits for review.
		thread := github.ThreadKey(s.t.Repo, req.Pull)
		if err := n.engine().BindThread(req.Ticket, thread, true); err != nil {
			s.log.Warn("pull request not bound", "thread", thread, "error", err)
			return Response{Error: "binding pull request #" + strconv.Itoa(req.Pull) + ": " + err.Error()}
		}
		s.log.Info("pull request bound", "thread", thread)
	}
	err := n.engine().FileReport(req.Ticket, core.ReportInput{
		AssignmentID: req.Assignment, State: core.ReportState(req.Status), Summary: req.Summary, Question: req.Question,
		Pull: req.Pull,
	})
	if err != nil {
		s.log.Warn("report refused", "state", req.Status, "error", err)
		return Response{Error: err.Error()}
	}
	// A ready report on a Ticket that stays open (it waits for its pull request) hands the work off: the session pauses
	// once its turn is over (I1, R34). One that waits for its review ended the Assignment: the Engine stops the session.
	handoff := false
	if req.Status == string(core.ReportReady) {
		if t, _, err := n.engine().Ticket(req.Ticket); err == nil && t.HandedOff() {
			handoff = true
		}
	}
	s.noteHandoff(handoff)
	s.noteReport()
	s.log.Info("report filed", "state", req.Status)
	return Response{OK: true}
}

// callNode sends one request line to the node socket and decodes one response line into resp.
func callNode(socket string, req, resp any) error {
	if socket == "" {
		return errors.New("ATHENA_SOCKET is not set")
	}
	conn, err := net.DialTimeout("unix", socket, dialTimeout)
	if err != nil {
		return fmt.Errorf("connect %s: %w", socket, err)
	}
	defer conn.Close()
	line, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	if err := json.NewDecoder(conn).Decode(resp); err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	return nil
}
