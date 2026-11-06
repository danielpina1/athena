package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests drive the installed `codex` and only run with ATHENA_REAL_CODEX=1. They send no prompt and spend no
// quota: `codex --version`, an app-server asked for its account and hooks (initialize, account/read, hooks/list,
// config/batchWrite of hook trust), and `codex sandbox`, which runs a command with no model. They use a throwaway
// CODEX_HOME, never Athena's or the owner's ~/.codex.

func realCodex(t *testing.T) string {
	t.Helper()
	if os.Getenv("ATHENA_REAL_CODEX") != "1" {
		t.Skip("set ATHENA_REAL_CODEX=1 to run against the installed codex")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Fatalf("no codex on PATH: %v", err)
	}
	return bin
}

// The real Codex takes Athena's config.toml under --strict-config, lists Athena's hooks from it, trusts them once
// Athena records their hashes, and keeps that trust across a rewrite of the file.
func TestRealHooksListTrusted(t *testing.T) {
	bin := realCodex(t)
	dir := t.TempDir()
	r := &Runtime{Binary: bin, Home: filepath.Join(dir, "codex"), AthenaBin: filepath.Join(dir, "athena bin", "athena"),
		Socket: filepath.Join(dir, "node.sock"), WorkRoot: filepath.Join(dir, "work")}
	s := r.Status(context.Background())
	t.Logf("first check: %+v", s)
	if !s.Installed || s.Version == "" || s.Hooks != "trusted" {
		t.Fatalf("status = %+v, want installed with Athena's hooks trusted", s)
	}
	if s.Account == "" && s.Why != NotLoggedIn {
		t.Fatalf("a throwaway home is not logged in, so Why should say so: %+v", s)
	}
	cfg := readFile(t, filepath.Join(r.Home, "config.toml"))
	if n := strings.Count(cfg, "\n[hooks.state."); n != len(hookEvents) {
		t.Fatalf("%d trust entries recorded, want %d:\n%s", n, len(hookEvents), cfg)
	}
	// The trust Codex recorded is in a form WriteHome keeps (Codex names the file with symlinks resolved).
	header := cfg[strings.Index(cfg, "\n[hooks.state.")+1:]
	header = header[:strings.Index(header, "\n")]
	t.Logf("first trust entry: %s", strings.ReplaceAll(header, dir, "<dir>"))
	if exists, cur, err := HomeCurrent(r.home()); !exists || !cur || err != nil {
		t.Fatalf("WriteHome would drop the trust Codex recorded: exists=%v current=%v err=%v", exists, cur, err)
	}
	r.Invalidate()
	if s := r.Status(context.Background()); s.Hooks != "trusted" {
		t.Fatalf("second check: %+v", s)
	}
	if again := readFile(t, filepath.Join(r.Home, "config.toml")); again != cfg {
		t.Fatalf("the second check rewrote the trust:\n%s", again)
	}
}

// Codex's workspace-write sandbox lets a session commit in a Ticket's worktree once the worktree's git directory and
// the common directory's objects, refs and logs are writable roots, and keeps a write outside them, and to the common
// directory's config and hooks, out. It does not let the commit through with the common directory itself as a root:
// Codex keeps a workspace's own git directory read-only even inside a writable root (logged below).
func TestRealSandboxCommitsInWorktree(t *testing.T) {
	bin := realCodex(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "codex")
	if err := WriteHome(Home{Dir: home, AthenaBin: "/usr/bin/true"}); err != nil {
		t.Fatal(err)
	}
	repo, wt, outside := filepath.Join(dir, "repo"), filepath.Join(dir, "wt"), filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	git := func(d string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...)
		cmd.Dir = d
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(dir, "init", "-q", repo)
	git(repo, "commit", "-q", "--allow-empty", "-m", "init")
	git(repo, "worktree", "add", "-q", wt, "-b", "athena/t-1")
	common, gitDir := filepath.Join(repo, ".git"), filepath.Join(repo, ".git", "worktrees", "wt")
	if real, err := filepath.EvalSymlinks(common); err == nil {
		common, gitDir = real, filepath.Join(real, "worktrees", "wt")
	}

	sandbox := func(roots []string, script string) (string, error) {
		quoted := make([]string, len(roots))
		for i, r := range roots {
			quoted[i] = tomlString(r)
		}
		cmd := exec.Command(bin, "sandbox", "-c", "sandbox_mode=workspace-write",
			// The test's directories are under TMPDIR, which workspace-write opens by default.
			"-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true", "-c", "sandbox_workspace_write.exclude_slash_tmp=true",
			"-c", "sandbox_workspace_write.writable_roots=["+strings.Join(quoted, ",")+"]", "--", "sh", "-c", script)
		cmd.Dir = wt
		cmd.Env = Env(os.Environ(), home)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	commit := "echo hi >> f && git add f && git -c user.name=t -c user.email=t@example.invalid commit -qm f"

	out, err := sandbox([]string{common}, commit)
	t.Logf("commit with the common directory as the root: err=%v\n%s", err, out)

	roots := []string{gitDir, filepath.Join(common, "objects"), filepath.Join(common, "refs"), filepath.Join(common, "logs")}
	if out, err := sandbox(roots, commit); err != nil {
		t.Fatalf("commit in the worktree: %v\n%s", err, out)
	}
	for _, target := range []string{filepath.Join(outside, "f"), filepath.Join(common, "config"), filepath.Join(common, "hooks", "pre-commit")} {
		if out, err := sandbox(roots, "echo x >> '"+target+"'"); err == nil {
			t.Errorf("the sandbox let a write to %s through:\n%s", target, out)
		}
	}
	if b, err := os.ReadFile(filepath.Join(common, "config")); err != nil || strings.Contains(string(b), "\nx\n") {
		t.Fatalf("config: %v\n%s", err, b)
	}
}
