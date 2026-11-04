package node

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/danielpina1/athena/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpClient connects an in-memory MCP client to the report server of one session.
func mcpClient(t *testing.T, ticket, assignment, socket string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := newMCPServer(ticket, assignment, socket).Connect(ctx, st, nil)
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

func resultText(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestReportToolEndsTicket(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	cs := mcpClient(t, tk.ID, a.ID, h.cfg.SocketPath)
	ctx := context.Background()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != len(WorkerTools()) || !slices.ContainsFunc(tools.Tools, func(tl *mcp.Tool) bool { return tl.Name == "report" }) {
		t.Fatalf("tools = %+v, want %v", tools.Tools, WorkerTools())
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "report", Arguments: map[string]any{"status": "progress", "summary": "half way", "question": "which color?"}})
	if err != nil || res.IsError {
		t.Fatalf("progress report: %v %s", err, resultText(res))
	}
	tk, st := h.ticket(tk.ID)
	if tk.Attention == nil || tk.Attention.Question != "which color?" || st != (core.Stage{Stage: "person", Action: "answer"}) {
		t.Fatalf("question not recorded: %+v %+v", tk.Attention, st)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "report", Arguments: map[string]any{"status": "ready", "summary": "done"}})
	if err != nil || res.IsError {
		t.Fatalf("ready report: %v %s", err, resultText(res))
	}
	tk, _ = h.ticket(tk.ID)
	if tk.Ended == nil || tk.Ended.Outcome != core.OutcomeCompleted {
		t.Fatalf("ticket not completed: %+v", tk.Ended)
	}
	h.eventually("session gone", func() bool { return h.n.liveCount() == 0 })
}

func TestReportToolRejects(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	ctx := context.Background()

	cs := mcpClient(t, tk.ID, a.ID, h.cfg.SocketPath)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "report", Arguments: map[string]any{"status": "bogus", "summary": "x"}})
	if err == nil && !res.IsError {
		t.Fatal("a report with an unknown status was accepted")
	}

	stranger := mcpClient(t, tk.ID, "not-the-assignment", h.cfg.SocketPath)
	res, err = stranger.CallTool(ctx, &mcp.CallToolParams{Name: "report", Arguments: map[string]any{"status": "ready", "summary": "x"}})
	if err != nil || !res.IsError {
		t.Fatalf("a report from a stranger: %v %+v, want a tool error", err, res)
	}
	tk, _ = h.ticket(tk.ID)
	if tk.Ended != nil {
		t.Fatal("a stranger's report ended the ticket")
	}

	gone := mcpClient(t, tk.ID, a.ID, "/tmp/athena-no-such.sock")
	res, err = gone.CallTool(ctx, &mcp.CallToolParams{Name: "report", Arguments: map[string]any{"status": "progress", "summary": "x"}})
	if err != nil || !res.IsError || !strings.Contains(resultText(res), "node") {
		t.Fatalf("report with the node unreachable: %v %+v, want a tool error", err, res)
	}
}
