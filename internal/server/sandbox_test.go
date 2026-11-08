package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/danielpina1/athena/internal/node"
)

func writeConfigFile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Without a sandbox section the sandbox is on wherever Claude Code has one.
func TestSandboxDefaultsOn(t *testing.T) {
	c, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Sandbox.On() != node.SandboxSupported() {
		t.Fatalf("On = %v on a platform where supported = %v", c.Sandbox.On(), node.SandboxSupported())
	}
	if sb := c.nodeSandbox(); (sb != nil) != node.SandboxSupported() || (sb != nil && (sb.Required || len(sb.DenyRead) != 0)) {
		t.Fatalf("default node sandbox = %+v", sb)
	}
}

// weakerNetworkIsolation is on by default on macOS, where gh verifies TLS through trustd, and off elsewhere; an
// explicit value wins on every platform.
func TestSandboxWeakerNetworkIsolationDefault(t *testing.T) {
	for _, tc := range []struct {
		body       string
		goos       string
		want       bool
		wantStored *bool
	}{
		{`{}`, "darwin", true, nil},
		{`{}`, "linux", false, nil},
		{`{"sandbox": {"weakerNetworkIsolation": false}}`, "darwin", false, new(false)},
		{`{"sandbox": {"weakerNetworkIsolation": true}}`, "linux", true, new(true)},
	} {
		dir := t.TempDir()
		writeConfigFile(t, dir, tc.body)
		c, err := LoadConfig(dir)
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if got := c.Sandbox.weakerNetworkIsolationOn(tc.goos); got != tc.want {
			t.Errorf("%s on %s: weakerNetworkIsolation = %v, want %v", tc.body, tc.goos, got, tc.want)
		}
		// An explicit false survives `athena github setup` rewriting the file; an unset value stays unset.
		if err := SaveConfig(dir, c); err != nil {
			t.Fatal(err)
		}
		again, err := LoadConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := again.Sandbox.WeakerNetworkIsolation; (got == nil) != (tc.wantStored == nil) || (got != nil && *got != *tc.wantStored) {
			t.Errorf("%s: after save weakerNetworkIsolation = %v, want %v", tc.body, got, tc.wantStored)
		}
	}
	if !node.SandboxSupported() {
		return
	}
	// The node gets the effective value.
	dir := t.TempDir()
	writeConfigFile(t, dir, `{}`)
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sb := c.nodeSandbox(); sb.WeakerNetworkIsolation != (runtime.GOOS == "darwin") {
		t.Errorf("default node sandbox weakerNetworkIsolation = %v on %s", sb.WeakerNetworkIsolation, runtime.GOOS)
	}
	writeConfigFile(t, dir, `{"sandbox": {"weakerNetworkIsolation": false}}`)
	if c, err = LoadConfig(dir); err != nil {
		t.Fatal(err)
	}
	if sb := c.nodeSandbox(); sb.WeakerNetworkIsolation {
		t.Error("weakerNetworkIsolation false still reached the node sandbox")
	}
}

func TestSandboxConfigLoads(t *testing.T) {
	if !node.SandboxSupported() {
		t.Skip("no Claude Code sandbox on this platform")
	}
	dir := t.TempDir()
	writeConfigFile(t, dir, `{"github": {"appId": 1, "slug": "a", "keyFile": "/keys/app.pem", "installationId": 2, "repos": ["o/r"]},
		"sandbox": {"required": true, "allowedDomains": ["proxy.golang.org", "*.npmjs.org"], "allowWrite": ["/cache/go"],
		"weakerNetworkIsolation": true, "caFile": "/etc/ca.pem"}}`)
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	sb := c.nodeSandbox()
	if sb == nil || !sb.Required || !sb.WeakerNetworkIsolation || sb.CAFile != "/etc/ca.pem" ||
		!slices.Equal(sb.AllowedDomains, []string{"proxy.golang.org", "*.npmjs.org"}) || !slices.Equal(sb.AllowWrite, []string{"/cache/go"}) {
		t.Fatalf("node sandbox = %+v", sb)
	}
	if !slices.Equal(sb.DenyRead, []string{"/keys/app.pem"}) {
		t.Fatalf("denyRead = %v, want the App key", sb.DenyRead)
	}

	// The section survives `athena github setup` rewriting the file.
	if err := SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	again, err := LoadConfig(dir)
	if err != nil || !again.Sandbox.Required || again.Sandbox.CAFile != "/etc/ca.pem" {
		t.Fatalf("after save: %+v, %v", again.Sandbox, err)
	}

	writeConfigFile(t, dir, `{"sandbox": {"enabled": false, "required": true}}`)
	if c, err := LoadConfig(dir); err != nil || c.Sandbox.On() || c.nodeSandbox() != nil {
		t.Fatalf("enabled false: on=%v sandbox=%+v err=%v", c.Sandbox.On(), c.nodeSandbox(), err)
	}
}

func TestSandboxConfigRejects(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"sandbox": {"allowedDomains": ["https://example.com"]}}`, "allowedDomains"},
		{`{"sandbox": {"allowedDomains": ["example.com/path"]}}`, "allowedDomains"},
		{`{"sandbox": {"allowWrite": ["relative/dir"]}}`, "allowWrite"},
		{`{"sandbox": {"caFile": "ca.pem"}}`, "caFile"},
		{`{"sandbox": {"enable": true}}`, "unknown field"},
	} {
		dir := t.TempDir()
		writeConfigFile(t, dir, tc.body)
		if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %s", tc.body, err, tc.want)
		}
	}
}

// The server gives its node the configured sandbox: a task's worker settings carry the block with the extra host.
func TestServerWorkersGetSandbox(t *testing.T) {
	if !node.SandboxSupported() {
		t.Skip("no Claude Code sandbox on this platform")
	}
	newFakeEnv(t, openTurnFixture(t))
	dir := newStateDir(t)
	writeConfigFile(t, dir, `{"sandbox": {"allowedDomains": ["proxy.golang.org"]}}`)
	s := startServer(t, dir)
	id := s.submit("Sandboxed task")
	s.running(id)
	matches, _ := filepath.Glob(filepath.Join(dir, "work", "*", ".athena", "settings.json"))
	if len(matches) != 1 {
		t.Fatalf("worker settings files: %v", matches)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Sandbox struct {
			Enabled bool `json:"enabled"`
			Weaker  bool `json:"enableWeakerNetworkIsolation"`
			Network struct {
				AllowedDomains   []string `json:"allowedDomains"`
				AllowUnixSockets []string `json:"allowUnixSockets"`
			} `json:"network"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	net := settings.Sandbox.Network
	if !settings.Sandbox.Enabled || !slices.Contains(net.AllowedDomains, "proxy.golang.org") || !slices.Contains(net.AllowedDomains, "github.com") ||
		!slices.Equal(net.AllowUnixSockets, []string{filepath.Join(dir, NodeSocket)}) {
		t.Fatalf("worker sandbox:\n%s", raw)
	}
	// Unset in config.json, the worker's settings let gh reach trustd on macOS, and only there.
	if settings.Sandbox.Weaker != (runtime.GOOS == "darwin") {
		t.Fatalf("enableWeakerNetworkIsolation = %v on %s:\n%s", settings.Sandbox.Weaker, runtime.GOOS, raw)
	}
}

// With the sandbox off, the node still learns the App key, which restricted sessions' commands may never read.
func TestSecretsReachTheNodeWithSandboxOff(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, `{"github": {"appId": 1, "slug": "a", "keyFile": "/keys/app.pem", "installationId": 2, "repos": ["o/r"]},
		"sandbox": {"enabled": false}}`)
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.nodeSandbox() != nil {
		t.Fatal("sandbox off still made a node sandbox")
	}
	if got := c.nodeSecrets(); !slices.Equal(got, []string{"/keys/app.pem"}) {
		t.Fatalf("secrets = %v, want the App key", got)
	}
	writeConfigFile(t, dir, `{}`)
	if c, err = LoadConfig(dir); err != nil || len(c.nodeSecrets()) != 0 {
		t.Fatalf("no GitHub: secrets = %v, %v", c.nodeSecrets(), err)
	}
}
