package node

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	dir, def, _, err := n.EnsureClone(ctx, "O/R", fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(state, "repos", "o", "r"); dir != want || def != "trunk" {
		t.Fatalf("EnsureClone = %q, %q; want %q, trunk", dir, def, want)
	}
	// The configured URL, before insteadOf rewrites it to the stand-in.
	mirror := MirrorDir(state, "o/r")
	if b, err := os.ReadFile(filepath.Join(mirror, "config")); err != nil || string(b) != mirrorConfig {
		t.Fatalf("mirror config = %q, %v; want the node's fixed config", b, err)
	}
	if got := git(t, dir, "config", "--get", "remote.origin.pushurl"); got != "https://github.com/o/r.git" {
		t.Fatalf("clone push URL = %q, want the plain https URL", got)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("clone dir: %v %v, want 0700", err, st)
	}
	// Nothing the clone, the mirror or the node's system file keeps holds the token.
	_ = filepath.Walk(filepath.Join(state), func(p string, info os.FileInfo, err error) error {
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
	if _, _, _, err := n.EnsureClone(ctx, "o/r", fakeToken); err != nil {
		t.Fatal(err)
	}
	if got, want := git(t, dir, "rev-parse", "origin/trunk"), git(t, work, "rev-parse", "HEAD"); got != want {
		t.Fatalf("origin/trunk = %s after the second EnsureClone, want %s", got, want)
	}
}

func TestCloneRefusesBadRepository(t *testing.T) {
	for _, repo := range []string{"", "o", "o/r/x", "../r", "o/..", "o/r;rm", "-o/r x"} {
		if _, _, _, err := testNode(t).EnsureClone(context.Background(), repo, fakeToken); err == nil {
			t.Errorf("EnsureClone(%q) succeeded", repo)
		}
	}
}

func TestWorktreeFromDefaultBranch(t *testing.T) {
	_, work := fakeGitHub(t, "o/r")
	n := testNode(t)
	ctx := context.Background()
	if _, _, _, err := n.EnsureClone(ctx, "o/r", fakeToken); err != nil {
		t.Fatal(err)
	}
	// The default branch moves after the clone; the next worktree starts from where it is now.
	git(t, work, "commit", "-q", "--allow-empty", "-m", "newer trunk")
	git(t, work, "push", "-q", "origin", "trunk")
	clone, def, _, err := n.EnsureClone(ctx, "o/r", fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := ensureWorktree(clone, wt, "athena/t-0001", "origin/"+def, cloneGitEnv(clone, "o/r")); err != nil {
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
	clone, def, _, err := n.EnsureClone(ctx, "o/r", fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	wt1 := filepath.Join(t.TempDir(), "wt1")
	if err := ensureWorktree(clone, wt1, "athena/t-0001", "origin/"+def, cloneGitEnv(clone, "o/r")); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	hooks, fsmon := plantHooks(t, marker)
	git(t, wt1, "config", "core.hooksPath", hooks)
	git(t, wt1, "config", "core.fsmonitor", fsmon)

	git(t, work, "commit", "-q", "--allow-empty", "-m", "later")
	git(t, work, "push", "-q", "origin", "trunk")
	if _, _, _, err := n.EnsureClone(ctx, "o/r", fakeToken); err != nil {
		t.Fatal(err)
	}
	wt2 := filepath.Join(t.TempDir(), "wt2")
	if err := ensureWorktree(clone, wt2, "athena/t-0002", "origin/"+def, cloneGitEnv(clone, "o/r")); err != nil {
		t.Fatal(err)
	}
	cleanupWorktree(placement{workDir: wt2, repo: clone, github: "o/r"}, core.Ticket{ID: "T-0002"}, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	clone, _, _, err := n.EnsureClone(ctx, "o/r", fakeToken)
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
	if _, _, _, err := n.EnsureClone(ctx, "o/r", fakeToken); err != nil {
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
	clone, _, _, err := n.EnsureClone(context.Background(), "o/r", fakeToken)
	if err != nil {
		t.Fatalf("clone with the owner's SSH rewrite: %v", err)
	}
	getURL := func(dir string, env []string) string {
		cmd := exec.Command("git", "-C", dir, "ls-remote", "--get-url", "https://github.com/o/r.git")
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	want := githubTarget("o/r")
	mirror := MirrorDir(n.cfg.StateDir, "o/r")
	if got := getURL(mirror, mirrorGitEnv(mirror, "o/r", nil)); got != want {
		t.Errorf("the token-bearing node git reaches %q, want %q", got, want)
	}
	s := &session{t: core.Ticket{ID: "T-1", Repo: "o/r"}}
	if got := getURL(clone, append(os.Environ(), n.githubEnv(s)...)); got != want {
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

// gitCall is one git process the test's wrapper saw.
type gitCall struct {
	args []string
	env  map[string]string
}

// wrapGit puts a git first on PATH that records every call's arguments and environment, and returns a reader of them.
func wrapGit(t *testing.T) func() []gitCall {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(t.TempDir(), "git.log")
	script := "#!/bin/sh\n{ echo '@@ARGS'; for a in \"$@\"; do printf '%s\\n' \"$a\"; done; echo '@@ENV'; env; echo '@@END'; } >> " + log +
		"\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []gitCall {
		b, _ := os.ReadFile(log)
		var calls []gitCall
		var cur *gitCall
		inEnv := false
		for _, l := range strings.Split(string(b), "\n") {
			switch {
			case l == "@@ARGS":
				calls = append(calls, gitCall{env: map[string]string{}})
				cur, inEnv = &calls[len(calls)-1], false
			case l == "@@ENV":
				inEnv = true
			case l == "@@END":
				cur = nil
			case cur != nil && inEnv:
				k, v, _ := strings.Cut(l, "=")
				cur.env[k] = v
			case cur != nil:
				cur.args = append(cur.args, l)
			}
		}
		return calls
	}
}

// config returns the call's command-scope config (GIT_CONFIG_KEY/VALUE_n) as key -> last value, and whether a key is
// set at all.
func (c gitCall) config() map[string]string {
	out := map[string]string{}
	n, _ := strconv.Atoi(c.env["GIT_CONFIG_COUNT"])
	for i := 0; i < n; i++ {
		out[c.env["GIT_CONFIG_KEY_"+strconv.Itoa(i)]] = c.env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)]
	}
	return out
}

func (c gitCall) tokenBearing() bool {
	for _, v := range c.config() {
		if strings.Contains(v, "git-credential") {
			return true
		}
	}
	return false
}

// R26: the token is used only against the node's bare mirror, whose git reads no worker-writable config and pins TLS
// and routing; a worker's http.* in the shared clone reaches only a token-less local fetch.
func TestMirrorFetchIgnoresPlantedHTTPConfig(t *testing.T) {
	for _, v := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(v, "")
	}
	_, work := fakeGitHub(t, "o/r")
	n := testNode(t)
	ctx := context.Background()
	clone, _, _, err := n.EnsureClone(ctx, "o/r", fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	mirror := MirrorDir(n.cfg.StateDir, "o/r")
	git(t, clone, "config", "http.proxy", "http://127.0.0.1:9")
	git(t, clone, "config", "http.https://github.com/.proxy", "http://127.0.0.1:9")
	git(t, clone, "config", "http.sslCAInfo", "/tmp/evil-ca.pem")
	git(t, clone, "config", "http.curloptResolve", "github.com:443:127.0.0.1")
	git(t, work, "commit", "-q", "--allow-empty", "-m", "later")
	git(t, work, "push", "-q", "origin", "trunk")

	calls := wrapGit(t)
	if _, _, _, err := n.EnsureClone(ctx, "o/r", fakeToken); err != nil {
		t.Fatal(err)
	}
	if got, want := git(t, clone, "rev-parse", "refs/remotes/origin/trunk"), git(t, work, "rev-parse", "HEAD"); got != want {
		t.Fatalf("clone origin/trunk = %s, want %s", got, want)
	}
	bearing := 0
	for _, c := range calls() {
		if !c.tokenBearing() {
			continue
		}
		bearing++
		args := strings.Join(c.args, " ")
		onMirror := c.env["GIT_DIR"] == mirror || (slices.Contains(c.args, "clone") && strings.HasSuffix(args, mirror))
		if !onMirror || strings.Contains(args, clone+" ") || strings.HasSuffix(args, clone) {
			t.Errorf("a token-bearing git ran on %q, want only the mirror %s", args, mirror)
		}
		cfg := c.config()
		for k, want := range map[string]string{"http.proxy": "", "http.curloptResolve": "", "core.hooksPath": os.DevNull,
			"http.https://github.com/o/r.git.proxy": "", "protocol.allow": "never"} {
			if got, ok := cfg[k]; !ok || got != want {
				t.Errorf("token-bearing git %q: %s = %q (set %v), want %q", args, k, got, ok, want)
			}
		}
		if c.env["GIT_CONFIG_GLOBAL"] != os.DevNull || c.env["GIT_CONFIG_SYSTEM"] == "" {
			t.Errorf("token-bearing git reads global %q / system %q", c.env["GIT_CONFIG_GLOBAL"], c.env["GIT_CONFIG_SYSTEM"])
		}
	}
	if bearing == 0 {
		t.Fatal("no token-bearing git ran")
	}
	if st, err := os.Stat(mirror); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("mirror: %v %v, want a 0700 directory", err, st)
	}
	if out, _ := exec.Command("git", "-C", mirror, "worktree", "list", "--porcelain").Output(); strings.Count(string(out), "worktree ") != 1 {
		t.Fatalf("worktrees on the mirror:\n%s", out)
	}
}

// The shared clone fetches from the mirror over the file protocol, with no credential helper and no token.
func TestCloneFetchFromMirrorHasNoToken(t *testing.T) {
	fakeGitHub(t, "o/r")
	n := testNode(t)
	calls := wrapGit(t)
	clone, _, _, err := n.EnsureClone(context.Background(), "o/r", fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := n.EnsureClone(context.Background(), "o/r", fakeToken); err != nil {
		t.Fatal(err)
	}
	mirror := MirrorDir(n.cfg.StateDir, "o/r")
	if got := git(t, clone, "config", "--get", "remote.origin.url"); got != mirror {
		t.Fatalf("clone origin = %q, want the mirror %s", got, mirror)
	}
	onClone := 0
	for _, c := range calls() {
		onThisClone := (len(c.args) >= 2 && c.args[0] == "-C" && c.args[1] == clone) ||
			(slices.Contains(c.args, "clone") && c.args[len(c.args)-1] == clone)
		if !onThisClone {
			continue
		}
		onClone++
		if c.tokenBearing() {
			t.Errorf("git on the clone has a credential helper: %q", c.args)
		}
		for k, v := range c.env {
			if strings.Contains(v, fakeToken) || strings.Contains(v, "git-credential") || strings.Contains(v, "github.com") {
				t.Errorf("git on the clone %q: %s=%s", c.args, k, v)
			}
		}
	}
	if onClone < 2 {
		t.Fatalf("saw %d node git calls on the clone, want its clone and fetch", onClone)
	}
}

// A worktree's push goes to GitHub (with the session's own token), not into the local mirror.
func TestWorktreePushTargetsGitHub(t *testing.T) {
	fakeGitHub(t, "O/R")
	n := testNode(t)
	clone, def, _, err := n.EnsureClone(context.Background(), "O/R", fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := ensureWorktree(clone, wt, "athena/t-0001", "origin/"+def, cloneGitEnv(clone, "o/r")); err != nil {
		t.Fatal(err)
	}
	if got := git(t, wt, "remote", "get-url", "--push", "origin"); got != "https://github.com/o/r.git" {
		t.Fatalf("push URL = %q, want https://github.com/o/r.git", got)
	}
	if got := git(t, wt, "remote", "get-url", "origin"); got != MirrorDir(n.cfg.StateDir, "o/r") {
		t.Fatalf("fetch URL = %q, want the mirror", got)
	}
}

// evilRepo makes a bare repository at <dir>/o/r.git: a copy of the real one (so it has its history) plus an evil
// commit on trunk, and returns that commit.
func evilRepo(t *testing.T, realBare string) (dir, evilSHA string) {
	t.Helper()
	dir = t.TempDir()
	bare := filepath.Join(dir, "o", "r.git")
	git(t, dir, "clone", "-q", "--bare", realBare, bare)
	w := t.TempDir()
	git(t, w, "clone", "-q", bare, ".")
	git(t, w, "commit", "-q", "--allow-empty", "-m", "evil")
	git(t, w, "push", "-q", "origin", "HEAD:trunk")
	return dir, git(t, w, "rev-parse", "HEAD")
}

// A worker can reach the mirror's own files by path: what it plants in the mirror's config (a remote URL or proxy
// for the fetch URL, a CA file, an include) or as a commondir is gone before the next token-bearing fetch.
func TestMirrorConfigRewrittenBeforeTokenFetch(t *testing.T) {
	root, work := fakeGitHub(t, "o/r")
	n := testNode(t)
	ctx := context.Background()
	if _, _, _, err := n.EnsureClone(ctx, "o/r", fakeToken); err != nil {
		t.Fatal(err)
	}
	mirror := MirrorDir(n.cfg.StateDir, "o/r")
	evil, evilSHA := evilRepo(t, filepath.Join(root, "o", "r.git"))
	evilURL := "file://" + evil + "/o/r.git"
	inc := filepath.Join(t.TempDir(), "inc")
	if err := os.WriteFile(inc, []byte("[remote \"https://github.com/o/r.git\"]\n\turl = "+evilURL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(mirror, "config")
	for _, kv := range [][2]string{
		{"remote.https://github.com/o/r.git.url", evilURL},
		{"remote.https://github.com/o/r.git.proxy", "http://127.0.0.1:9"},
		{"http.sslCAInfo", "/tmp/evil-ca.pem"},
		{"include.path", inc},
	} {
		git(t, root, "config", "--file", cfg, kv[0], kv[1])
	}
	if err := os.WriteFile(filepath.Join(mirror, "commondir"), []byte(filepath.Join(evil, "o", "r.git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "commit", "-q", "--allow-empty", "-m", "real later")
	git(t, work, "push", "-q", "origin", "trunk")
	want := git(t, work, "rev-parse", "HEAD")

	calls := wrapGit(t)
	_, _, base, err := n.EnsureClone(ctx, "o/r", fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	if base != want || base == evilSHA {
		t.Fatalf("base = %s, want the real %s (evil %s)", base, want, evilSHA)
	}
	b, err := os.ReadFile(cfg)
	if err != nil || string(b) != mirrorConfig {
		t.Fatalf("mirror config = %q, %v; want it rewritten to %q", b, err, mirrorConfig)
	}
	if _, err := os.Stat(filepath.Join(mirror, "commondir")); !os.IsNotExist(err) {
		t.Fatalf("the planted commondir is still there: %v", err)
	}
	cmd := exec.Command("git", "rev-parse", "refs/heads/trunk")
	cmd.Env = append(os.Environ(), "GIT_DIR="+mirror)
	if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != want {
		t.Fatalf("mirror trunk = %s %v, want %s", out, err, want)
	}
	seen := false
	for _, c := range calls() {
		if c.tokenBearing() && slices.Contains(c.args, "fetch") {
			seen = true
			if got, ok := c.config()["remote.https://github.com/o/r.git.proxy"]; !ok || got != "" {
				t.Errorf("token-bearing fetch: remote.<url>.proxy = %q (set %v), want it pinned to \"\"", got, ok)
			}
		}
	}
	if !seen {
		t.Fatal("no token-bearing fetch seen")
	}
}

// A worker's insteadOf for the mirror's path in the shared clone's config does not change the next worktree's base:
// it is the commit the mirror's default branch names; a fetch redirected elsewhere fails the launch instead.
func TestWorktreeBaseFromMirror(t *testing.T) {
	root, work := fakeGitHub(t, "o/r")
	n := testNode(t)
	ctx := context.Background()
	clone, _, _, err := n.EnsureClone(ctx, "o/r", fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	mirror := MirrorDir(n.cfg.StateDir, "o/r")
	evil, evilSHA := evilRepo(t, filepath.Join(root, "o", "r.git"))
	git(t, clone, "config", "url.file://"+evil+"/o/r.git.insteadOf", mirror)
	git(t, work, "commit", "-q", "--allow-empty", "-m", "real later")
	git(t, work, "push", "-q", "origin", "trunk")
	want := git(t, work, "rev-parse", "HEAD")
	_, def, base, err := n.EnsureClone(ctx, "o/r", fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	if base != want {
		t.Fatalf("base = %s, want the mirror's %s (evil %s)", base, want, evilSHA)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := ensureWorktree(clone, wt, "athena/t-0009", base, cloneGitEnv(clone, "o/r")); err != nil {
		t.Fatal(err)
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != want || def != "trunk" {
		t.Fatalf("worktree HEAD = %s (default %q), want %s on trunk", got, def, want)
	}
	// A remote named by the mirror's path, if git takes it, redirects the fetch itself: the mirror's commit is then
	// missing, and the launch fails rather than starting from the worker's history.
	git(t, clone, "config", "--unset", "url.file://"+evil+"/o/r.git.insteadOf")
	git(t, clone, "config", "remote."+mirror+".url", "file://"+evil+"/o/r.git")
	git(t, work, "commit", "-q", "--allow-empty", "-m", "real even later")
	git(t, work, "push", "-q", "origin", "trunk")
	later := git(t, work, "rev-parse", "HEAD")
	if _, _, base, err := n.EnsureClone(ctx, "o/r", fakeToken); err == nil && base != later {
		t.Fatalf("EnsureClone with a redirected clone fetch set base %s, want %s or a failure", base, later)
	} else if err == nil {
		// git did not take the remote for the path: the fetch reached the mirror, and the clone has its commit.
		if got := git(t, clone, "rev-parse", "refs/remotes/origin/trunk"); got != later {
			t.Fatalf("clone origin/trunk = %s, want %s", got, later)
		}
	}
}

// R28: the base of a new worktree is what GitHub advertises, never what the mirror's files say: a planted HEAD naming
// another branch, or a loose refs/heads/<default> written after the fetch, leave it alone.
func TestMirrorBaseFromRemoteAdvertisement(t *testing.T) {
	root, work := fakeGitHub(t, "o/r")
	n := testNode(t)
	ctx := context.Background()
	if _, _, _, err := n.EnsureClone(ctx, "o/r", fakeToken); err != nil {
		t.Fatal(err)
	}
	mirror := MirrorDir(n.cfg.StateDir, "o/r")
	// A worker pushes its own branch to GitHub and points the mirror's HEAD at it.
	git(t, work, "checkout", "-q", "-b", "side")
	git(t, work, "commit", "-q", "--allow-empty", "-m", "worker code")
	git(t, work, "push", "-q", "origin", "side")
	workerSHA := git(t, work, "rev-parse", "HEAD")
	git(t, work, "checkout", "-q", "trunk")
	git(t, work, "commit", "-q", "--allow-empty", "-m", "real later")
	git(t, work, "push", "-q", "origin", "trunk")
	want := git(t, filepath.Join(root, "o", "r.git"), "rev-parse", "refs/heads/trunk")
	if err := os.WriteFile(filepath.Join(mirror, "HEAD"), []byte("ref: refs/heads/side\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, def, base, err := n.EnsureClone(ctx, "o/r", fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	if def != "trunk" || base != want {
		t.Fatalf("planted HEAD: default %q base %s; want trunk %s (worker's %s)", def, base, want, workerSHA)
	}
	if b, _ := os.ReadFile(filepath.Join(mirror, "HEAD")); string(b) != "ref: refs/heads/trunk\n" {
		t.Fatalf("mirror HEAD = %q, want it set from the advertisement", b)
	}

	// A loose ref written between the fetch and anything that would read it.
	n.afterMirrorFetch = func(m string) {
		if err := os.WriteFile(filepath.Join(m, "refs", "heads", "trunk"), []byte(workerSHA+"\n"), 0o600); err != nil {
			t.Error(err)
		}
	}
	_, def, base, err = n.EnsureClone(ctx, "o/r", fakeToken)
	n.afterMirrorFetch = nil
	if err != nil {
		t.Fatal(err)
	}
	if def != "trunk" || base != want {
		t.Fatalf("planted loose ref: default %q base %s; want trunk %s (worker's %s)", def, base, want, workerSHA)
	}
}

// writeFileAtomic writes through a fresh temporary file: a symlink planted at any name it might use, or at the
// destination, is not followed, and a planted directory does not stop it.
func TestWriteFileAtomicDoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	dst := filepath.Join(dir, "config")
	var victims []string
	for i, name := range []string{"config", "config.athena-tmp", "config.tmp", "config~", ".config.tmp"} {
		v := filepath.Join(outside, "victim"+strconv.Itoa(i))
		victims = append(victims, v)
		if err := os.Symlink(v, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeFileAtomic(dst, mirrorConfig); err != nil {
		t.Fatal(err)
	}
	for _, v := range victims {
		if _, err := os.Lstat(v); !os.IsNotExist(err) {
			t.Errorf("a planted symlink was followed: %s exists (%v)", v, err)
		}
	}
	st, err := os.Lstat(dst)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 {
		t.Fatalf("destination: %v %v, want a regular 0600 file", err, st)
	}
	if b, _ := os.ReadFile(dst); string(b) != mirrorConfig {
		t.Fatalf("destination = %q", b)
	}
	// A directory where a fixed temporary name would go.
	d2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d2, "config.athena-tmp", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(d2, "config"), "x"); err != nil {
		t.Fatalf("a planted directory stopped the write: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "config.athena-") && e.Name() != "config.athena-tmp" {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}
