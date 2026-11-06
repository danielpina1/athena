package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/danielpina1/athena/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// perTicketFixture points fakeclaude's fixture at <dir>/{ticket}.jsonl and returns the path it reads for ticketID
// ("T-0001" for the first Ticket), so a test can change what the Ticket's next session replays.
func perTicketFixture(t *testing.T, ticketID string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ATHENA_FAKE_FIXTURE", filepath.Join(dir, "{ticket}.jsonl"))
	return filepath.Join(dir, ticketID+".jsonl")
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) submitPaired(text string, runtime string) core.Ticket {
	h.t.Helper()
	tk, err := h.eng.SubmitOwnerTask(text, core.TaskOptions{Runtime: runtime, Pairing: core.PairingReview})
	if err != nil {
		h.t.Fatal(err)
	}
	return tk
}

// The implementer runs on Claude Code; its ready report starts a review on Codex in the same directory, read-only: the
// sandbox is read-only, a write is denied by the read_only rule, the MCP server is the review one, and its verdict
// applies the held report.
func TestReviewRunsReadOnlyOnOtherRuntime(t *testing.T) {
	h := newCodexHarness(t, codexTurn(
		codexHook(EventPreToolUse, "echo x > notes.txt"),
		codexCommandItem("item/started", "item_0003", "echo x > notes.txt"),
		codexCommandItem("item/completed", "item_0003", "echo x > notes.txt"),
		`{"method":"item/completed","params":{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","item":{"id":"item_0004","type":"agentMessage","text":"Read-only; reviewing.","phase":"commentary"},"completedAtMs":1893456003500}}`,
		`{"fake":"mcp","tool":"review","arguments":{"verdict":"approved","findings":"- none"}}`,
	), nil)
	implFixture := perTicketFixture(t, "T-0001")
	copyFile(t, openTurnFixture(t), implFixture) // the implementer's turn stays open until it reports
	tk := h.submitPaired("Fix the login page", core.RuntimeClaude)
	_, impl := h.running(tk.ID)
	if impl.Runtime != core.RuntimeClaude {
		t.Fatalf("implementer on %q", impl.Runtime)
	}
	if r := h.report(tk.ID, impl.ID, core.ReportReady, "login fixed"); !r.OK {
		t.Fatalf("ready = %+v", r)
	}
	h.eventually("the review's verdict ends the ticket", func() bool {
		cur, _ := h.ticket(tk.ID)
		return cur.Ended != nil
	})
	cur, _ := h.ticket(tk.ID)
	if cur.Ended.Outcome != core.OutcomeCompleted {
		t.Fatalf("ended %+v", cur.Ended)
	}
	var review *core.Assignment
	for i := range cur.Assignments {
		if cur.Assignments[i].Role == core.RoleReview {
			review = &cur.Assignments[i]
		}
	}
	if review == nil || review.Runtime != core.RuntimeCodex || review.ReviewOutcome == nil ||
		review.ReviewOutcome.Verdict != core.ReviewApproved {
		t.Fatalf("review = %+v", review)
	}
	if last := cur.LatestAssignment(); last.Report == nil || last.Report.Summary != "login fixed" || last.SessionID != impl.SessionID {
		t.Fatalf("held report not applied on the implementer's session: %+v", last)
	}
	// Codex ran read-only, in the implementer's directory.
	dir := TicketDir(h.cfg.WorkRoot, tk)
	if !h.rpcHas(`"sandbox":"read-only"`) || !h.rpcHas(`"sandboxPolicy":{"type":"readOnly"}`) || h.rpcHas(`workspaceWrite`) {
		t.Fatalf("the review did not run read-only:\n%s", strings.Join(h.rpc(), "\n"))
	}
	if !h.rpcHas(`"cwd":` + string(must(json.Marshal(dir)))) {
		t.Fatalf("the review ran elsewhere than %s", dir)
	}
	// The write was denied by the read_only rule.
	denied := slices.ContainsFunc(h.rpc(), func(l string) bool {
		return strings.Contains(l, `"fake":"hook"`) && strings.Contains(l, `"denied":true`) && strings.Contains(l, ruleReadOnly)
	})
	if !denied {
		t.Fatalf("the reviewer's write was not denied read_only:\n%s", strings.Join(h.rpc(), "\n"))
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("notes.txt exists: %v", err)
	}
	// Its MCP server offers the review tool alone, and the verdict went through it.
	b, err := os.ReadFile(strings.ReplaceAll(h.argsLog, "{ticket}", tk.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `mcp_servers.athena.args=[\"mcp\",\"--ticket\",\"`+tk.ID+`\",\"--role\",\"review\"]`) {
		t.Fatalf("review session args = %s", b)
	}
	if !h.rpcHas(`Review filed (approved)`) {
		t.Fatalf("the review tool did not file the verdict:\n%s", strings.Join(h.rpc(), "\n"))
	}
	// The reviewer got its own first message and prompt: no worker preamble, no handoff note.
	if !h.rpcHas(`You are the reviewer of Athena Ticket `+tk.ID) || h.rpcHas(`You are the worker of Athena Ticket`) ||
		h.rpcHas(`You are continuing this Ticket`) || !h.rpcHas(`You are an Athena reviewer`) {
		t.Fatalf("the reviewer's first message or prompt:\n%s", strings.Join(h.rpc(), "\n"))
	}
	if tr := h.transcript(tk.ID); !strings.Contains(tr, `"role":"review","reason":"review"`) {
		t.Fatalf("no review marker in the transcript")
	}
}

// Only a review session has the review tool: its MCP server offers nothing else, a worker's offers no review, and the
// node refuses a verdict from a worker and a report or worker tool from a review.
func TestReviewToolOnlyForReviewRole(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	ctx := context.Background()

	worker := mcpClient(t, tk.ID, a.ID, h.cfg.SocketPath)
	tools, err := worker.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(tools.Tools, func(tl *mcp.Tool) bool { return tl.Name == "review" }) {
		t.Fatal("a worker's MCP server offers review")
	}
	var resp Response
	if err := callNode(h.cfg.SocketPath, ReviewRequest{Op: OpReview, Ticket: tk.ID, Assignment: a.ID, Verdict: "approved",
		Findings: "- none"}, &resp); err != nil || resp.OK || !strings.Contains(resp.Error, "only a review session") {
		t.Fatalf("a worker's verdict: %+v %v", resp, err)
	}

	// The worker reports ready on a paired Ticket: the review runs on Claude Code, the only runtime.
	p := newHarness(t, nil)
	t.Setenv("ATHENA_FAKE_FIXTURE", openTurnFixture(t))
	ptk := p.submitPaired("Paired task", "")
	_, impl := p.running(ptk.ID)
	if r := p.report(ptk.ID, impl.ID, core.ReportReady, "done"); !r.OK {
		t.Fatalf("ready = %+v", r)
	}
	var rv core.Assignment
	p.eventually("the review runs", func() bool {
		cur, _ := p.ticket(ptk.ID)
		rv = *cur.LatestAssignment()
		return rv.Role == core.RoleReview && rv.State == core.AssignmentRunning && p.n.lookup(ptk.ID, rv.ID) != nil
	})
	// Claude Code's review session gets the review server, and starts without the tools that write files.
	mcpJSON, _ := os.ReadFile(filepath.Join(TicketDir(p.cfg.WorkRoot, ptk), ".athena", "mcp.json"))
	if !strings.Contains(string(mcpJSON), `"review"`) {
		t.Fatalf("review mcp.json = %s", mcpJSON)
	}
	p.eventually("the review's process started", func() bool {
		inv := p.invocations()
		return len(inv) >= 2 && slices.Contains(inv[len(inv)-1].Args, "--disallowedTools")
	})

	cs := connect(t, newReviewMCPServer(ptk.ID, rv.ID, p.cfg.SocketPath))
	tools, err = cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "review" {
		t.Fatalf("review tools = %+v", tools.Tools)
	}
	if r := p.report(ptk.ID, rv.ID, core.ReportProgress, "x"); r.OK || !strings.Contains(r.Error, "review tool") {
		t.Fatalf("a review's report: %+v", r)
	}
	var wr WorkerResponse
	if err := callNode(p.cfg.SocketPath, WorkerRequest{Op: OpWorker, Ticket: ptk.ID, Assignment: rv.ID, Tool: "follow_thread",
		Args: json.RawMessage(`{"thread":"o/r#1"}`)}, &wr); err != nil || wr.OK {
		t.Fatalf("a review's worker tool: %+v %v", wr, err)
	}
	// An inconsistent verdict comes back as the tool's error, for the reviewer to correct; a consistent one is filed.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "review", Arguments: map[string]any{"verdict": "approved",
		"findings": "- [high] a.go:1 — broken"}})
	if err != nil || !res.IsError || !strings.Contains(resultText(res), "changes") {
		t.Fatalf("inconsistent verdict: %v %+v", err, res)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "review", Arguments: map[string]any{"verdict": "changes",
		"findings": "- [high] a.go:1 — broken"}})
	if err != nil || res.IsError {
		t.Fatalf("verdict: %v %s", err, resultText(res))
	}
	cur, _ := p.ticket(ptk.ID)
	next := cur.LatestAssignment()
	if next.Role != "" || cur.Inputs[len(cur.Inputs)-1].Kind != core.InputReview {
		t.Fatalf("changes did not go back to the implementer: %+v", next)
	}
	// The implementer resumes its own session with the findings.
	p.eventually("the implementer resumes with the findings", func() bool {
		return strings.Contains(p.stdin(), "[input "+cur.Inputs[len(cur.Inputs)-1].ID+": review]")
	})
	inv := p.invocations()
	if v, _ := argValue(inv[len(inv)-1].Args, "--resume"); v != impl.SessionID {
		t.Fatalf("resumed %q, want the implementer's session %q", v, impl.SessionID)
	}
}

// connect is an in-memory MCP client of server s.
func connect(t *testing.T, s *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// A review whose turn ends without a verdict is nudged once; the next such turn is a review failure, which applies the
// held report: the Ticket never waits for a person.
func TestReviewNudgeThenFailure(t *testing.T) {
	h := newHarness(t, nil)
	fixture := perTicketFixture(t, "T-0001")
	copyFile(t, openTurnFixture(t), fixture)
	t.Setenv("ATHENA_FAKE_LATER_FIXTURE", fixtureLines(t, "simple-turn.jsonl", func(_ int, l string) bool {
		return strings.Contains(l, `"type":"result"`)
	}))
	tk := h.submitPaired("Paired task", "")
	_, impl := h.running(tk.ID)
	// The review's turn ends without calling review.
	copyFile(t, claudeFixture(t, "simple-turn.jsonl"), fixture)
	if r := h.report(tk.ID, impl.ID, core.ReportReady, "done"); !r.OK {
		t.Fatalf("ready = %+v", r)
	}
	h.eventually("the review fails and the held report applies", func() bool {
		cur, _ := h.ticket(tk.ID)
		return cur.Ended != nil
	})
	cur, _ := h.ticket(tk.ID)
	if cur.Ended.Outcome != core.OutcomeCompleted {
		t.Fatalf("ended %+v", cur.Ended)
	}
	var review core.Assignment
	for _, a := range cur.Assignments {
		if a.Role == core.RoleReview {
			review = a
		}
	}
	if review.ReviewOutcome == nil || review.ReviewOutcome.Verdict != core.ReviewFailed || review.Report != nil {
		t.Fatalf("review = %+v", review)
	}
	if n := strings.Count(h.stdin(), reviewNudgeText); n != 1 {
		t.Fatalf("nudged %d times, want 1:\n%s", n, h.stdin())
	}
	if strings.Contains(h.stdin(), nudgeText) {
		t.Fatal("the review got the worker's nudge")
	}
}
