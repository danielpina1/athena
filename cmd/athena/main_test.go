package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, errb.String())
	}
	if got, want := out.String(), "athena dev\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"nope"}, &out, &errb); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), `unknown command "nope"`) {
		t.Fatalf("stderr = %q, want unknown command message", errb.String())
	}
}

func TestNoArgs(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage: athena") {
		t.Fatalf("stderr = %q, want usage", errb.String())
	}
}

func TestOwnerCommandsWired(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "athena.sock")
	for _, cmd := range []string{"task", "status", "logs", "reply", "stop", "retry", "approvals", "approve", "deny", "metrics"} {
		args := []string{cmd, "--state-dir", dir}
		switch cmd {
		case "task", "logs", "stop", "retry", "approve", "deny":
			args = append(args, "x")
		case "reply":
			args = append(args, "T-1", "hi")
		}
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 1 {
			t.Errorf("%s: exit code = %d, want 1 (stderr %q)", cmd, code, errb.String())
		}
		if want := "athena server is not running (no " + sock + ")\n"; errb.String() != want {
			t.Errorf("%s: stderr = %q, want %q", cmd, errb.String(), want)
		}
	}
}

func TestServerUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"server", "--state-dir", t.TempDir(), "extra"}, &out, &errb); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage: athena server") {
		t.Fatalf("stderr = %q, want usage", errb.String())
	}
}

func TestHookRequiresEvent(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"hook"}, &out, &errb); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage: athena hook [--runtime claude|codex] <event>") {
		t.Fatalf("stderr = %q, want usage", errb.String())
	}
}

func TestHookWiredAndFailsClosed(t *testing.T) {
	t.Setenv("ATHENA_SOCKET", "/nonexistent/athena.sock")
	old := stdin
	stdin = strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`)
	t.Cleanup(func() { stdin = old })

	var out, errb bytes.Buffer
	if code := run([]string{"hook", "PreToolUse"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, errb.String())
	}
	var got struct {
		HookSpecificOutput struct {
			PermissionDecision string `json:"permissionDecision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout %q is not JSON: %v", out.String(), err)
	}
	if got.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("stdout = %s, want deny", out.String())
	}
}

func TestMCPRequiresTicket(t *testing.T) {
	for _, args := range [][]string{{"mcp"}, {"mcp", "--ticket", ""}, {"mcp", "--ticket", "T-1", "extra"}} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 {
			t.Errorf("%v: exit code = %d, want 2", args, code)
		}
		if !strings.Contains(errb.String(), "usage: athena mcp --ticket <id>") && !strings.Contains(errb.String(), "flag") {
			t.Errorf("%v: stderr = %q, want usage", args, errb.String())
		}
	}
}

func TestMCPRunsServerForTicket(t *testing.T) {
	t.Setenv("ATHENA_SOCKET", "/tmp/athena-test.sock")
	var gotTicket, gotSocket string
	orig := runMCPServer
	runMCPServer = func(ticket, socket string) error { gotTicket, gotSocket = ticket, socket; return nil }
	t.Cleanup(func() { runMCPServer = orig })
	var out, errb bytes.Buffer
	if code := run([]string{"mcp", "--ticket", "T-0007"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d (stderr %q)", code, errb.String())
	}
	if gotTicket != "T-0007" || gotSocket != "/tmp/athena-test.sock" {
		t.Fatalf("RunMCP(%q, %q)", gotTicket, gotSocket)
	}
}

func TestMCPRunsReviewServer(t *testing.T) {
	t.Setenv("ATHENA_SOCKET", "/tmp/athena-test.sock")
	var gotTicket string
	orig, origWorker := runReviewMCPServer, runMCPServer
	runReviewMCPServer = func(ticket, socket string) error { gotTicket = ticket; return nil }
	runMCPServer = func(string, string) error { t.Error("the worker's server ran for a review"); return nil }
	t.Cleanup(func() { runReviewMCPServer, runMCPServer = orig, origWorker })
	var out, errb bytes.Buffer
	if code := run([]string{"mcp", "--ticket", "T-0007", "--role", "review"}, &out, &errb); code != 0 || gotTicket != "T-0007" {
		t.Fatalf("exit code = %d, ticket %q (stderr %q)", code, gotTicket, errb.String())
	}
	if code := run([]string{"mcp", "--role", "review"}, &out, &errb); code != 2 {
		t.Fatalf("a review server without a ticket: exit %d", code)
	}
}

func TestMCPRunsRouterServer(t *testing.T) {
	t.Setenv("ATHENA_SOCKET", "/tmp/athena-test.sock")
	t.Setenv("ATHENA_ROUTER_TOKEN", "tok")
	var gotSocket, gotToken string
	orig := runRouterMCPServer
	runRouterMCPServer = func(socket, token string) error { gotSocket, gotToken = socket, token; return nil }
	t.Cleanup(func() { runRouterMCPServer = orig })
	var out, errb bytes.Buffer
	if code := run([]string{"mcp", "--role", "router"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d (stderr %q)", code, errb.String())
	}
	if gotSocket != "/tmp/athena-test.sock" || gotToken != "tok" {
		t.Fatalf("RunRouterMCP(%q, %q)", gotSocket, gotToken)
	}
	for _, args := range [][]string{{"mcp", "--role", "worker"}, {"mcp", "--role", "router", "--ticket", "T-1"}} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 {
			t.Errorf("%v: exit code = %d, want 2", args, code)
		}
	}
}

func TestCredentialCommandsWired(t *testing.T) {
	stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	t.Cleanup(func() { stdin = os.Stdin })
	var out, errb bytes.Buffer
	if code := run([]string{"git-credential", "store"}, &out, &errb); code != 0 || out.Len() != 0 {
		t.Fatalf("git-credential store: exit %d, stdout %q", code, out.String())
	}
	out.Reset()
	if code := run([]string{"gh", "auth", "token"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "gh auth is not available") {
		t.Fatalf("gh auth token: exit %d, stderr %q", code, errb.String())
	}
}

// The node commands and retry-elsewhere reach the CLI: an unjoined node says so, and a usage error is the CLI's.
func TestNodeCommandsDispatched(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"node", "run", "--state", filepath.Join(t.TempDir(), "n")}, &out, &errb); code != 1 ||
		errb.String() != "This machine has not joined yet; run athena node join first.\n" {
		t.Fatalf("node run: exit %d, stderr %q", code, errb.String())
	}
	errb.Reset()
	if code := run([]string{"retry-elsewhere"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "athena retry-elsewhere <id>") {
		t.Fatalf("retry-elsewhere: exit %d, stderr %q", code, errb.String())
	}
}
