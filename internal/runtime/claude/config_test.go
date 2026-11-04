package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func readJSON(t *testing.T, path string) any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, b)
	}
	return v
}

func hookEntry(command string, timeout int) any {
	return []any{map[string]any{"hooks": []any{map[string]any{
		"type": "command", "command": command, "timeout": float64(timeout),
	}}}}
}

func TestWriteSettingsShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "settings.json")
	if err := WriteSettings(path, "/opt/athena/bin/athena", "/run/athena.sock", 90*time.Minute); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"env": map[string]any{"ATHENA_SOCKET": "/run/athena.sock"},
		"hooks": map[string]any{
			"PreToolUse":        hookEntry("/opt/athena/bin/athena hook PreToolUse", 5400),
			"PermissionRequest": hookEntry("/opt/athena/bin/athena hook PermissionRequest", 5400),
			"PostToolUse":       hookEntry("/opt/athena/bin/athena hook PostToolUse", 30),
			"Stop":              hookEntry("/opt/athena/bin/athena hook Stop", 30),
			"SessionStart":      hookEntry("/opt/athena/bin/athena hook SessionStart", 30),
		},
		"attribution":         map[string]any{"commit": "", "pr": "", "sessionUrl": false},
		"includeCoAuthoredBy": false,
	}
	if got := readJSON(t, path); !reflect.DeepEqual(got, want) {
		gb, _ := json.MarshalIndent(got, "", " ")
		wb, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("settings mismatch\ngot:\n%s\nwant:\n%s", gb, wb)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", st.Mode().Perm())
	}
}

func TestWriteSettingsDefaultTimeoutIsTwoHours(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := WriteSettings(path, "athena", "/s", 0); err != nil {
		t.Fatal(err)
	}
	hooks := readJSON(t, path).(map[string]any)["hooks"].(map[string]any)
	for _, ev := range []string{"PreToolUse", "PermissionRequest"} {
		got := hooks[ev].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["timeout"]
		if got != float64(7200) {
			t.Errorf("%s timeout = %v, want 7200", ev, got)
		}
	}
	for _, ev := range []string{"PostToolUse", "Stop", "SessionStart"} {
		got := hooks[ev].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["timeout"]
		if got != float64(30) {
			t.Errorf("%s timeout = %v, want 30", ev, got)
		}
	}
}

func TestWriteSettingsQuotesBinaryPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := WriteSettings(path, "/Users/a b/it's/athena", "/s", 0); err != nil {
		t.Fatal(err)
	}
	hooks := readJSON(t, path).(map[string]any)["hooks"].(map[string]any)
	cmd := hooks["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"]
	want := `'/Users/a b/it'\''s/athena' hook Stop`
	if cmd != want {
		t.Fatalf("command = %q, want %q", cmd, want)
	}
}

func TestWriteSettingsRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSettings(filepath.Join(dir, "a.json"), "", "/s", 0); err == nil {
		t.Fatal("want error for empty athenaBin")
	}
	if err := WriteSettings(filepath.Join(dir, "a.json"), "athena", "", 0); err == nil {
		t.Fatal("want error for empty socket")
	}
}

func TestWriteMCPConfigShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := WriteMCPConfig(path, "/opt/athena", "/run/athena.sock", "tkt-1", "asg-1"); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"mcpServers": map[string]any{"athena": map[string]any{
		"type":    "stdio",
		"command": "/opt/athena",
		"args":    []any{"mcp", "--ticket", "tkt-1"},
		"env":     map[string]any{"ATHENA_SOCKET": "/run/athena.sock", "ATHENA_TICKET": "tkt-1", "ATHENA_ASSIGNMENT": "asg-1"},
	}}}
	if got := readJSON(t, path); !reflect.DeepEqual(got, want) {
		gb, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("mcp config mismatch\ngot:\n%s", gb)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", st.Mode().Perm())
	}
	if err := WriteMCPConfig(path, "/opt/athena", "/run/athena.sock", "tkt-1", ""); err == nil {
		t.Fatal("WriteMCPConfig accepted an empty assignment")
	}
}

func TestWriteRouterMCPConfigShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := WriteRouterMCPConfig(path, "/bin/athena", "/tmp/s.sock", "tok"); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	s, ok := cfg.MCPServers["athena"]
	if !ok || len(cfg.MCPServers) != 1 {
		t.Fatalf("servers = %+v", cfg.MCPServers)
	}
	if s.Command != "/bin/athena" || strings.Join(s.Args, " ") != "mcp --role router" {
		t.Fatalf("server = %+v", s)
	}
	if s.Env["ATHENA_SOCKET"] != "/tmp/s.sock" || s.Env["ATHENA_ROUTER_TOKEN"] != "tok" || s.Env["ATHENA_TICKET"] != "" {
		t.Fatalf("env = %v", s.Env)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
	if err := WriteRouterMCPConfig(path, "/bin/athena", "/tmp/s.sock", ""); err == nil {
		t.Fatal("no token accepted")
	}
}

func TestWriteSettingsWithSandbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	sb := &Sandbox{
		AllowWrite:     []string{"/w/t-1", "/repo/.git"},
		DenyWrite:      []string{"/repo/.git/config", "/repo/.git/hooks"},
		DenyRead:       []string{"/home/o/.athena/github-app.pem"},
		AllowedDomains: []string{"github.com", "*.github.com"},
		UnixSockets:    []string{"/run/athena.sock"},
	}
	if err := WriteSettingsWith(path, "/opt/athena", "/run/athena.sock", 0, sb, "SSL_CERT_FILE=/etc/ca.pem"); err != nil {
		t.Fatal(err)
	}
	got := readJSON(t, path).(map[string]any)
	want := map[string]any{
		"enabled":                  true,
		"failIfUnavailable":        false,
		"autoAllowBashIfSandboxed": false,
		"allowUnsandboxedCommands": false,
		"filesystem": map[string]any{
			"allowWrite": []any{"/w/t-1", "/repo/.git"},
			"denyWrite":  []any{"/repo/.git/config", "/repo/.git/hooks"},
			"denyRead":   []any{"/home/o/.athena/github-app.pem"},
		},
		"network": map[string]any{
			"allowedDomains":   []any{"github.com", "*.github.com"},
			"strictAllowlist":  true,
			"allowUnixSockets": []any{"/run/athena.sock"},
		},
	}
	if !reflect.DeepEqual(got["sandbox"], want) {
		gb, _ := json.MarshalIndent(got["sandbox"], "", " ")
		t.Fatalf("sandbox block:\n%s", gb)
	}
	if env := got["env"]; !reflect.DeepEqual(env, map[string]any{"ATHENA_SOCKET": "/run/athena.sock", "SSL_CERT_FILE": "/etc/ca.pem"}) {
		t.Fatalf("env = %v", env)
	}
	if got["hooks"].(map[string]any)["PreToolUse"] == nil {
		t.Fatal("the sandbox replaced the hooks")
	}

	// The optional switches, and empty lists written as [] rather than null.
	sb = &Sandbox{Required: true, AllowAllUnixSockets: true, WeakerNetworkIsolation: true}
	if err := WriteSettingsWith(path, "/opt/athena", "/s", 0, sb); err != nil {
		t.Fatal(err)
	}
	block := readJSON(t, path).(map[string]any)["sandbox"].(map[string]any)
	if block["failIfUnavailable"] != true || block["enableWeakerNetworkIsolation"] != true ||
		block["network"].(map[string]any)["allowAllUnixSockets"] != true {
		t.Fatalf("switches not written: %v", block)
	}
	if fs := block["filesystem"].(map[string]any); !reflect.DeepEqual(fs["allowWrite"], []any{}) {
		t.Fatalf("empty allowWrite = %#v, want []", fs["allowWrite"])
	}
}

// The settings env cannot replace ATHENA_SOCKET, and a malformed entry is refused.
func TestWriteSettingsEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := WriteSettingsWith(path, "athena", "/s", 0, nil, "ATHENA_SOCKET=/evil"); err != nil {
		t.Fatal(err)
	}
	got := readJSON(t, path).(map[string]any)
	if got["env"].(map[string]any)["ATHENA_SOCKET"] != "/s" {
		t.Fatalf("env = %v", got["env"])
	}
	if _, ok := got["sandbox"]; ok {
		t.Fatal("a sandbox block without a Sandbox")
	}
	if err := WriteSettingsWith(path, "athena", "/s", 0, nil, "NOEQUALS"); err == nil {
		t.Fatal("want an error for an env entry without =")
	}
}

// Every settings file hides Claude Code's attribution: no Co-Authored-By trailer on commits, no "Generated with" line
// or session link in pull requests, in both the current form and the deprecated one.
func TestWriteSettingsHidesAttribution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := WriteSettingsWith(path, "/bin/athena", "/run/a.sock", 0, &Sandbox{}, "A=b"); err != nil {
		t.Fatal(err)
	}
	got := readJSON(t, path).(map[string]any)
	want := map[string]any{"commit": "", "pr": "", "sessionUrl": false}
	if !reflect.DeepEqual(got["attribution"], want) {
		t.Errorf("attribution = %v, want %v", got["attribution"], want)
	}
	if v, ok := got["includeCoAuthoredBy"]; !ok || v != false {
		t.Errorf("includeCoAuthoredBy = %v (set %v), want false", v, ok)
	}
}
