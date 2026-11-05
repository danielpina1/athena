package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

type sandboxBlock struct {
	Enabled                  bool `json:"enabled"`
	FailIfUnavailable        bool `json:"failIfUnavailable"`
	AutoAllowBashIfSandboxed bool `json:"autoAllowBashIfSandboxed"`
	AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
	Weaker                   bool `json:"enableWeakerNetworkIsolation"`
	Filesystem               struct {
		AllowWrite []string `json:"allowWrite"`
		DenyWrite  []string `json:"denyWrite"`
		DenyRead   []string `json:"denyRead"`
	} `json:"filesystem"`
	Network struct {
		AllowedDomains      []string `json:"allowedDomains"`
		StrictAllowlist     bool     `json:"strictAllowlist"`
		AllowUnixSockets    []string `json:"allowUnixSockets"`
		AllowAllUnixSockets bool     `json:"allowAllUnixSockets"`
	} `json:"network"`
}

type sessionSettings struct {
	Env     map[string]string `json:"env"`
	Sandbox *sandboxBlock     `json:"sandbox"`
}

func readSessionSettings(t *testing.T, dir string) sessionSettings {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ".athena", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s sessionSettings
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("settings.json: %v\n%s", err, raw)
	}
	return s
}

// A worker in a worktree of a repository gets the sandbox block: its directory and the repository's git directory
// writable, the git config and hooks and Athena's own files not, GitHub and the configured hosts reachable, the node
// socket allowed, and no way to run a command outside the sandbox.
func TestWorkerSettingsCarrySandbox(t *testing.T) {
	repo := newRepo(t)
	h := newHarness(t, func(c *Config) {
		c.Sandbox = &SandboxConfig{Required: true, AllowedDomains: []string{"proxy.golang.org"}, AllowWrite: []string{"/cache/go"},
			DenyRead: []string{"/keys/app.pem"}, CAFile: "/etc/ca.pem", WeakerNetworkIsolation: true}
	})
	tk := h.submit("Sandboxed change", repo)
	tk, _ = h.running(tk.ID)
	dir := TicketDir(h.cfg.WorkRoot, tk)
	s := readSessionSettings(t, dir)
	sb := s.Sandbox
	if sb == nil {
		t.Fatal("settings.json has no sandbox block")
	}
	if !sb.Enabled || !sb.FailIfUnavailable || sb.AutoAllowBashIfSandboxed || sb.AllowUnsandboxedCommands || !sb.Weaker {
		t.Errorf("switches = %+v", sb)
	}
	common := filepath.Join(repo, ".git")
	if real, err := filepath.EvalSymlinks(common); err == nil {
		common = real // place resolves the repository's path
	}
	for _, p := range []string{dir, common, "/cache/go"} {
		if !slices.Contains(sb.Filesystem.AllowWrite, p) {
			t.Errorf("allowWrite %v lacks %s", sb.Filesystem.AllowWrite, p)
		}
	}
	for _, p := range []string{
		filepath.Join(common, "config"), filepath.Join(common, "config.worktree"), filepath.Join(common, "hooks"),
		filepath.Join(common, "worktrees", filepath.Base(dir), "config.worktree"),
		filepath.Join(dir, ".athena", "settings.json"), filepath.Join(dir, ".athena", "mcp.json"), filepath.Join(dir, ".athena", ".gitignore"),
	} {
		if !slices.Contains(sb.Filesystem.DenyWrite, p) {
			t.Errorf("denyWrite %v lacks %s", sb.Filesystem.DenyWrite, p)
		}
	}
	if !slices.Equal(sb.Filesystem.DenyRead, []string{"/keys/app.pem"}) {
		t.Errorf("denyRead = %v", sb.Filesystem.DenyRead)
	}
	if want := append(slices.Clone(GitHubDomains), "proxy.golang.org"); !slices.Equal(sb.Network.AllowedDomains, want) || !sb.Network.StrictAllowlist {
		t.Errorf("network = %+v, want domains %v and a strict allowlist", sb.Network, want)
	}
	if !slices.Equal(sb.Network.AllowUnixSockets, []string{h.cfg.SocketPath}) {
		t.Errorf("allowUnixSockets = %v, want the node socket %s", sb.Network.AllowUnixSockets, h.cfg.SocketPath)
	}
	if sb.Network.AllowAllUnixSockets != (runtime.GOOS == "linux") {
		t.Errorf("allowAllUnixSockets = %v on %s", sb.Network.AllowAllUnixSockets, runtime.GOOS)
	}
	for _, name := range []string{"SSL_CERT_FILE", "GIT_SSL_CAINFO", "CURL_CA_BUNDLE"} {
		if s.Env[name] != "/etc/ca.pem" {
			t.Errorf("env %s = %q, want the CA file", name, s.Env[name])
		}
	}
	if s.Env["ATHENA_SOCKET"] != h.cfg.SocketPath {
		t.Errorf("env ATHENA_SOCKET = %q", s.Env["ATHENA_SOCKET"])
	}
}

// Without Config.Sandbox the settings have no sandbox block (and a plain Ticket directory needs no git paths).
func TestWorkerSettingsWithoutSandbox(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Plain task", "")
	tk, _ = h.running(tk.ID)
	if s := readSessionSettings(t, TicketDir(h.cfg.WorkRoot, tk)); s.Sandbox != nil {
		t.Fatalf("sandbox block without Config.Sandbox: %+v", s.Sandbox)
	}

	h = newHarness(t, func(c *Config) { c.Sandbox = &SandboxConfig{} })
	tk = h.submit("Plain sandboxed task", "")
	tk, _ = h.running(tk.ID)
	dir := TicketDir(h.cfg.WorkRoot, tk)
	sb := readSessionSettings(t, dir).Sandbox
	if sb == nil || !slices.Equal(sb.Filesystem.AllowWrite, []string{dir}) || len(sb.Filesystem.DenyWrite) != 3 ||
		sb.FailIfUnavailable || sb.Weaker {
		t.Fatalf("sandbox of a plain directory = %+v", sb)
	}
}

func TestGitDirs(t *testing.T) {
	repo := newRepo(t)
	if g, c := gitDirs(repo); g != filepath.Join(repo, ".git") || c != g {
		t.Errorf("repository: %q %q", g, c)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", "-q", "-b", "b1", wt)
	g, c := gitDirs(wt)
	realRepo, _ := filepath.EvalSymlinks(repo)
	if want := filepath.Join(realRepo, ".git"); c != want && c != filepath.Join(repo, ".git") {
		t.Errorf("worktree common = %q, want %s", c, want)
	}
	if filepath.Dir(g) != filepath.Join(c, "worktrees") {
		t.Errorf("worktree gitdir = %q, want under %s/worktrees", g, c)
	}
	// A relative gitdir, as `git worktree add --relative-paths` writes it.
	rel := filepath.Join(t.TempDir(), "rel")
	if err := os.MkdirAll(filepath.Join(rel, "gd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, ".git"), []byte("gitdir: gd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, "gd", "commondir"), []byte("../common\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if g, c := gitDirs(rel); g != filepath.Join(rel, "gd") || c != filepath.Join(rel, "common") {
		t.Errorf("relative: %q %q", g, c)
	}
	if g, c := gitDirs(t.TempDir()); g != "" || c != "" {
		t.Errorf("no git: %q %q", g, c)
	}
}

// The kept prompt lives in the state directory, outside every path a sandboxed session may write.
func TestKeptPromptIsOutsideTheSandboxWriteSet(t *testing.T) {
	base := t.TempDir()
	n := New(Config{StateDir: filepath.Join(base, "state"), SocketPath: filepath.Join(base, "n.sock"), Sandbox: &SandboxConfig{}}, nil)
	sb, _ := n.sandboxFor(filepath.Join(base, "work", "t-1"))
	if sb == nil {
		t.Fatal("no sandbox")
	}
	kept := PromptPath(n.cfg.StateDir, "T-0001")
	for _, w := range sb.AllowWrite {
		if kept == w || strings.HasPrefix(kept, w+string(filepath.Separator)) {
			t.Fatalf("%s is inside the write set %s", kept, w)
		}
	}
}

func TestEndedTicketDropsItsKeptPrompt(t *testing.T) {
	n := New(Config{StateDir: t.TempDir()}, nil)
	n.sessionPrompt("T-0001", false)
	path := PromptPath(n.cfg.StateDir, "T-0001")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	n.dropPrompt("T-0001")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("kept prompt still there: %v", err)
	}
	n.dropPrompt("T-0001") // already gone: no error
}
