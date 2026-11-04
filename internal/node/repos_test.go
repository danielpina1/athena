package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGitHub makes <root>/<owner>/<name>.git, a bare repository whose default branch is trunk (one commit) beside a
// branch other with one more, and points https://github.com/ at root through GIT_CONFIG_* for every git this test
// process starts: no test reaches the network. It returns root and a work clone that pushes to the bare repository.
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
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url.file://"+root+"/.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/")
	work = t.TempDir()
	git(t, work, "clone", "-q", bare, ".")
	return root, work
}

const fakeToken = "ghs_n0deT0kenNeverShown0123456789abcdef"

func TestCloneUsesHelperNotURL(t *testing.T) {
	_, work := fakeGitHub(t, "o/r")
	state := t.TempDir()
	ctx := context.Background()
	dir, def, err := EnsureClone(ctx, state, "O/R", fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(state, "repos", "o", "r"); dir != want || def != "trunk" {
		t.Fatalf("EnsureClone = %q, %q; want %q, trunk", dir, def, want)
	}
	// The configured URL, before insteadOf rewrites it to the stand-in.
	if got := git(t, dir, "config", "--get", "remote.origin.url"); got != "https://github.com/O/R.git" {
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
	// The helper the node's git runs with answers with the token, and only through git's credential protocol.
	cmd := exec.Command("git", "credential", "fill")
	cmd.Env = append(cloneEnv(fakeToken), "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=O/R.git\n\n")
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), "username=x-access-token\npassword="+fakeToken+"\n") {
		t.Fatalf("git credential fill with the node's helper = %q, %v", out, err)
	}

	// A second call fetches: a commit pushed meanwhile is on origin/trunk.
	git(t, work, "commit", "-q", "--allow-empty", "-m", "later")
	git(t, work, "push", "-q", "origin", "trunk")
	if _, _, err := EnsureClone(ctx, state, "o/r", fakeToken); err != nil {
		t.Fatal(err)
	}
	if got, want := git(t, dir, "rev-parse", "origin/trunk"), git(t, work, "rev-parse", "HEAD"); got != want {
		t.Fatalf("origin/trunk = %s after the second EnsureClone, want %s", got, want)
	}
}

func TestCloneRefusesBadRepository(t *testing.T) {
	for _, repo := range []string{"", "o", "o/r/x", "../r", "o/..", "o/r;rm", "-o/r x"} {
		if _, _, err := EnsureClone(context.Background(), t.TempDir(), repo, fakeToken); err == nil {
			t.Errorf("EnsureClone(%q) succeeded", repo)
		}
	}
}

func TestWorktreeFromDefaultBranch(t *testing.T) {
	_, work := fakeGitHub(t, "o/r")
	state := t.TempDir()
	ctx := context.Background()
	if _, _, err := EnsureClone(ctx, state, "o/r", fakeToken); err != nil {
		t.Fatal(err)
	}
	// The default branch moves after the clone; the next worktree starts from where it is now.
	git(t, work, "commit", "-q", "--allow-empty", "-m", "newer trunk")
	git(t, work, "push", "-q", "origin", "trunk")
	clone, def, err := EnsureClone(ctx, state, "o/r", fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := ensureWorktree(clone, wt, "athena/t-0001", "origin/"+def); err != nil {
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
