package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/render"
)

// The control socket speaks JSON lines: one request line per connection, answered by one response line, except logs,
// which answers with one response line per rendered event until its end.

// Control ops.
const (
	OpPing      = "ping"
	OpSubmit    = "submit"
	OpTickets   = "tickets"
	OpTicket    = "ticket"
	OpReply     = "reply"
	OpStop      = "stop"
	OpRetry     = "retry"
	OpApprovals = "approvals"
	OpAnswer    = "answer"
	OpLogs      = "logs"
	OpMetrics   = "metrics"
	// OpHealth reports the Sources, the effects runner and the router (athena status).
	OpHealth      = "health"
	OpTriggers    = "triggers"
	OpThreads     = "threads"
	OpDecisions   = "decisions"
	OpReevaluate  = "reevaluate"
	OpDismiss     = "dismiss"
	OpUnsubscribe = "unsubscribe"
	OpPolicyShow  = "policy.show"
	OpPolicySet   = "policy.set"
	// OpPause and OpResume set and clear the kill switch (athena pause, athena resume).
	OpPause  = "pause"
	OpResume = "resume"
	// OpProposals lists the Proposals that wait on the owner, pending or failed; OpApply and OpDiscard (ID) apply or discard one.
	OpProposals = "proposals"
	OpApply     = "apply"
	OpDiscard   = "discard"
	// OpWebRevoke revokes a passkey (ID) inside the server, so its open dashboard connections close.
	OpWebRevoke = "web.revoke"
	// OpWebStatus reports the dashboard's endpoint: its URL, its certificate and the open connections.
	OpWebStatus = "web.status"
)

// Request is one control request; Op says which of its fields count.
type Request struct {
	Op      string `json:"op"`
	ID      string `json:"id,omitempty"`
	Text    string `json:"text,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Model   string `json:"model,omitempty"`
	All     bool   `json:"all,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Approve bool   `json:"approve,omitempty"`
	Note    string `json:"note,omitempty"`
	Follow  bool   `json:"follow,omitempty"`
	// Window is the period of a metrics request: "1h" (the default), "24h" or "start" for since the server started.
	Window string `json:"window,omitempty"`
	// Thread is a Thread key: the Thread of triggers, decisions and unsubscribe.
	Thread string `json:"thread,omitempty"`
	// Repo is the repository (owner/name) a submitted task works in.
	Repo string `json:"repo,omitempty"`
	// Runtime is the runtime a submitted task runs on: claude, codex or auto; empty is runtimes.default of the
	// configuration (auto unless it says otherwise).
	Runtime string `json:"runtime,omitempty"`
	// Review pairs a submitted task with a review of its ready reports (true) or with none (false); unset is the
	// Policy's pairing default.
	Review *bool `json:"review,omitempty"`
	// Policy is the settings document policy.set saves.
	Policy json.RawMessage `json:"policy,omitempty"`
}

// Response answers a request: OK with its Data, or the Error that refused it.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Data  any    `json:"data,omitempty"`
}

// TicketSummary is a row of the Ticket list.
type TicketSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Stage     string    `json:"stage"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ApprovalView is an Approval without the tool's raw input.
type ApprovalView struct {
	ID        string    `json:"id"`
	TicketID  string    `json:"ticketId"`
	Tool      string    `json:"tool,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Rule      string    `json:"rule,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// TicketDetail is one Ticket in full.
type TicketDetail struct {
	TicketSummary
	// Dir is the directory the Ticket's sessions run in; SourceDir the directory the task named, if any.
	Dir       string `json:"dir,omitempty"`
	SourceDir string `json:"sourceDir,omitempty"`
	// Repo is the GitHub repository (owner/name) the Ticket works in, if any.
	Repo    string `json:"repo,omitempty"`
	Runtime string `json:"runtime,omitempty"`
	Model   string `json:"model,omitempty"`
	// Pairing is how the Ticket's work is paired: "review", or empty for none.
	Pairing string `json:"pairing,omitempty"`
	// Question is what the Ticket asks a person, if anything.
	Question    string            `json:"question,omitempty"`
	Ended       *core.Ended       `json:"ended,omitempty"`
	Assignments []core.Assignment `json:"assignments"`
	// Undelivered counts the Inputs waiting for the worker.
	Undelivered int           `json:"undelivered"`
	Approval    *ApprovalView `json:"approval,omitempty"`
	// Stats are the Ticket's performance numbers.
	Stats core.TicketStats `json:"stats"`
}

// StageString renders a Ticket's stage: "ended:<outcome>", or the stage with its action as "stage/action".
func StageString(t *core.Ticket) string {
	if t.Ended != nil {
		return "ended:" + string(t.Ended.Outcome)
	}
	s := core.TicketStage(t)
	if s.Action != "" {
		return s.Stage + "/" + s.Action
	}
	return s.Stage
}

const (
	maxControlRequest = 1 << 20
	controlReadTime   = 30 * time.Second
	controlWriteTime  = 10 * time.Second
	logsPollInterval  = 200 * time.Millisecond
)

// control is the Dispatcher as the control socket sees it: this file frames its JSON lines, dispatch.go acts.
type control = Dispatcher

// serveConn answers one connection; ctx ends with the server.
func (c *control) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	_ = conn.SetReadDeadline(time.Now().Add(controlReadTime))
	br := bufio.NewReader(io.LimitReader(conn, maxControlRequest))
	line, err := br.ReadBytes('\n')
	if err != nil && (err != io.EOF || len(line) == 0) {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	send := func(r Response) error {
		_ = conn.SetWriteDeadline(time.Now().Add(controlWriteTime))
		return json.NewEncoder(conn).Encode(r)
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		_ = send(Response{Error: "request is not JSON"})
		return
	}
	if req.Op == OpLogs {
		// The client ends a stream by disconnecting: anything it sends or its EOF ends it.
		go func() {
			_, _ = io.Copy(io.Discard, br)
			cancel()
		}()
		if err := c.logs(ctx, req, send); err != nil {
			_ = send(errorResponse(err, c.log, req))
		}
		return
	}
	data, err := c.handleRequest(req)
	if err != nil {
		_ = send(errorResponse(err, c.log, req))
		return
	}
	_ = send(Response{OK: true, Data: data})
}

// errorResponse turns an error into a refusal: a UserError's own message, anything else a plain "internal error" whose
// detail goes only to the log.
func errorResponse(err error, log *slog.Logger, req Request) Response {
	var ue *core.UserError
	if errors.As(err, &ue) {
		return Response{Error: ue.Msg}
	}
	if log != nil {
		log.Error("control request failed", "op", req.Op, "ticket", req.ID, "error", err)
	}
	return Response{Error: "internal error"}
}

func userErrorf(format string, args ...any) error {
	return &core.UserError{Msg: fmt.Sprintf(format, args...)}
}

// parseWindow turns a metrics window name into its duration: 0 is since start.
func parseWindow(name string) (time.Duration, error) {
	switch name {
	case "", "1h":
		return time.Hour, nil
	case "24h":
		return 24 * time.Hour, nil
	case "start":
		return 0, nil
	}
	return 0, userErrorf("unknown window %q (use 1h, 24h or start)", name)
}

func summary(t *core.Ticket) TicketSummary {
	return TicketSummary{ID: t.ID, Title: t.Title, Stage: StageString(t), CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}

func approvalView(a core.Approval) ApprovalView {
	return ApprovalView{ID: a.ID, TicketID: a.TicketID, Tool: a.Tool, Summary: a.Summary, Rule: a.Rule, CreatedAt: a.CreatedAt}
}

// logs streams a Ticket's transcript, one rendered event per response line. With Follow it keeps tailing the
// transcript until the Ticket ends or ctx is done (the client went away or the server stops).
func (c *control) logs(ctx context.Context, req Request, send func(Response) error) error {
	t, _, err := c.eng.Ticket(req.ID)
	if err != nil {
		return err
	}
	// The node keeps the transcript in the state dir, so it outlives the Ticket's worktree.
	path := node.TranscriptPath(c.stateDir, t.ID)
	tl := &tail{path: path, r: render.New(t.CreatedAt)}
	defer tl.close()
	for {
		// Look whether the Ticket ended before reading, so nothing written before its end is missed.
		ended := !req.Follow
		if req.Follow {
			cur, _, err := c.eng.Ticket(req.ID)
			if err != nil {
				return err
			}
			ended = cur.Ended != nil
		}
		lines, err := tl.read()
		if err != nil {
			return err
		}
		for _, l := range lines {
			if err := send(Response{OK: true, Data: l}); err != nil {
				return nil // the client went away
			}
		}
		if ended {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(logsPollInterval):
		}
	}
}

// tail reads complete lines appended to a file that may not exist yet.
type tail struct {
	path    string
	f       *os.File
	partial []byte
	r       *render.Renderer
}

func (tl *tail) read() ([]render.LogLine, error) {
	if tl.f == nil {
		f, err := os.Open(tl.path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("open transcript: %w", err)
		}
		tl.f = f
	}
	data, err := io.ReadAll(tl.f)
	if err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}
	data = append(tl.partial, data...)
	var out []render.LogLine
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		if line := data[:i]; len(bytes.TrimSpace(line)) > 0 {
			out = append(out, tl.r.Render(line)...)
		}
		data = data[i+1:]
	}
	tl.partial = append([]byte(nil), data...)
	return out, nil
}

func (tl *tail) close() {
	if tl.f != nil {
		tl.f.Close()
	}
}
