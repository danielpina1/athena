package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/runtime/claude"
)

// TestRealClaudeSandbox runs the installed `claude` (haiku, a few cents) in a worktree with the settings a sandboxed
// worker gets, and has it run one probe script through Bash. It only runs when ATHENA_TEST_REAL_CLAUDE=1. It checks
// that the worker's commands can write its worktree and commit (the repository's git directory), and cannot write
// outside it, the git hooks or config, or Athena's settings; cannot read a denied file; can reach the node socket
// through the real `athena git-credential`; can reach GitHub and not another host.
//
// Behind a TLS-inspecting proxy whose CA is only in the macOS keychain, git cannot verify GitHub from inside the
// sandbox: set ATHENA_TEST_SANDBOX_CA_FILE to a PEM bundle with that CA (the sandbox's caFile).
func TestRealClaudeSandbox(t *testing.T) {
	if os.Getenv("ATHENA_TEST_REAL_CLAUDE") != "1" {
		t.Skip("set ATHENA_TEST_REAL_CLAUDE=1 to run against the real claude binary")
	}
	if !SandboxSupported() {
		t.Skip("no Claude Code sandbox on this platform")
	}
	// Not under the system temp directory: the sandbox may treat it as its own scratch space. The socket path must
	// stay short (macOS limits it to 104 bytes).
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(cache, "athena-sbx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	if real, err := filepath.EvalSymlinks(base); err == nil {
		base = real
	}

	athena := filepath.Join(base, "bin", "athena")
	if out, err := exec.Command("go", "build", "-o", athena, "github.com/danielpina1/athena/cmd/athena").CombinedOutput(); err != nil {
		t.Fatalf("build athena: %v\n%s", err, out)
	}
	// Stand-in for the hooks: allow every permission request, so only the sandbox stands between a command and a path.
	stub := filepath.Join(base, "bin", "hook-stub")
	script := "#!/bin/sh\ncat >/dev/null\n" +
		"[ \"$2\" = PermissionRequest ] && echo '{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\"}}}'\n" +
		"exit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	repo := filepath.Join(base, "repo")
	wt := filepath.Join(base, "work", "t-1")
	outside := filepath.Join(base, "outside")
	secret := filepath.Join(base, "secret.pem")
	for _, d := range []string{repo, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(secret, []byte("not for workers\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	git(t, repo, "worktree", "add", "-q", "-b", "athena/t-1", wt)

	sock := filepath.Join(base, "node.sock")
	n := New(Config{StateDir: filepath.Join(base, "state"), SocketPath: sock, AthenaBin: athena,
		Sandbox: &SandboxConfig{Required: true, DenyRead: []string{secret}, CAFile: os.Getenv("ATHENA_TEST_SANDBOX_CA_FILE"),
			WeakerNetworkIsolation: runtime.GOOS == "darwin"}}, nil) // the server's default: gh needs trustd on macOS
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	go n.Serve(ctx)
	defer n.Close()

	adir := filepath.Join(wt, ".athena")
	if err := os.MkdirAll(adir, 0o700); err != nil {
		t.Fatal(err)
	}
	settings, mcp := filepath.Join(adir, "settings.json"), filepath.Join(adir, "mcp.json")
	sb, env := n.sandboxFor(wt)
	if err := claude.WriteSettingsWith(settings, stub, sock, 0, sb, env...); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	common := filepath.Join(repo, ".git")
	probe := fmt.Sprintf(`r() { if sh -c "$2" >/dev/null 2>"$1.err"; then echo "$1: ok"; else echo "$1: FAIL"; fi; }
{
r inside 'echo x > in.txt'
r commit 'git add in.txt && git commit -qm probe'
r outside 'echo x > %[1]s/out.txt'
r home 'echo x > "$HOME/.athena-sandbox-probe"'
r hooks 'echo x > %[2]s/hooks/post-commit'
r gitconfig 'git config --local core.fsmonitor /bin/evil'
r settings 'echo {} > .athena/settings.json'
r readsecret 'cat %[3]s'
r socket 'printf "protocol=https\nhost=github.com\npath=o/r\n\n" | %[4]s git-credential get 2>&1 | grep -q "not a live athena session"'
r github 'git ls-remote https://github.com/danielpina1/athena HEAD'
r otherhost 'curl -sS -o /dev/null --max-time 15 https://example.com'
} > probe.out
`, outside, common, secret, athena)
	if err := os.WriteFile(filepath.Join(wt, "probe.sh"), []byte(probe), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := claude.Start(ctx, claude.Options{Dir: wt, Model: "haiku", SettingsPath: settings, MCPConfigPath: mcp,
		Env: append([]string{"ATHENA_TICKET=t-1", "ATHENA_ASSIGNMENT=a-1"}, env...)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send("Run this exact Bash command once, in the sandbox (never ask to disable it): sh probe.sh   "+
		"Do not run anything else. Then say done.", ""); err != nil {
		t.Fatal(err)
	}
	for ev := range s.Events() {
		if _, ok := ev.(claude.Result); ok {
			_ = s.Close()
		}
	}
	if err := s.Wait(); err != nil {
		t.Logf("claude exited: %v (stderr: %s)", err, s.Stderr())
	}

	out, err := os.ReadFile(filepath.Join(wt, "probe.out"))
	if err != nil {
		t.Fatalf("the probe did not run: %v (stderr: %s)", err, s.Stderr())
	}
	t.Logf("probe:\n%s", out)
	want := map[string]string{"inside": "ok", "commit": "ok", "outside": "FAIL", "home": "FAIL", "hooks": "FAIL",
		"gitconfig": "FAIL", "settings": "FAIL", "readsecret": "FAIL", "socket": "ok", "github": "ok", "otherhost": "FAIL"}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if k, v, ok := strings.Cut(line, ": "); ok {
			got[k] = v
		}
	}
	for k, v := range want {
		errOut, _ := os.ReadFile(filepath.Join(wt, k+".err"))
		if got[k] != v {
			t.Errorf("%s = %q, want %s (stderr: %s)", k, got[k], v, strings.TrimSpace(string(errOut)))
		} else if v == "FAIL" {
			t.Logf("%s refused: %s", k, strings.TrimSpace(string(errOut)))
		}
	}
	// The probe's word is checked against the files themselves.
	if _, err := os.Stat(filepath.Join(outside, "out.txt")); err == nil {
		t.Error("a sandboxed command wrote outside the worktree")
	}
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, ".athena-sandbox-probe")); err == nil {
		os.Remove(filepath.Join(home, ".athena-sandbox-probe"))
		t.Error("a sandboxed command wrote to the home directory")
	}
	if _, err := os.Stat(filepath.Join(common, "hooks", "post-commit")); err == nil {
		t.Error("a sandboxed command wrote a git hook")
	}
	if b, _ := os.ReadFile(settings); !strings.Contains(string(b), `"sandbox"`) {
		t.Error("a sandboxed command rewrote the worker's settings")
	}
	if log := git(t, wt, "log", "--format=%s", "-1"); log != "probe" {
		t.Errorf("last commit = %q, want the probe's", log)
	}
}
