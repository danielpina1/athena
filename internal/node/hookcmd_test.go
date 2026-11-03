package node

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeNode serves one JSON-lines request per connection on a Unix socket.
// reply receives the decoded request and returns the raw line to write back;
// returning "" closes the connection without answering.
type fakeNode struct {
	path string
	reqs chan HookRequest
}

func startFakeNode(t *testing.T, reply func(HookRequest) string) *fakeNode {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes on macOS; avoid t.TempDir.
	dir, err := os.MkdirTemp("/tmp", "ath")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeNode{path: filepath.Join(dir, "n.sock"), reqs: make(chan HookRequest, 8)}
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req HookRequest
				if err := json.Unmarshal(line, &req); err != nil {
					return
				}
				f.reqs <- req
				if out := reply(req); out != "" {
					c.Write([]byte(out + "\n"))
				}
			}()
		}
	}()
	return f
}

func runHook(t *testing.T, event, stdin, socket string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = RunHook(event, strings.NewReader(stdin), &out, &errb, socket)
	return code, out.String(), errb.String()
}

func fixture(t *testing.T, event string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "hooks", event+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func asMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not JSON: %q: %v", s, err)
	}
	return m
}

func setHookEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ATHENA_TICKET", "T-1")
	t.Setenv("ATHENA_ASSIGNMENT", "A-1")
}

func TestHookFailsClosed(t *testing.T) {
	setHookEnv(t)
	for _, event := range []string{"PreToolUse", "PermissionRequest"} {
		t.Run(event, func(t *testing.T) {
			code, stdout, stderr := runHook(t, event, fixture(t, event), "/nonexistent/athena.sock")
			if code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if stderr == "" {
				t.Errorf("want the connection error on stderr")
			}
			assertDeny(t, event, stdout, "athena node unreachable")
		})
	}
}

func TestHookFailsClosedNoSocketConfigured(t *testing.T) {
	setHookEnv(t)
	code, stdout, _ := runHook(t, "PreToolUse", fixture(t, "PreToolUse"), "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	assertDeny(t, "PreToolUse", stdout, "athena node unreachable")
}

func TestHookFailsClosedOnEOF(t *testing.T) {
	setHookEnv(t)
	n := startFakeNode(t, func(HookRequest) string { return "" })
	code, stdout, _ := runHook(t, "PreToolUse", fixture(t, "PreToolUse"), n.path)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	assertDeny(t, "PreToolUse", stdout, "athena node unreachable")
}

func TestHookFailsClosedOnGarbageAndEscalate(t *testing.T) {
	setHookEnv(t)
	for name, resp := range map[string]string{
		"garbage":  "not json",
		"escalate": `{"verdict":"escalate","reason":"needs human"}`,
		"unknown":  `{"verdict":"maybe"}`,
		"empty":    `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			n := startFakeNode(t, func(HookRequest) string { return resp })
			code, stdout, _ := runHook(t, "PreToolUse", fixture(t, "PreToolUse"), n.path)
			if code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			m := asMap(t, stdout)
			hso := m["hookSpecificOutput"].(map[string]any)
			if hso["permissionDecision"] != "deny" {
				t.Fatalf("decision = %v, want deny; out %s", hso["permissionDecision"], stdout)
			}
		})
	}
}

func TestHookInvalidInputDenies(t *testing.T) {
	setHookEnv(t)
	n := startFakeNode(t, func(HookRequest) string { return `{"verdict":"allow"}` })
	for _, in := range []string{"", "not json"} {
		code, stdout, _ := runHook(t, "PreToolUse", in, n.path)
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		assertDeny(t, "PreToolUse", stdout, "")
	}
	select {
	case r := <-n.reqs:
		t.Fatalf("invalid input must not reach the node: %+v", r)
	default:
	}
}

func assertDeny(t *testing.T, event, stdout, wantReason string) {
	t.Helper()
	m := asMap(t, stdout)
	hso, ok := m["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("no hookSpecificOutput in %s", stdout)
	}
	if hso["hookEventName"] != event {
		t.Errorf("hookEventName = %v, want %s", hso["hookEventName"], event)
	}
	var reason any
	switch event {
	case "PreToolUse":
		if hso["permissionDecision"] != "deny" {
			t.Fatalf("permissionDecision = %v, want deny", hso["permissionDecision"])
		}
		reason = hso["permissionDecisionReason"]
	case "PermissionRequest":
		d, _ := hso["decision"].(map[string]any)
		if d["behavior"] != "deny" {
			t.Fatalf("behavior = %v, want deny", d["behavior"])
		}
		reason = d["message"]
	}
	if wantReason != "" && reason != wantReason {
		t.Errorf("reason = %v, want %q", reason, wantReason)
	}
	if r, _ := reason.(string); r == "" {
		t.Errorf("deny must carry a reason")
	}
}

func TestHookRoundTrip(t *testing.T) {
	setHookEnv(t)
	n := startFakeNode(t, func(HookRequest) string { return `{"verdict":"allow","reason":"fine"}` })
	stdin := fixture(t, "PreToolUse")
	code, stdout, stderr := runHook(t, "PreToolUse", stdin, n.path)
	if code != 0 || stderr != "" {
		t.Fatalf("exit = %d stderr = %q", code, stderr)
	}
	want := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "allow",
		"permissionDecisionReason": "fine",
	}}
	if got := asMap(t, stdout); !jsonEqual(got, want) {
		t.Fatalf("stdout = %s, want %v", stdout, want)
	}
	req := <-n.reqs
	if req.Op != "hook" || req.Event != "PreToolUse" || req.Ticket != "T-1" || req.Assignment != "A-1" {
		t.Fatalf("request = %+v", req)
	}
	if !jsonEqual(asMap(t, string(req.Input)), asMap(t, stdin)) {
		t.Fatalf("input not forwarded verbatim: %s", req.Input)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestHookOutputPerEvent(t *testing.T) {
	setHookEnv(t)
	cases := []struct {
		name     string
		event    string
		resp     string
		wantCode int
		wantOut  any // nil means stdout must be empty
	}{
		{"pretool deny", "PreToolUse", `{"verdict":"deny","reason":"nope"}`, 0,
			map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": "nope"}}},
		{"permission allow", "PermissionRequest", `{"verdict":"allow"}`, 0,
			map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": map[string]any{"behavior": "allow"}}}},
		{"permission deny", "PermissionRequest", `{"verdict":"deny","reason":"nope"}`, 0,
			map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": map[string]any{"behavior": "deny", "message": "nope"}}}},
		{"posttool allow silent", "PostToolUse", `{"verdict":"allow"}`, 0, nil},
		{"stop allow silent", "Stop", `{"verdict":"allow"}`, 0, nil},
		{"stop deny blocks", "Stop", `{"verdict":"deny","reason":"keep going"}`, 0,
			map[string]any{"decision": "block", "reason": "keep going"}},
		{"posttool deny blocks", "PostToolUse", `{"verdict":"deny","reason":"bad output"}`, 0,
			map[string]any{"decision": "block", "reason": "bad output"}},
		{"sessionstart context", "SessionStart", `{"verdict":"allow","additionalContext":"You are ticket T-1."}`, 0,
			map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "SessionStart", "additionalContext": "You are ticket T-1."}}},
		{"sessionstart no context", "SessionStart", `{"verdict":"allow"}`, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := startFakeNode(t, func(HookRequest) string { return tc.resp })
			code, stdout, stderr := runHook(t, tc.event, fixture(t, tc.event), n.path)
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d (stderr %q)", code, tc.wantCode, stderr)
			}
			if tc.wantOut == nil {
				if stdout != "" {
					t.Fatalf("stdout = %q, want empty", stdout)
				}
				return
			}
			if !jsonEqual(asMap(t, stdout), tc.wantOut) {
				t.Fatalf("stdout = %s, want %v", stdout, tc.wantOut)
			}
		})
	}
}

func TestNonDecisionEventsSurviveSocketFailure(t *testing.T) {
	setHookEnv(t)
	for _, event := range []string{"PostToolUse", "Stop", "SessionStart"} {
		t.Run(event, func(t *testing.T) {
			code, stdout, stderr := runHook(t, event, fixture(t, event), "/nonexistent/athena.sock")
			if code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
			if stderr == "" {
				t.Fatalf("want the error on stderr")
			}
		})
	}
}

func TestHookWaitsForSlowAnswer(t *testing.T) {
	setHookEnv(t)
	n := startFakeNode(t, func(HookRequest) string {
		time.Sleep(600 * time.Millisecond) // an escalation waiting on a human
		return `{"verdict":"allow"}`
	})
	_, stdout, _ := runHook(t, "PreToolUse", fixture(t, "PreToolUse"), n.path)
	hso := asMap(t, stdout)["hookSpecificOutput"].(map[string]any)
	if hso["permissionDecision"] != "allow" {
		t.Fatalf("stdout = %s, want allow after the wait", stdout)
	}
}

func TestUnknownEvent(t *testing.T) {
	code, stdout, stderr := runHook(t, "Bogus", "{}", "")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "Bogus") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}
