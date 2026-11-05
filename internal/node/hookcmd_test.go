package node

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
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

func codexFixture(t *testing.T, event string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "hooks", "codex", event+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// runHookMain runs `athena hook <args...>` as the athena binary does.
func runHookMain(t *testing.T, args []string, stdin, socket string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = HookMain(args, strings.NewReader(stdin), &out, &errb, socket)
	return code, out.String(), errb.String()
}

func TestHookRuntimeFlagDefaultsToClaude(t *testing.T) {
	setHookEnv(t)
	n := startFakeNode(t, func(HookRequest) string { return `{"verdict":"allow"}` })
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"PreToolUse"}, "claude"},
		{[]string{"--runtime", "claude", "PreToolUse"}, "claude"},
		{[]string{"--runtime", "codex", "PreToolUse"}, "codex"},
		{[]string{"--runtime=codex", "PreToolUse"}, "codex"},
	} {
		code, _, stderr := runHookMain(t, tc.args, codexFixture(t, "PreToolUse"), n.path)
		if code != 0 {
			t.Fatalf("%v: exit %d (%s)", tc.args, code, stderr)
		}
		if req := <-n.reqs; req.Runtime != tc.want || req.Event != EventPreToolUse {
			t.Fatalf("%v: request %+v, want runtime %q", tc.args, req, tc.want)
		}
	}
	for _, bad := range [][]string{{"--runtime", "gemini", "PreToolUse"}, {"--runtime", "codex"}, {}, {"PreToolUse", "extra"}} {
		if code, _, _ := runHookMain(t, bad, "{}", n.path); code != 2 {
			t.Errorf("%v: exit %d, want 2 (usage)", bad, code)
		}
	}
	// Claude Code's settings name no runtime: its output is unchanged, an allow included.
	code, stdout, _ := runHookMain(t, []string{"PreToolUse"}, fixture(t, "PreToolUse"), n.path)
	<-n.reqs
	want := map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "allow"}}
	if code != 0 || !jsonEqual(asMap(t, stdout), want) {
		t.Fatalf("claude allow = %d %s, want %v", code, stdout, want)
	}
}

// codexPreToolUseOutput is pre-tool-use.command.output (codex-cli 0.160.1) as far as Athena writes it: unknown fields
// fail the decode, as additionalProperties false fails Codex's check.
type codexPreToolUseOutput struct {
	HookSpecificOutput *struct {
		HookEventName            string  `json:"hookEventName"`
		PermissionDecision       *string `json:"permissionDecision"`
		PermissionDecisionReason *string `json:"permissionDecisionReason"`
		AdditionalContext        *string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

func strictDecode(t *testing.T, s string, v any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("output %q does not fit the schema: %v", s, err)
	}
}

func TestCodexDenyHasReason(t *testing.T) {
	setHookEnv(t)
	// Codex 0.160.1 rejects a PreToolUse deny without a non-empty permissionDecisionReason, and any allow or ask.
	for _, tc := range []struct {
		name, resp, socket, reason string
	}{
		{"deny with a reason", `{"verdict":"deny","reason":"reading credential files is not allowed"}`, "", "reading credential files is not allowed"},
		{"deny without one", `{"verdict":"deny"}`, "", "denied by athena"},
		{"node unreachable", "", "/nonexistent/athena.sock", denyUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := tc.socket
			if socket == "" {
				socket = startFakeNode(t, func(HookRequest) string { return tc.resp }).path
			}
			code, stdout, _ := runHookMain(t, []string{"--runtime", "codex", "PreToolUse"}, codexFixture(t, "PreToolUse"), socket)
			var out codexPreToolUseOutput
			strictDecode(t, stdout, &out)
			h := out.HookSpecificOutput
			if code != 0 || h == nil || h.HookEventName != "PreToolUse" || h.PermissionDecision == nil || *h.PermissionDecision != "deny" ||
				h.PermissionDecisionReason == nil || *h.PermissionDecisionReason != tc.reason {
				t.Fatalf("exit %d, output %s, want a deny with reason %q", code, stdout, tc.reason)
			}
		})
	}
	// An allow prints nothing: Codex goes on as without the hook.
	n := startFakeNode(t, func(HookRequest) string { return `{"verdict":"allow"}` })
	if code, stdout, _ := runHookMain(t, []string{"--runtime", "codex", "PreToolUse"}, codexFixture(t, "PreToolUse"), n.path); code != 0 || stdout != "" {
		t.Fatalf("codex allow: exit %d, stdout %q, want nothing", code, stdout)
	}
	// The pinned schema still has the fields the output uses, and the decision values.
	schema := readSchema(t, "pre-tool-use.command.output.json")
	def := schema.Definitions["PreToolUseHookSpecificOutputWire"]
	for _, f := range []string{"hookEventName", "permissionDecision", "permissionDecisionReason"} {
		if _, ok := def.Properties[f]; !ok {
			t.Errorf("pre-tool-use schema lost %s", f)
		}
	}
	if e := schema.Definitions["PreToolUsePermissionDecisionWire"].Enum; !slices.Contains(e, "deny") {
		t.Errorf("permissionDecision values = %v, want deny among them", e)
	}
}

// hookSchema is the part of a pinned hook output schema the tests read.
type hookSchema struct {
	Title                string                     `json:"title"`
	AdditionalProperties bool                       `json:"additionalProperties"`
	Properties           map[string]json.RawMessage `json:"properties"`
	Definitions          map[string]struct {
		AdditionalProperties bool                       `json:"additionalProperties"`
		Properties           map[string]json.RawMessage `json:"properties"`
		Required             []string                   `json:"required"`
		Enum                 []string                   `json:"enum"`
	} `json:"definitions"`
}

func readSchema(t *testing.T, name string) hookSchema {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "hooks", "codex", name))
	if err != nil {
		t.Fatal(err)
	}
	var s hookSchema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCodexPermissionRequestOutputGolden(t *testing.T) {
	setHookEnv(t)
	// permission-request.command.output of codex-cli 0.160.1, pinned: what the output may hold.
	schema := readSchema(t, "permission-request.command.output.json")
	if schema.Title != "permission-request.command.output" || schema.AdditionalProperties {
		t.Fatalf("schema = %q additionalProperties %v", schema.Title, schema.AdditionalProperties)
	}
	keys := func(m map[string]json.RawMessage) string {
		var k []string
		for name := range m {
			k = append(k, name)
		}
		slices.Sort(k)
		return strings.Join(k, ",")
	}
	if got := keys(schema.Properties); got != "continue,hookSpecificOutput,stopReason,suppressOutput,systemMessage" {
		t.Errorf("top-level properties = %s", got)
	}
	if got := keys(schema.Definitions["PermissionRequestHookSpecificOutputWire"].Properties); got != "decision,hookEventName" {
		t.Errorf("hookSpecificOutput properties = %s", got)
	}
	dec := schema.Definitions["PermissionRequestDecisionWire"]
	if got := keys(dec.Properties); got != "behavior,interrupt,message,updatedInput,updatedPermissions" || strings.Join(dec.Required, ",") != "behavior" {
		t.Errorf("decision properties = %s, required %v", got, dec.Required)
	}
	if e := schema.Definitions["PermissionRequestBehaviorWire"].Enum; strings.Join(e, ",") != "allow,deny" {
		t.Errorf("behavior values = %v", e)
	}

	for _, tc := range []struct {
		name, resp, golden string
	}{
		{"allow", `{"verdict":"allow","reason":"approved"}`,
			`{"hookSpecificOutput":{"decision":{"behavior":"allow"},"hookEventName":"PermissionRequest"}}`},
		{"deny", `{"verdict":"deny","reason":"denied by the owner: not now"}`,
			`{"hookSpecificOutput":{"decision":{"behavior":"deny","message":"denied by the owner: not now"},"hookEventName":"PermissionRequest"}}`},
		{"deny without a reason", `{"verdict":"deny"}`,
			`{"hookSpecificOutput":{"decision":{"behavior":"deny","message":"denied by athena"},"hookEventName":"PermissionRequest"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := startFakeNode(t, func(HookRequest) string { return tc.resp })
			code, stdout, _ := runHookMain(t, []string{"--runtime", "codex", "PermissionRequest"}, codexFixture(t, "PermissionRequest"), n.path)
			if code != 0 || strings.TrimSpace(stdout) != tc.golden {
				t.Fatalf("exit %d, output\n%s\nwant\n%s", code, stdout, tc.golden)
			}
			if req := <-n.reqs; req.Runtime != "codex" || req.Event != EventPermissionRequest {
				t.Fatalf("request %+v", req)
			}
			var out struct {
				HookSpecificOutput *struct {
					HookEventName string `json:"hookEventName"`
					Decision      *struct {
						Behavior string  `json:"behavior"`
						Message  *string `json:"message"`
					} `json:"decision"`
				} `json:"hookSpecificOutput"`
			}
			strictDecode(t, stdout, &out)
			if h := out.HookSpecificOutput; h == nil || h.HookEventName != "PermissionRequest" || h.Decision == nil || !slices.Contains([]string{"allow", "deny"}, h.Decision.Behavior) {
				t.Fatalf("output %s does not follow the schema", stdout)
			}
		})
	}
}
