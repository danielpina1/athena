package main

import (
	"bytes"
	"encoding/json"
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

func TestNotImplementedSubcommands(t *testing.T) {
	for _, cmd := range []string{"server", "task", "status", "logs", "reply", "stop", "retry", "approvals", "approve", "deny"} {
		var out, errb bytes.Buffer
		if code := run([]string{cmd}, &out, &errb); code != 1 {
			t.Errorf("%s: exit code = %d, want 1", cmd, code)
		}
		if want := "athena " + cmd + ": not implemented\n"; errb.String() != want {
			t.Errorf("%s: stderr = %q, want %q", cmd, errb.String(), want)
		}
	}
}

func TestHookRequiresEvent(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"hook"}, &out, &errb); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage: athena hook <event>") {
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
