package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/runtime/codex"
)

func TestCodexConfigDefaultsAndLoads(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The product default: Codex enabled, "codex" from PATH, its model left to Codex.
	if cx := c.Runtimes.Codex; !cx.On() || cx.Binary != "codex" || cx.Model != "" {
		t.Fatalf("default codex = %+v", cx)
	}
	r := CodexRuntime(dir, c, "/opt/athena", "")
	if r.Home != filepath.Join(dir, "codex") || r.Binary != "codex" || r.AthenaBin != "/opt/athena" ||
		r.Socket != filepath.Join(dir, NodeSocket) || r.WorkRoot != filepath.Join(dir, "work") || r.KeepHome {
		t.Fatalf("runtime = %+v", r)
	}
	if r := CodexRuntime(dir, c, "/opt/athena", "/bin/fakecodex"); r.Binary != "/bin/fakecodex" {
		t.Fatalf("--codex: %+v", r)
	}

	writeConfigFile(t, dir, `{"runtimes": {"codex": {"enabled": false, "binary": "/usr/local/bin/codex", "model": "gpt-5-codex"}}}`)
	if c, err = LoadConfig(dir); err != nil {
		t.Fatal(err)
	}
	if cx := c.Runtimes.Codex; cx.On() || cx.Binary != "/usr/local/bin/codex" || cx.Model != "gpt-5-codex" {
		t.Fatalf("codex = %+v", cx)
	}
	if r := CodexRuntime(dir, c, "/opt/athena", ""); r.Binary != "/usr/local/bin/codex" || r.Model != "gpt-5-codex" {
		t.Fatalf("runtime = %+v", r)
	}
	// The section survives a rewrite of the file.
	if err := SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	if again, err := LoadConfig(dir); err != nil || again.Runtimes.Codex.On() || again.Runtimes.Codex.Model != "gpt-5-codex" {
		t.Fatalf("after save: %+v, %v", again.Runtimes, err)
	}

	writeConfigFile(t, dir, `{"runtimes": {"codex": {"bin": "codex"}}}`)
	if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "bin") {
		t.Fatalf("an unknown field: %v", err)
	}
}

// Codex's decision hooks wait a fixed day: with Codex on, the server refuses an approval timeout they would not
// outlast; with Codex off it does not care.
func TestServerRefusesAnApprovalTimeoutCodexHooksWouldNotOutlast(t *testing.T) {
	if err := checkCodexApprovalTimeout(23*time.Hour + 55*time.Minute); err != nil {
		t.Fatalf("the longest allowed: %v", err)
	}
	dir := newStateDir(t)
	cfg := testConfig(t, dir)
	cfg.Node.ApprovalTimeout = 24 * time.Hour
	if err := Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "approval timeout of 24h0m0s is longer than Codex's hooks wait (at most 23h55m0s)") {
		t.Fatalf("Run = %v", err)
	}
	writeConfigFile(t, dir, `{"runtimes": {"codex": {"enabled": false}}}`)
	s := startServerWith(t, dir, func(c *Config) { c.Node.ApprovalTimeout = 24 * time.Hour })
	s.stop()
}

var (
	fakeCodexOnce sync.Once
	fakeCodexPath string
	fakeCodexErr  error
)

// fakeCodex builds fakecodex once per test binary run, into the fakeclaude build's directory (removed by TestMain).
func fakeCodex(t *testing.T) string {
	t.Helper()
	fakeClaude(t)
	fakeCodexOnce.Do(func() {
		fakeCodexPath = filepath.Join(fakeDir, "codex")
		out, err := exec.Command("go", "build", "-o", fakeCodexPath,
			"github.com/danielpina1/athena/internal/runtime/codex/testdata/fakecodex").CombinedOutput()
		if err != nil {
			fakeCodexErr = fmt.Errorf("build fakecodex: %v\n%s", err, out)
		}
	})
	if fakeCodexErr != nil {
		t.Fatal(fakeCodexErr)
	}
	return fakeCodexPath
}

// TestServerRunsCodexTickets: the server gives its node the Codex runtime of its configuration (here fakecodex, logged
// out), so a task pinned to Codex runs there, and one Codex cannot run fails with why rather than run elsewhere; the
// test servers otherwise point Codex at a binary that does not exist, so no test reaches the owner's codex.
func TestServerRunsCodexTickets(t *testing.T) {
	if cfg := testConfig(t, t.TempDir()); cfg.CodexBinary == "" || cfg.CodexBinary == DefaultCodexBinary {
		t.Fatalf("test servers run Codex %q, which may be the owner's", cfg.CodexBinary)
	}
	fake := fakeCodex(t)
	t.Setenv("ATHENA_FAKE_CODEX_LOGGED_OUT", "1")
	dir := newStateDir(t)
	s := startServerWith(t, dir, func(c *Config) { c.CodexBinary = fake })
	var sub struct {
		ID string `json:"id"`
	}
	s.ok(Request{Op: OpSubmit, Text: "Pinned to Codex", Runtime: core.RuntimeCodex}, &sub)
	var d TicketDetail
	eventually(t, "the codex ticket failed", func() bool {
		d = s.detail(sub.ID)
		a := d.Assignments[len(d.Assignments)-1]
		return a.Report != nil && a.Report.State == core.ReportFailed
	})
	if d.Runtime != core.RuntimeCodex || !strings.Contains(d.Assignments[0].Report.Summary, codex.NotLoggedIn) {
		t.Fatalf("runtime %q, report %+v", d.Runtime, d.Assignments[0].Report)
	}
	if _, err := os.Stat(filepath.Join(CodexHome(dir), "config.toml")); err != nil {
		t.Fatalf("the server's Codex check wrote no config.toml: %v", err)
	}
	r, err := call(dir, Request{Op: OpSubmit, Text: "x", Runtime: "gemini"})
	if err != nil || r.OK || !strings.Contains(r.Error, "gemini") {
		t.Fatalf("unknown runtime: %+v %v", r, err)
	}
}

// The runtimes section: the default runtime of a task (auto), the preferred one, the quota brake in percent and the
// session caps; the server gives the chooser the brake as fractions.
func TestRuntimesConfigDefaultsAndChooser(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := c.Runtimes
	if r.Default != core.RuntimeAuto || r.Preferred != core.RuntimeClaude || r.AvoidAbove["5h"] != 85 || r.AvoidAbove["7d"] != 90 ||
		r.Claude.MaxSessions != 0 || r.Codex.MaxSessions != 0 {
		t.Fatalf("defaults = %+v", r)
	}
	ch := r.chooser()
	if ch.Preferred != core.RuntimeClaude || ch.AvoidAbove["5h"] != 0.85 || ch.AvoidAbove["7d"] != 0.90 || len(ch.MaxSessions) != 0 {
		t.Fatalf("default chooser = %+v", ch)
	}

	writeConfigFile(t, dir, `{"runtimes": {"default": "claude", "preferred": "codex", "avoidAbove": {"5h": 50},
		"claude": {"maxSessions": 2}, "codex": {"enabled": false, "maxSessions": 1}}}`)
	if c, err = LoadConfig(dir); err != nil {
		t.Fatal(err)
	}
	ch = c.Runtimes.chooser()
	if c.Runtimes.Default != core.RuntimeClaude || ch.Preferred != core.RuntimeCodex || len(ch.AvoidAbove) != 1 || ch.AvoidAbove["5h"] != 0.5 ||
		ch.MaxSessions[core.RuntimeClaude] != 2 || ch.MaxSessions[core.RuntimeCodex] != 1 || c.Runtimes.Codex.On() {
		t.Fatalf("configured: %+v, chooser %+v", c.Runtimes, ch)
	}
	if err := SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	if again, err := LoadConfig(dir); err != nil || again.Runtimes.Preferred != core.RuntimeCodex || again.Runtimes.Claude.MaxSessions != 2 {
		t.Fatalf("after save: %+v, %v", again.Runtimes, err)
	}
	// An empty brake brakes nothing.
	writeConfigFile(t, dir, `{"runtimes": {"avoidAbove": {}}}`)
	if c, err = LoadConfig(dir); err != nil || len(c.Runtimes.chooser().AvoidAbove) != 0 {
		t.Fatalf("empty brake: %+v, %v", c.Runtimes, err)
	}

	for _, bad := range []struct{ body, want string }{
		{`{"runtimes": {"default": "gemini"}}`, "runtimes.default"},
		{`{"runtimes": {"preferred": "auto"}}`, "runtimes.preferred"},
		{`{"runtimes": {"avoidAbove": {"5h": 150}}}`, "runtimes.avoidAbove"},
		{`{"runtimes": {"avoidAbove": {"5h": 0}}}`, "runtimes.avoidAbove"},
		{`{"runtimes": {"claude": {"maxSessions": -1}}}`, "maxSessions"},
		{`{"runtimes": {"codex": {"maxSessions": -2}}}`, "maxSessions"},
	} {
		writeConfigFile(t, dir, bad.body)
		if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: %v, want an error naming %s", bad.body, err, bad.want)
		}
	}
}

// A task that names no runtime runs on the configuration's default, auto: here on Claude Code, since the test server's
// Codex does not exist. `runtime` on the submit pins one.
func TestServerDefaultRuntimeIsAuto(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	dir := newStateDir(t)
	s := startServer(t, dir)
	var sub struct {
		ID string `json:"id"`
	}
	s.ok(Request{Op: OpSubmit, Text: "No runtime named"}, &sub)
	var d TicketDetail
	eventually(t, "the auto ticket runs", func() bool {
		d = s.detail(sub.ID)
		return d.Assignments[0].Runtime != ""
	})
	if d.Runtime != core.RuntimeAuto || d.Assignments[0].Runtime != core.RuntimeClaude {
		t.Fatalf("runtime %q, assignment on %q", d.Runtime, d.Assignments[0].Runtime)
	}
	s.ok(Request{Op: OpSubmit, Text: "Pinned", Runtime: core.RuntimeClaude}, &sub)
	if d := s.detail(sub.ID); d.Runtime != core.RuntimeClaude {
		t.Fatalf("pinned runtime = %q", d.Runtime)
	}
}
