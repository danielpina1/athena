package node

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/danielpina1/athena/internal/core"
)

// testNode is a Node for the clone tests: a state directory, no socket served.
func testNode(t *testing.T) *Node {
	t.Helper()
	n := New(Config{WorkRoot: t.TempDir(), StateDir: t.TempDir(), AthenaBin: "/usr/bin/true", SocketPath: "/tmp/athena-unused.sock",
		Credentials: &fakeCreds{}}, nil)
	t.Cleanup(func() { n.Close() })
	return n
}

// fakeGitHub makes <root>/<owner>/<name>.git, a bare repository whose default branch is trunk (one commit) beside a
// branch other with one more, and points the node's https://github.com/<repo>.git at it (githubTarget, with the file
// transport allowed for the node's git): no test reaches the network. It returns root and a work clone that pushes to the bare repository.
func fakeGitHub(t *testing.T, repo string) (root, work string) {
	t.Helper()
	root = t.TempDir()
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(src, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "README")
	git(t, src, "commit", "-q", "-m", "init")
	git(t, src, "checkout", "-q", "-b", "other")
	git(t, src, "commit", "-q", "--allow-empty", "-m", "other work")
	git(t, src, "checkout", "-q", "trunk")
	bare := filepath.Join(root, repo+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, root, "clone", "-q", "--bare", src, bare)
	git(t, bare, "symbolic-ref", "HEAD", "refs/heads/trunk")
	prevTarget, prevExtra := githubTarget, nodeGitExtra
	githubTarget = func(repo string) string { return "file://" + root + "/" + strings.ToLower(repo) + ".git" }
	nodeGitExtra = [][2]string{{"protocol.file.allow", "always"}}
	t.Cleanup(func() { githubTarget, nodeGitExtra = prevTarget, prevExtra })
	work = t.TempDir()
	git(t, work, "clone", "-q", bare, ".")
	return root, work
}

const fakeToken = "ghs_n0deT0kenNeverShown0123456789abcdef"

func TestCloneUsesHelperNotURL(t *testing.T) {
	_, work := fakeGitHub(t, "o/r")
	n := testNode(t)
	state := n.cfg.StateDir
	ctx := context.Background()
	dir, def, err := n.EnsureClone(ctx, "O/R", fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(state, "repos", "o", "r"); dir != want || def != "trunk" {
		t.Fatalf("EnsureClone = %q, %q; want %q, trunk", dir, def, want)
	}
	// The configured URL, before insteadOf rewrites it to the stand-in.
	if got := git(t, dir, "config", "--get", "remote.origin.url"); got != "https://github.com/o/r.git" {
		t.Fatalf("origin = %q, want the plain https URL", got)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("clone dir: %v %v, want 0700", err, st)
	}
	// Nothing the clone keeps holds the token.
	_ = filepath.Walk(filepath.Join(dir, ".git"), func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() && info.Size() < 1<<20 {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), fakeToken) {
				t.Errorf("%s holds the token", p)
			}
		}
		return nil
	})
	// The call's nonce ended with it.
	n.grants.Range(func(k, _ any) bool { t.Errorf("nonce %v outlived its git call", k); return true })

	// A second call fetches: a commit pushed meanwhile is on origin/trunk.
	git(t, work, "commit", "-q", "--allow-empty", "-m", "later")
	git(t, work, "push", "-q", "origin", "trunk")
	if _, _, err := n.EnsureClone(ctx, "o/r", fakeToken); err != nil {
		t.Fatal(err)
	}
	if got, want := git(t, dir, "rev-parse", "origin/trunk"), git(t, work, "rev-parse", "HEAD"); got != want {
		t.Fatalf("origin/trunk = %s after the second EnsureClone, want %s", got, want)
	}
}

func TestCloneRefusesBadRepository(t *testing.T) {
	for _, repo := range []string{"", "o", "o/r/x", "../r", "o/..", "o/r;rm", "-o/r x"} {
		if _, _, err := testNode(t).EnsureClone(context.Background(), repo, fakeToken); err == nil {
			t.Errorf("EnsureClone(%q) succeeded", repo)
		}
	}
}

func TestWorktreeFromDefaultBranch(t *testing.T) {
	_, work := fakeGitHub(t, "o/r")
	n := testNode(t)
	ctx := context.Background()
	if _, _, err := n.EnsureClone(ctx, "o/r", fakeToken); err != nil {
		t.Fatal(err)
	}
	// The default branch moves after the clone; the next worktree starts from where it is now.
	git(t, work, "commit", "-q", "--allow-empty", "-m", "newer trunk")
	git(t, work, "push", "-q", "origin", "trunk")
	clone, def, err := n.EnsureClone(ctx, "o/r", fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := ensureWorktree(clone, wt, "athena/t-0001", "origin/"+def, cloneGitEnv(clone, "o/r", nil)); err != nil {
		t.Fatal(err)
	}
	if got, want := git(t, wt, "rev-parse", "HEAD"), git(t, work, "rev-parse", "HEAD"); got != want {
		t.Fatalf("worktree HEAD = %s, want the remote default branch %s", got, want)
	}
	if got := git(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "athena/t-0001" {
		t.Fatalf("worktree branch = %q", got)
	}
	if out, err := exec.Command("git", "-C", wt, "rev-parse", "--abbrev-ref", "@{upstream}").CombinedOutput(); err == nil {
		t.Fatalf("the ticket branch tracks %s; a plain git push must not go to the default branch", out)
	}
	if git(t, clone, "rev-parse", "--abbrev-ref", "HEAD") != "trunk" {
		t.Fatal("the clone's own checkout moved")
	}
}

// plantHooks makes a hooks directory whose every hook and an fsmonitor touch marker, and returns it with the
// fsmonitor script.
func plantHooks(t *testing.T, marker string) (hooks, fsmonitor string) {
	t.Helper()
	hooks = t.TempDir()
	script := "#!/bin/sh\necho \"$0\" >> " + marker + "\n"
	for _, h := range []string{"reference-transaction", "post-checkout", "post-merge", "pre-auto-gc", "post-rewrite", "fsmonitor-watchman"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fsmonitor = filepath.Join(t.TempDir(), "fsmon")
	if err := os.WriteFile(fsmonitor, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return hooks, fsmonitor
}

// A worker can write the clone's shared config from its worktree: the node's own git (fetch, worktree add and
// remove, status) runs none of the hooks or commands it plants.
func TestCloneIgnoresPlantedHooks(t *testing.T) {
	_, work := fakeGitHub(t, "o/r")
	n := testNode(t)
	ctx := context.Background()
	clone, def, err := n.EnsureClone(ctx, "o/r", fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	wt1 := filepath.Join(t.TempDir(), "wt1")
	if err := ensureWorktree(clone, wt1, "athena/t-0001", "origin/"+def, cloneGitEnv(clone, "o/r", nil)); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	hooks, fsmon := plantHooks(t, marker)
	git(t, wt1, "config", "core.hooksPath", hooks)
	git(t, wt1, "config", "core.fsmonitor", fsmon)

	git(t, work, "commit", "-q", "--allow-empty", "-m", "later")
	git(t, work, "push", "-q", "origin", "trunk")
	if _, _, err := n.EnsureClone(ctx, "o/r", fakeToken); err != nil {
		t.Fatal(err)
	}
	wt2 := filepath.Join(t.TempDir(), "wt2")
	if err := ensureWorktree(clone, wt2, "athena/t-0002", "origin/"+def, cloneGitEnv(clone, "o/r", nil)); err != nil {
		t.Fatal(err)
	}
	cleanupWorktree(placement{workDir: wt2, repo: clone, github: "o/r"}, core.Ticket{ID: "T-0002"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("the node's git ran planted hooks:\n%s", b)
	}
	// The planted hooks are real: the worker's own git runs them.
	git(t, wt1, "checkout", "-q", "-b", "scratch")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the planted hooks never run, so the test proves nothing: %v", err)
	}
}

// A worker's insteadOf (even one as long as the whole URL), a rewritten remote URL or an allowed ext transport in the
// shared config do not redirect the node's fetch.
func TestCloneFetchNotRedirected(t *testing.T) {
	_, work := fakeGitHub(t, "o/r")
	n := testNode(t)
	ctx := context.Background()
	clone, _, err := n.EnsureClone(ctx, "o/r", fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	evil := t.TempDir()
	git(t, evil, "init", "-q", "--bare", "o/r.git")
	evilWork := t.TempDir()
	git(t, evilWork, "init", "-q", "-b", "trunk")
	git(t, evilWork, "commit", "-q", "--allow-empty", "-m", "evil")
	git(t, evilWork, "push", "-q", filepath.Join(evil, "o", "r.git"), "trunk")
	git(t, clone, "config", "url.file://"+evil+"/.insteadOf", "https://github.com/")
	git(t, clone, "config", "url.file://"+evil+"/o/r.git.insteadOf", "https://github.com/o/r.git")
	git(t, clone, "config", "remote.origin.url", "file://"+evil+"/o/r.git")
	git(t, clone, "config", "protocol.ext.allow", "always")

	git(t, work, "commit", "-q", "--allow-empty", "-m", "real later")
	git(t, work, "push", "-q", "origin", "trunk")
	if _, _, err := n.EnsureClone(ctx, "o/r", fakeToken); err != nil {
		t.Fatal(err)
	}
	if got, want := git(t, clone, "rev-parse", "refs/remotes/origin/trunk"), git(t, work, "rev-parse", "HEAD"); got != want {
		t.Fatalf("origin/trunk = %s, want the real repository's %s (the fetch was redirected)", got, want)
	}
}

// The owner's global `url."git@github.com:".insteadOf https://github.com/` sends neither the node's git nor a
// session's push over SSH as the owner.
func TestOwnerSSHInsteadOfIgnored(t *testing.T) {
	fakeGitHub(t, "o/r")
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[url \"git@github.com:\"]\n\tinsteadOf = https://github.com/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	n := testNode(t)
	clone, _, err := n.EnsureClone(context.Background(), "o/r", fakeToken)
	if err != nil {
		t.Fatalf("clone with the owner's SSH rewrite: %v", err)
	}
	getURL := func(env []string) string {
		cmd := exec.Command("git", "-C", clone, "ls-remote", "--get-url", "https://github.com/o/r.git")
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	want := githubTarget("o/r")
	if got := getURL(cloneGitEnv(clone, "o/r", nil)); got != want {
		t.Errorf("node git reaches %q, want %q", got, want)
	}
	s := &session{t: core.Ticket{ID: "T-1", Repo: "o/r"}}
	if got := getURL(append(os.Environ(), n.githubEnv(s)...)); got != want {
		t.Errorf("a session's push goes to %q, want %q", got, want)
	}
	env := envLookup(n.githubEnv(s))
	found := false
	for i := 0; env("GIT_CONFIG_KEY_"+strconv.Itoa(i)) != ""; i++ {
		found = found || (env("GIT_CONFIG_KEY_"+strconv.Itoa(i)) == "protocol.ssh.allow" && env("GIT_CONFIG_VALUE_"+strconv.Itoa(i)) == "never")
	}
	if !found {
		t.Error("sessions may still use ssh")
	}
}
