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
