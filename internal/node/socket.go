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
	"sync"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/redact"
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
// report (a worker's Report) and ping. Every op names its Ticket and Assignment, and one that does not match a live
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
	start := time.Now()
	s := n.lookup(req.Ticket, req.Assignment)
	if req.Event == EventStop {
		// Never block a Stop: a blocking answer would loop the session.
		if s != nil {
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
		resp := n.decide(ctx, s, req.Event, req.Input, start)
		if resp.Verdict == VerdictDeny {
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

func (n *Node) ruleContext(s *session) RuleContext {
	home, _ := os.UserHomeDir()
	return RuleContext{TicketDir: s.place.workDir, Blocked: n.cfg.Blocked, Home: home, StateDir: n.cfg.StateDir, VenvActive: false}
}

// decide applies the hook rules to one tool call; an escalation becomes an Approval the call waits for.
func (n *Node) decide(ctx context.Context, s *session, event string, raw json.RawMessage, start time.Time) HookResponse {
	var in struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || in.ToolName == "" {
		n.observeHook(event, VerdictDeny, start)
		return denied("hook input names no tool")
	}
	d := Evaluate(n.ruleContext(s), in.ToolName, in.ToolInput)
	// Never log the tool input: it may name credential paths or carry secrets.
	s.log.Info("hook decision", "event", event, "tool", in.ToolName, "verdict", d.Verdict, "rule", d.Rule)
	switch d.Verdict {
	case VerdictAllow:
		n.observeHook(event, VerdictAllow, start)
		return allowed("")
	case VerdictEscalate:
		return n.escalate(ctx, s, event, in.ToolName, in.ToolInput, d, start)
	default:
		n.observeHook(event, VerdictDeny, start)
		return denied(d.Reason)
	}
}

// escalate asks a person through an Approval and waits up to ApprovalTimeout, or until the session stops, for the
// answer; no answer is a deny and the Approval expires.
func (n *Node) escalate(ctx context.Context, s *session, event, tool string, input json.RawMessage, d Decision, start time.Time) HookResponse {
	eng := n.engine()
	ap, err := eng.RequestApproval(core.Approval{
		TicketID: s.t.ID, AssignmentID: s.a.ID, Tool: tool, Rule: d.Rule, Input: input,
		Summary: summarize(tool, input, d.Reason),
	})
	n.observeHook(event, VerdictEscalate, start)
	if err != nil {
		s.log.Error("request approval", "error", err)
		return denied("approval could not be requested: " + err.Error())
	}
	s.log.Info("approval requested", "approval", ap.ID, "rule", d.Rule)
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

// summarize says in a line what an escalated tool call would do, with secrets in it masked.
func summarize(tool string, input json.RawMessage, reason string) string {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	what := tool
	for _, k := range []string{"command", "file_path", "notebook_path", "url", "path"} {
		if v, ok := in[k].(string); ok && v != "" {
			what = tool + ": " + v
			break
		}
	}
	s := redact.String(reason + " — " + what)
	if utf8.RuneCountInString(s) > summaryMaxRunes {
		r := []rune(s)
		s = string(r[:summaryMaxRunes-1]) + "…"
	}
	return s
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
	if (req.Status == string(core.ReportReady) || req.Status == string(core.ReportDropped)) && s.hasUnseenFailure() {
		s.log.Warn("report refused", "state", req.Status, "error", "tool call failed in the same step")
		return Response{Error: "a tool call of yours failed in the step that filed this report, so you cannot have seen its result; " +
			"read the result, then report again"}
	}
	err := n.engine().FileReport(req.Ticket, core.ReportInput{
		AssignmentID: req.Assignment, State: core.ReportState(req.Status), Summary: req.Summary, Question: req.Question,
	})
	if err != nil {
		s.log.Warn("report refused", "state", req.Status, "error", err)
		return Response{Error: err.Error()}
	}
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
