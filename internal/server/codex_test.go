package server

import (
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
	r := CodexRuntime(dir, c, "/opt/athena", "", 0)
	if r.Home != filepath.Join(dir, "codex") || r.Binary != "codex" || r.AthenaBin != "/opt/athena" ||
		r.Socket != filepath.Join(dir, NodeSocket) || r.WorkRoot != filepath.Join(dir, "work") || r.HookTimeout != 35*time.Minute {
		t.Fatalf("runtime = %+v", r)
	}
	if r := CodexRuntime(dir, c, "/opt/athena", "/bin/fakecodex", time.Minute); r.Binary != "/bin/fakecodex" || r.HookTimeout != 6*time.Minute {
		t.Fatalf("--codex and the approval timeout: %+v", r)
	}

	writeConfigFile(t, dir, `{"runtimes": {"codex": {"enabled": false, "binary": "/usr/local/bin/codex", "model": "gpt-5-codex"}}}`)
	if c, err = LoadConfig(dir); err != nil {
		t.Fatal(err)
	}
	if cx := c.Runtimes.Codex; cx.On() || cx.Binary != "/usr/local/bin/codex" || cx.Model != "gpt-5-codex" {
		t.Fatalf("codex = %+v", cx)
	}
	if r := CodexRuntime(dir, c, "/opt/athena", "", 0); r.Binary != "/usr/local/bin/codex" || r.Model != "gpt-5-codex" {
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
