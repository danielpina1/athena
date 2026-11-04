//go:build live

package server

// Live tests drive the real, logged-in Claude Code (haiku) through a real server. They cost a little money and need
// the network, so they run only with `ATHENA_LIVE=1 go test -tags live -run Live -v ./internal/server`.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
)

const liveOutside = "/tmp/athena-live-outside.txt"

// liveEnv skips the test unless it may run against the real CLI, and returns the claude binary.
func liveEnv(t *testing.T) string {
	t.Helper()
	if os.Getenv("ATHENA_LIVE") != "1" {
		t.Skip("set ATHENA_LIVE=1 to run against the real claude")
	}
	p, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not on PATH")
	}
	return p
}

// buildAthena builds the athena binary the hooks and the MCP server run.
func buildAthena(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "athena")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/danielpina1/athena/cmd/athena").CombinedOutput()
	if err != nil {
		t.Fatalf("build athena: %v\n%s", err, out)
	}
	return bin
}

type liveServer struct {
	*testServer
	workRoot string
}

// startLive starts a server with a temp state dir, real claude and the haiku model.
func startLive(t *testing.T) *liveServer {
	t.Helper()
	claudeBin := liveEnv(t)
	athena := buildAthena(t)
	stateDir := newStateDir(t)
	cfg := Config{
		StateDir: stateDir,
		Node: node.Config{
			ClaudeBinary:    claudeBin,
			AthenaBin:       athena,
			Model:           "haiku",
			ApprovalTimeout: 5 * time.Minute,
		},
		Stderr: io.Discard,
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &testServer{t: t, stateDir: stateDir, cancel: cancel, done: make(chan error, 1)}
	go func() { s.done <- Run(ctx, cfg) }()
	t.Cleanup(func() { stopLive(s) })
	eventually(t, "control socket answers", func() bool {
		select {
		case err := <-s.done:
			t.Fatalf("Run returned early: %v", err)
		default:
		}
		r, err := call(stateDir, Request{Op: OpPing})
		return err == nil && r.OK
	})
	return &liveServer{testServer: s, workRoot: filepath.Join(stateDir, "work")}
}

// stopLive cancels the server and waits for it: a live session may need its close grace (10 s) before it is killed.
func stopLive(s *testServer) {
	if s.stopped {
		return
	}
	s.stopped = true
	s.cancel()
	select {
	case err := <-s.done:
		if err != nil {
			s.t.Errorf("Run: %v", err)
		}
	case <-time.After(45 * time.Second):
		s.t.Error("Run did not return after cancel")
	}
}

// await polls cond until it holds or limit passes, failing with what.
func await(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s", limit, what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// dumpOnFailure prints the transcript and the server log when the test fails.
func (s *liveServer) dumpOnFailure(t *testing.T, id string) {
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, p := range []string{node.TranscriptPath(s.stateDir, id), filepath.Join(s.stateDir, LogDir, LogFile)} {
			b, _ := os.ReadFile(p)
			t.Logf("--- %s ---\n%s", p, b)
		}
	})
}

// logLines reads the Ticket's rendered log through the control socket.
func (s *liveServer) logLines(t *testing.T, id string) []LogLine {
	t.Helper()
	conn, err := net.DialTimeout("unix", filepath.Join(s.stateDir, ControlSocket), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(Request{Op: OpLogs, ID: id}); err != nil {
		t.Fatal(err)
	}
	var out []LogLine
	sc := bufio.NewScanner(conn)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var r struct {
			OK    bool    `json:"ok"`
			Error string  `json:"error"`
			Data  LogLine `json:"data"`
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("logs line: %v", err)
		}
		if !r.OK {
			t.Fatalf("logs: %s", r.Error)
		}
		out = append(out, r.Data)
	}
	return out
}

// checkConfig checks the files the node wrote for the session: hooks with the long timeout, and an MCP server that
// carries the Ticket and the Assignment.
func (s *liveServer) checkConfig(t *testing.T, dir, ticket, assignment string) {
	t.Helper()
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	b, err := os.ReadFile(filepath.Join(dir, ".athena", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"PreToolUse", "PermissionRequest"} {
		hs := settings.Hooks[ev]
		if len(hs) == 0 || len(hs[0].Hooks) == 0 || hs[0].Hooks[0].Timeout < int((5*time.Minute).Seconds()) {
			t.Fatalf("%s hook timeout is not at least the approval timeout: %s", ev, b)
		}
		t.Logf("settings: %s hook timeout %ds", ev, hs[0].Hooks[0].Timeout)
	}
	var mcp struct {
		Servers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	b, err = os.ReadFile(filepath.Join(dir, ".athena", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &mcp); err != nil {
		t.Fatal(err)
	}
	env := mcp.Servers["athena"].Env
	if env["ATHENA_TICKET"] != ticket || env["ATHENA_ASSIGNMENT"] != assignment {
		t.Fatalf("mcp env = %v, want ticket %s assignment %s", env, ticket, assignment)
	}
	t.Logf("mcp env: ticket %s assignment %s", env["ATHENA_TICKET"], env["ATHENA_ASSIGNMENT"])
}

// checkIsolation checks the transcript for the owner's own settings: none of their plugins loaded and no hook but
// Athena's ran (the owner's user settings enable plugins that have SessionStart hooks).
func (s *liveServer) checkIsolation(t *testing.T, transcript string) {
	t.Helper()
	f, err := os.Open(transcript)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 4<<20)
	var hooks []string
	var plugins []string
	for sc.Scan() {
		var ev struct {
			Type     string `json:"type"`
			Subtype  string `json:"subtype"`
			HookName string `json:"hook_name"`
			Plugins  []struct {
				Source string `json:"source"`
				Path   string `json:"path"`
			} `json:"plugins"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Type != "system" {
			continue
		}
		switch ev.Subtype {
		case "hook_started":
			hooks = append(hooks, ev.HookName)
		case "init":
			for _, p := range ev.Plugins {
				plugins = append(plugins, p.Source)
				if p.Path != "builtin" {
					t.Errorf("plugin %s loaded from %s: the owner's settings were not isolated", p.Source, p.Path)
				}
			}
		}
	}
	if len(hooks) != 1 || hooks[0] != "SessionStart:startup" {
		t.Errorf("hooks that started = %v, want only Athena's SessionStart", hooks)
	}
	t.Logf("isolation: plugins %v, hooks started %v", plugins, hooks)
}

func (s *liveServer) pendingApprovals() []ApprovalView {
	var list []ApprovalView
	s.ok(Request{Op: OpApprovals}, &list)
	return list
}

func TestLiveHelloFile(t *testing.T) {
	s := startLive(t)
	id := s.submit("Create hello.txt containing exactly `hi` in your working directory, then report ready.")
	s.dumpOnFailure(t, id)

	var d TicketDetail
	await(t, 3*time.Minute, "ticket completed", func() bool {
		d = s.detail(id)
		if ap := s.pendingApprovals(); len(ap) > 0 {
			t.Fatalf("unexpected approval: %+v", ap[0])
		}
		return d.Ended != nil
	})
	if d.Ended.Outcome != core.OutcomeCompleted {
		t.Fatalf("ticket ended %+v, want completed", d.Ended)
	}
	got, err := os.ReadFile(filepath.Join(d.Dir, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hi" {
		t.Fatalf("hello.txt = %q, want %q", got, "hi")
	}

	// The report tool reached the node with this Ticket and Assignment.
	a := d.Assignments[len(d.Assignments)-1]
	if a.Report == nil || a.Report.State != core.ReportReady {
		t.Fatalf("assignment report = %+v, want ready", a.Report)
	}
	t.Logf("ticket %s assignment %s session %s report %q: %s", id, a.ID, a.SessionID, a.Report.State, a.Report.Summary)

	// The transcript landed in the state dir and renders through logs.
	tp := node.TranscriptPath(s.stateDir, id)
	if st, err := os.Stat(tp); err != nil || st.Size() == 0 {
		t.Fatalf("transcript %s: %v", tp, err)
	}
	lines := s.logLines(t, id)
	s.checkConfig(t, d.Dir, id, a.ID)
	s.checkIsolation(t, tp)

	// The session ends when the ready report ends the Ticket, so the transcript has no final result event.
	var inits, tools int
	var sawReport bool
	for _, l := range lines {
		t.Logf("log: %s %-6s %s", l.At.Format("15:04:05"), l.Kind, l.Text)
		switch l.Kind {
		case "init":
			inits++
		case "tool":
			tools++
			sawReport = sawReport || strings.HasPrefix(l.Text, "mcp__athena__report")
		}
	}
	if inits == 0 || tools == 0 || !sawReport {
		t.Fatalf("rendered log has %d init and %d tool lines, report call seen: %v", inits, tools, sawReport)
	}
	// Hooks fired for this session: the server logged their decisions with the Ticket and Assignment.
	logb, _ := os.ReadFile(filepath.Join(s.stateDir, LogDir, LogFile))
	want := `"msg":"hook decision","ticket":"` + id + `","assignment":"` + a.ID + `","event":"PreToolUse","tool":"Write"`
	if !strings.Contains(string(logb), want) {
		t.Fatalf("server log has no PreToolUse decision for Write of %s/%s", id, a.ID)
	}
}

func TestLiveWriteOutsideEscalates(t *testing.T) {
	s := startLive(t)
	for _, p := range []string{liveOutside, "/private" + liveOutside} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		os.Remove(liveOutside)
		os.Remove("/private" + liveOutside)
	})
	id := s.submit("Write " + liveOutside + " with `x`, then report ready.")
	s.dumpOnFailure(t, id)

	var ap ApprovalView
	await(t, 2*time.Minute, "an approval", func() bool {
		if l := s.pendingApprovals(); len(l) > 0 {
			ap = l[0]
			return true
		}
		return false
	})
	t.Logf("approval %s ticket %s tool %s rule %s summary %q", ap.ID, ap.TicketID, ap.Tool, ap.Rule, ap.Summary)
	if ap.TicketID != id {
		t.Fatalf("approval is for ticket %s, want %s", ap.TicketID, id)
	}
	if d := s.detail(id); d.Approval == nil || d.Approval.ID != ap.ID {
		t.Fatalf("ticket detail does not show the approval: %+v", d.Approval)
	}
	// ATHENA_LIVE_APPROVAL_DELAY (a Go duration) holds the answer back, to show a hook outlives the CLI's default timeout.
	if d, err := time.ParseDuration(os.Getenv("ATHENA_LIVE_APPROVAL_DELAY")); err == nil {
		t.Logf("waiting %s before answering", d)
		time.Sleep(d)
	}
	s.ok(Request{Op: OpAnswer, ID: ap.ID, Approve: false, Note: "denied by the owner"}, nil)

	// The worker sees the denial and files a report of some state.
	var d TicketDetail
	// A worker that is denied may try the same write another way (it did, through Bash `echo x > file`); that
	// escalates too, and the owner denies it as well.
	nudged := false
	await(t, 3*time.Minute, "a report after the denial", func() bool {
		d = s.detail(id)
		// A worker may end its turn with plain text instead of a report (haiku did once in six runs). Athena nudges it
		// once and then reports blocked; should it still be idle without a report, the owner asks too.
		if la := d.Assignments[len(d.Assignments)-1]; !nudged && la.IdleAt != nil && la.Report == nil {
			nudged = true
			t.Log("worker went idle without a report; replying to ask for one")
			s.ok(Request{Op: OpReply, ID: id, Text: "Please file your status with the report tool."}, nil)
		}
		for _, more := range s.pendingApprovals() {
			t.Logf("also denying approval %s tool %s rule %s: %s", more.ID, more.Tool, more.Rule, more.Summary)
			s.ok(Request{Op: OpAnswer, ID: more.ID, Approve: false, Note: "denied by the owner"}, nil)
		}
		d = s.detail(id)
		a := d.Assignments[len(d.Assignments)-1]
		return a.Report != nil && a.Report.State != core.ReportProgress || d.Ended != nil
	})
	for _, p := range []string{liveOutside, "/private" + liveOutside} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s exists although the approval was denied", p)
		}
	}
	a := d.Assignments[len(d.Assignments)-1]
	if a.Report == nil {
		t.Fatalf("no report; ticket %+v", d.Ended)
	}
	t.Logf("report %s: %s", a.Report.State, a.Report.Summary)
	s.checkConfig(t, d.Dir, id, a.ID)
	sum := strings.ToLower(a.Report.Summary)
	if !strings.Contains(sum, "den") && !strings.Contains(sum, "reject") && !strings.Contains(sum, "refus") && !strings.Contains(sum, "approv") {
		t.Fatalf("report does not mention the denial: %q", a.Report.Summary)
	}
	for _, l := range s.logLines(t, id) {
		t.Logf("log: %s %-6s %s", l.At.Format("15:04:05"), l.Kind, l.Text)
	}
}

// TestLiveBashWriteOutsideEscalates: the same write through the shell (a redirection) is escalated too; a worker
// denied a Write once got the file written by `echo x > file`, which the rules then allowed.
func TestLiveBashWriteOutsideEscalates(t *testing.T) {
	s := startLive(t)
	for _, p := range []string{liveOutside, "/private" + liveOutside} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		os.Remove(liveOutside)
		os.Remove("/private" + liveOutside)
	})
	id := s.submit("Run the shell command `echo x > " + liveOutside + "` with the Bash tool, then report ready.")
	s.dumpOnFailure(t, id)
	var ap ApprovalView
	await(t, 2*time.Minute, "an approval", func() bool {
		if l := s.pendingApprovals(); len(l) > 0 {
			ap = l[0]
			return true
		}
		return false
	})
	t.Logf("approval %s tool %s rule %s summary %q", ap.ID, ap.Tool, ap.Rule, ap.Summary)
	if ap.Tool != "Bash" || ap.Rule != "write-outside-ticket" {
		t.Fatalf("approval = %+v, want a Bash write-outside-ticket escalation", ap)
	}
	s.ok(Request{Op: OpAnswer, ID: ap.ID, Approve: false, Note: "denied by the owner"}, nil)
	await(t, 3*time.Minute, "the worker to stop trying", func() bool {
		for _, more := range s.pendingApprovals() {
			t.Logf("also denying approval %s tool %s rule %s: %s", more.ID, more.Tool, more.Rule, more.Summary)
			s.ok(Request{Op: OpAnswer, ID: more.ID, Approve: false, Note: "denied by the owner"}, nil)
		}
		d := s.detail(id)
		a := d.Assignments[len(d.Assignments)-1]
		return a.IdleAt != nil || a.Report != nil || d.Ended != nil
	})
	for _, p := range []string{liveOutside, "/private" + liveOutside} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s exists although the approval was denied", p)
		}
	}
}
