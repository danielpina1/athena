package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeRouter is a RouterHandler with one valid token that records its calls.
type fakeRouter struct {
	token string
	mu    sync.Mutex
	calls []string
}

func (f *fakeRouter) ValidToken(token string) bool { return token != "" && token == f.token }

func (f *fakeRouter) CallTool(_ context.Context, tool string, args json.RawMessage) (any, error) {
	f.mu.Lock()
	f.calls = append(f.calls, tool+" "+string(args))
	f.mu.Unlock()
	if tool == "fail" {
		return nil, &toolErr{"refused here"}
	}
	return map[string]any{"tool": tool}, nil
}

type toolErr struct{ msg string }

func (e *toolErr) Error() string { return e.msg }

// routerNode serves a Node's socket with a fakeRouter as its router.
func routerNode(t *testing.T) (*Node, *fakeRouter, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "athr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "n.sock")
	n := New(Config{StateDir: t.TempDir(), SocketPath: sock, AthenaBin: "/usr/bin/true"}, nil)
	r := &fakeRouter{token: strings.Repeat("ab", 32)}
	n.SetRouter(r)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		n.Close()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		var resp Response
		if callNode(sock, PingRequest{Op: OpPing}, &resp) == nil && resp.OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket never answered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return n, r, sock
}

func TestRouterOpsNeedToken(t *testing.T) {
	_, r, sock := routerNode(t)
	for _, tok := range []string{"", "wrong", strings.Repeat("cd", 32)} {
		var resp RouterResponse
		if err := callNode(sock, RouterRequest{Op: OpRouter, Token: tok, Tool: "route", Args: json.RawMessage(`{}`)}, &resp); err != nil {
			t.Fatal(err)
		}
		if resp.OK || !strings.Contains(resp.Error, "router") {
			t.Fatalf("token %q: %+v, want refused", tok, resp)
		}
	}
	if len(r.calls) != 0 {
		t.Fatalf("calls without the token reached the router: %v", r.calls)
	}
	var resp RouterResponse
	if err := callNode(sock, RouterRequest{Op: OpRouter, Token: r.token, Tool: "route", Args: json.RawMessage(`{"a":1}`)}, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || string(resp.Result) != `{"tool":"route"}` {
		t.Fatalf("with the token: %+v", resp)
	}
	if err := callNode(sock, RouterRequest{Op: OpRouter, Token: r.token, Tool: "fail"}, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Error != "refused here" {
		t.Fatalf("a tool error: %+v", resp)
	}
	// Ticket ids authenticate nothing for the router: a worker's report op with the router's token is still refused.
	var rr Response
	if err := callNode(sock, ReportRequest{Op: OpReport, Ticket: "", Assignment: "", Status: "ready", Summary: "x"}, &rr); err != nil {
		t.Fatal(err)
	}
	if rr.OK {
		t.Fatal("a report without a session was accepted")
	}
}

func TestRouterHookUsesRouterRules(t *testing.T) {
	_, r, sock := routerNode(t)
	hook := func(event, token, tool string) HookResponse {
		t.Helper()
		in, _ := json.Marshal(map[string]any{"tool_name": tool, "tool_input": map[string]any{"command": "ls"}})
		var resp HookResponse
		if err := callNode(sock, HookRequest{Op: OpHook, Event: event, Role: RoleRouter, Token: token, Input: in}, &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if got := hook(EventPreToolUse, r.token, "Bash"); got.Verdict != VerdictDeny || !strings.Contains(got.Reason, "mcp__athena__") {
		t.Fatalf("Bash: %+v", got)
	}
	if got := hook(EventPermissionRequest, r.token, "WebFetch"); got.Verdict != VerdictDeny {
		t.Fatalf("WebFetch permission: %+v", got)
	}
	if got := hook(EventPreToolUse, r.token, "mcp__athena__route"); got.Verdict != VerdictAllow {
		t.Fatalf("route: %+v", got)
	}
	if got := hook(EventPreToolUse, "wrong", "mcp__athena__route"); got.Verdict != VerdictDeny {
		t.Fatalf("route without the token: %+v", got)
	}
	if got := hook(EventStop, "wrong", ""); got.Verdict != VerdictAllow {
		t.Fatalf("a Stop is never blocked: %+v", got)
	}
	if got := hook(EventSessionStart, r.token, ""); got.Verdict != VerdictAllow {
		t.Fatalf("session start: %+v", got)
	}
}

func TestRouterHookSendsRoleAndToken(t *testing.T) {
	f := startFakeNode(t, func(HookRequest) string { return `{"verdict":"allow"}` })
	t.Setenv("ATHENA_ROLE", RoleRouter)
	t.Setenv("ATHENA_ROUTER_TOKEN", "tok")
	t.Setenv("ATHENA_TICKET", "")
	if code, _, stderr := runHook(t, EventPreToolUse, fixture(t, EventPreToolUse), f.path); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	req := <-f.reqs
	if req.Role != RoleRouter || req.Token != "tok" {
		t.Fatalf("request = %+v", req)
	}
}

func TestRouterMCPForwardsTools(t *testing.T) {
	_, r, sock := routerNode(t)
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := newRouterMCPServer(sock, r.token).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	want := RouterTools()
	slices.Sort(names)
	slices.Sort(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "route", Arguments: map[string]any{"decisions": []any{}}})
	if err != nil || res.IsError {
		t.Fatalf("route: %v %+v", err, res)
	}
	if got := resultText(res); got != `{"tool":"route"}` {
		t.Fatalf("result = %s", got)
	}
	r.mu.Lock()
	call := r.calls[len(r.calls)-1]
	r.mu.Unlock()
	if call != `route {"decisions":[]}` {
		t.Fatalf("forwarded %q", call)
	}

	// The server with a stale token gets refusals the model can read.
	ct2, st2 := mcp.NewInMemoryTransports()
	ss2, err := newRouterMCPServer(sock, "stale").Connect(ctx, st2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss2.Close()
	cs2, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs2.Close()
	res, err = cs2.CallTool(ctx, &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"resource": "tickets"}})
	if err != nil || !res.IsError {
		t.Fatalf("stale token: %v %+v", err, res)
	}
}
