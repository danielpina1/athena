package main

import (
	"bytes"
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
	for _, cmd := range []string{"server", "task", "status", "logs", "reply", "stop", "retry", "approvals", "approve", "deny", "hook", "mcp"} {
		var out, errb bytes.Buffer
		if code := run([]string{cmd}, &out, &errb); code != 1 {
			t.Errorf("%s: exit code = %d, want 1", cmd, code)
		}
		if want := "athena " + cmd + ": not implemented\n"; errb.String() != want {
			t.Errorf("%s: stderr = %q, want %q", cmd, errb.String(), want)
		}
	}
}
