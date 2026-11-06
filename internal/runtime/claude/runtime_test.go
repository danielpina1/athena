package claude

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/runtime/rt"
)

// TestRuntimeSessionYieldsRTEvents: a session the Runtime starts delivers the node's events, appends every line to the
// caller's transcript (with the offsets OnLine reports) and leaves the transcript open for the caller to close.
func TestRuntimeSessionYieldsRTEvents(t *testing.T) {
	dir := t.TempDir()
	r := Runtime{Binary: fakeBinary(t)}
	if err := r.Prepare(rt.Prepare{WorkDir: dir, AthenaBin: "/bin/athena", Socket: "/run/a.sock", TicketID: "T-1", AssignmentID: "A-1"}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var offs []int64
	trPath := filepath.Join(t.TempDir(), "transcripts", "T-1.jsonl")
	tr, err := rt.OpenTranscript(trPath, func(off int64, _ []byte) {
		mu.Lock()
		offs = append(offs, off)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	argsLog := filepath.Join(t.TempDir(), "args.json")
	s, err := r.Start(context.Background(), rt.StartOptions{Dir: dir, Model: "haiku", Transcript: tr,
		Env: []string{"ATHENA_FAKE_FIXTURE=" + fixtureAbs(t, "simple-turn.jsonl"), "ATHENA_FAKE_ARGS_LOG=" + argsLog}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send("make a file", "u-1"); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	deadline := time.After(20 * time.Second)
	for done := false; !done; {
		select {
		case ev := <-s.Events():
			switch e := ev.(type) {
			case rt.Init:
				kinds = append(kinds, "init")
			case rt.UserEcho:
				kinds = append(kinds, "echo:"+e.UUID)
			case rt.QuotaUpdate:
				kinds = append(kinds, "quota")
			case rt.TurnEnd:
				kinds = append(kinds, "end")
				done = e.OK
			}
		case <-deadline:
			t.Fatalf("no TurnEnd; saw %v", kinds)
		}
	}
	_ = s.Close()
	for range s.Events() {
	}
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(kinds, ","); !strings.Contains(got, "init") || !strings.Contains(got, "echo:u-1") ||
		!strings.Contains(got, "quota") || !strings.HasSuffix(got, "end") {
		t.Fatalf("events = %s", got)
	}
	// The transcript is still the caller's: open until it closes it.
	at := tr.Append([]byte("{\"after\":1}\n"))
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	file, _ := os.ReadFile(trPath)
	if !bytes.HasSuffix(file, []byte("{\"after\":1}\n")) || at != int64(len(file)-len("{\"after\":1}\n")) {
		t.Fatalf("append after Wait landed at %d of %d", at, len(file))
	}
	mu.Lock()
	if len(offs) < 2 || offs[0] != 0 {
		t.Fatalf("offsets = %v", offs)
	}
	mu.Unlock()
	// The worker's flags: Athena's settings and MCP config from Prepare, echoes on.
	args, _ := os.ReadFile(argsLog)
	for _, want := range []string{filepath.Join(dir, ".athena", "settings.json"), filepath.Join(dir, ".athena", "mcp.json"), "--replay-user-messages", "haiku"} {
		if !bytes.Contains(args, []byte(want)) {
			t.Errorf("args lack %q: %s", want, args)
		}
	}
}

// Prepare writes exactly what WriteSettingsWith and WriteMCPConfig write.
func TestRuntimePrepareWritesSettingsAndMCP(t *testing.T) {
	dir, ref := t.TempDir(), t.TempDir()
	sb := &rt.Sandbox{Required: true, AllowWrite: []string{dir}, UnixSockets: []string{"/run/a.sock"}}
	p := rt.Prepare{WorkDir: dir, AthenaBin: "/bin/athena", Socket: "/run/a.sock", TicketID: "T-1", AssignmentID: "A-1",
		HookTimeout: time.Hour, Sandbox: sb, SandboxEnv: []string{"SSL_CERT_FILE=/ca.pem"}}
	if err := (Runtime{}).Prepare(p); err != nil {
		t.Fatal(err)
	}
	if err := WriteSettingsWith(filepath.Join(ref, "settings.json"), "/bin/athena", "/run/a.sock", time.Hour, sb, "SSL_CERT_FILE=/ca.pem"); err != nil {
		t.Fatal(err)
	}
	if err := WriteMCPConfig(filepath.Join(ref, "mcp.json"), "/bin/athena", "/run/a.sock", "T-1", "A-1"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"settings.json", "mcp.json"} {
		got, err := os.ReadFile(filepath.Join(dir, ".athena", f))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := os.ReadFile(filepath.Join(ref, f))
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs:\n%s\nwant\n%s", f, got, want)
		}
	}
}

func TestRuntimeStartRefuses(t *testing.T) {
	tr, err := rt.OpenTranscript(filepath.Join(t.TempDir(), "t.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	r := Runtime{Binary: fakeBinary(t)}
	if _, err := r.Start(context.Background(), rt.StartOptions{Dir: t.TempDir(), Transcript: tr, ReadOnly: true}); err == nil {
		t.Fatal("a read-only session started writable")
	}
	if _, err := r.Start(context.Background(), rt.StartOptions{Dir: t.TempDir()}); err == nil {
		t.Fatal("a session without a transcript started")
	}
	// Unprepared: no settings, so no hooks.
	if _, err := r.Start(context.Background(), rt.StartOptions{Dir: t.TempDir(), Transcript: tr}); err == nil {
		t.Fatal("a session without Athena's settings started")
	}
}

func TestRuntimeCheck(t *testing.T) {
	script := func(out string) string {
		p := filepath.Join(t.TempDir(), "claude")
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho '"+out+"'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, c := range map[string]struct {
		bin                      string
		installed, ready, tested bool
		version                  string
	}{
		"verified": {script(VerifiedVersion + " (Claude Code)"), true, true, true, VerifiedVersion},
		"older":    {script("2.0.9 (Claude Code)"), true, true, true, "2.0.9"},
		"newer":    {script("2.2.0 (Claude Code)"), true, true, false, "2.2.0"},
		"odd":      {script("nightly"), true, true, false, "nightly"},
		"missing":  {filepath.Join(t.TempDir(), "claude"), false, false, false, ""},
	} {
		got := Runtime{Binary: c.bin}.Check(context.Background())
		if got.Installed != c.installed || got.Ready != c.ready || got.Tested != c.tested || got.Version != c.version {
			t.Errorf("%s: %+v", name, got)
		}
		if !c.ready && got.Why == "" {
			t.Errorf("%s: not ready without a why", name)
		}
	}
	if (Runtime{}).Name() != "claude" {
		t.Fatal("name")
	}
}

// A read-only session (a review): Prepare gives its MCP server only the review tool, and Start runs it without the
// tools that write files; a directory prepared for a worker never starts read-only.
func TestRuntimeReadOnlyReview(t *testing.T) {
	dir := t.TempDir()
	r := Runtime{Binary: fakeBinary(t)}
	p := rt.Prepare{WorkDir: dir, AthenaBin: "/bin/athena", Socket: "/run/a.sock", TicketID: "T-1", AssignmentID: "A-2", Role: "review", ReadOnly: true}
	if err := r.Prepare(p); err != nil {
		t.Fatal(err)
	}
	mcp, _ := os.ReadFile(filepath.Join(dir, ".athena", "mcp.json"))
	if !bytes.Contains(mcp, []byte(`"--ticket",
        "T-1",
        "--role",
        "review"`)) {
		t.Fatalf("review mcp.json = %s", mcp)
	}
	tr, err := rt.OpenTranscript(filepath.Join(t.TempDir(), "t.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	argsLog := filepath.Join(t.TempDir(), "args.json")
	s, err := r.Start(context.Background(), rt.StartOptions{Dir: dir, Transcript: tr, Role: "review", ReadOnly: true,
		Env: []string{"ATHENA_FAKE_ARGS_LOG=" + argsLog}})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	for range s.Events() {
	}
	_ = s.Wait()
	args, _ := os.ReadFile(argsLog)
	if !bytes.Contains(args, []byte(`"--disallowedTools","Write,Edit,MultiEdit,NotebookEdit"`)) {
		t.Fatalf("read-only args = %s", args)
	}

	// The worker's files of the same directory: a read-only start is refused.
	p.ReadOnly, p.Role = false, "worker"
	if err := r.Prepare(p); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(context.Background(), rt.StartOptions{Dir: dir, Transcript: tr, ReadOnly: true}); err == nil {
		t.Fatal("a read-only session started with a worker's files")
	}
}
