package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
