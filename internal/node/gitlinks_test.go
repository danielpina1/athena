package node

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/runtime/rt"
)

// tamperRuntime is Codex whose session, once started, does what a worker's indirect write (a script, a test) could:
// it repoints the worktree's .git file and its git directory's commondir and gitdir at a git directory of its own,
// rewrites the git directory's config.worktree to run a program, and leaves an untracked file, so the worktree is kept.
type tamperRuntime struct {
	rt.Runtime
	evil, marker string
	t            *testing.T
}

func (r tamperRuntime) Start(ctx context.Context, o rt.StartOptions) (rt.Session, error) {
	sess, err := r.Runtime.Start(ctx, o)
	if err != nil {
		return sess, err
	}
	gitDir, _ := gitDirs(o.Dir)
	for path, body := range map[string]string{
		filepath.Join(gitDir, "commondir"):       r.evil + "\n",
		filepath.Join(gitDir, "gitdir"):          filepath.Join(r.evil, ".git") + "\n",
		filepath.Join(gitDir, "config.worktree"): "[core]\n\tfsmonitor = touch " + r.marker + "\n",
		filepath.Join(o.Dir, ".git"):             "gitdir: " + r.evil + "\n",
		filepath.Join(o.Dir, "wip.txt"):          "half done\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			r.t.Errorf("tamper %s: %v", path, err)
		}
	}
	return sess, nil
}

// evilGitDir is a git directory of the worker's whose config runs a program that touches marker on any git status.
func evilGitDir(t *testing.T, marker string) string {
	t.Helper()
	evil := t.TempDir()
	git(t, evil, "init", "-q", "--bare")
	git(t, evil, "config", "core.fsmonitor", "touch "+marker)
	git(t, evil, "config", "core.hooksPath", evil)
	return evil
}

// I3: a Codex session that repointed its worktree's git links and rewrote its config.worktree (writes the hook rules
// cannot see) leaves none of it behind: once its process ends, the node puts the links back from the placement record
// and config.worktree back to what git made, before it keeps the worktree. The owner's own git there then runs
// nothing of the worker's.
func TestCodexSessionGitLinksAreRestored(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	evil := evilGitDir(t, marker)
	h := newCodexHarness(t, codexTurn(), func(c *Config) {
		c.Runtimes[core.RuntimeCodex] = tamperRuntime{Runtime: c.Runtimes[core.RuntimeCodex], evil: evil, marker: marker, t: t}
	})
	repo := newRepo(t)
	// A repository with per-worktree config: git copies the main worktree's config.worktree into a new worktree's.
	git(t, repo, "config", "extensions.worktreeConfig", "true")
	git(t, repo, "config", "--worktree", "core.sparseCheckout", "false")
	tk, err := h.eng.SubmitOwnerTask("Fix the parser", core.TaskOptions{Dir: repo, Runtime: core.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	_, a := h.running(tk.ID)
	wd, err := WorkingDir(h.cfg.WorkRoot, tk)
	if err != nil {
		t.Fatal(err)
	}
	common := filepath.Join(repo, ".git")
	admin := worktreeAdmin(common, wd)
	// The record holds config.worktree as git made it, before the session ran.
	rec, _, err := h.n.recordedPlacement(tk)
	if err != nil || rec.worktreeConfig == nil || !strings.Contains(*rec.worktreeConfig, "sparseCheckout") ||
		strings.Contains(*rec.worktreeConfig, "fsmonitor") {
		t.Fatalf("recorded config.worktree = %v (%v)", rec.worktreeConfig, err)
	}
	made := *rec.worktreeConfig
	h.eventually("the session tampered", func() bool { _, err := os.Stat(filepath.Join(wd, "wip.txt")); return err == nil })
	if b, _ := os.ReadFile(filepath.Join(wd, ".git")); !strings.Contains(string(b), evil) {
		t.Fatalf(".git during the session = %q, want the worker's", b)
	}
	if r := h.report(tk.ID, a.ID, core.ReportReady, "done"); !r.OK {
		t.Fatalf("report = %+v", r)
	}
	h.eventually("the ticket ends and its session is gone", func() bool {
		cur, _ := h.ticket(tk.ID)
		return cur.Ended != nil && h.n.liveSession(tk.ID) == nil
	})
	h.eventually("the .git file names Athena's git directory", func() bool {
		b, _ := os.ReadFile(filepath.Join(wd, ".git"))
		return strings.Contains(string(b), admin)
	})
	if _, err := os.Stat(filepath.Join(wd, "wip.txt")); err != nil {
		t.Fatalf("the worktree with changes was not kept: %v", err)
	}
	links := map[string]string{filepath.Join(admin, "gitdir"): filepath.Join(wd, ".git"), filepath.Join(admin, "commondir"): common}
	for path, want := range links {
		b, err := os.ReadFile(path)
		if got := strings.TrimSpace(string(b)); err != nil || !sameFile(got, want) {
			t.Errorf("%s = %q (%v), want %s", path, got, err, want)
		}
	}
	if b, err := os.ReadFile(filepath.Join(admin, "config.worktree")); err != nil || string(b) != made {
		t.Errorf("config.worktree = %q (%v), want what git made: %q", b, err, made)
	}
	// The owner's git, run plainly in the kept worktree, finds the repository and runs nothing of the worker's.
	if got := git(t, wd, "rev-parse", "--path-format=absolute", "--git-common-dir"); !sameFile(got, common) {
		t.Errorf("owner's git common dir = %s, want %s", got, common)
	}
	git(t, wd, "status", "--porcelain")
	if _, err := os.Stat(marker); err == nil {
		t.Error("the owner's git ran the worker's program")
	}
}

// restoreGitLinks replaces a link that became a symlink or a directory, removes a config.worktree git did not make,
// leaves alone a config.worktree it has no record of, and does nothing for a Ticket without a worktree.
func TestRestoreGitLinksReplacesAnyShape(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := newHarness(t, nil)
	repo := newRepo(t)
	tk := core.Ticket{ID: "T-0042", Title: "x", Execution: &core.Execution{Directory: repo}}
	p, err := h.n.place(tk)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureWorktreeIn(p.repo, p.common, p.workDir, branchName(tk), "HEAD", nil); err != nil {
		t.Fatal(err)
	}
	h.n.recordWorktreeConfig(tk, log)
	if rec, _, _ := h.n.recordedPlacement(tk); rec.worktreeConfig == nil || *rec.worktreeConfig != "" {
		t.Fatalf("recorded config.worktree = %v, want none recorded as \"\"", rec.worktreeConfig)
	}
	admin := worktreeAdmin(p.common, p.workDir)
	dotGit := filepath.Join(p.workDir, ".git")
	evil := t.TempDir()
	if err := os.Remove(dotGit); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, dotGit); err != nil { // .git: a symlink to the worker's directory
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(admin, "commondir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(admin, "commondir"), 0o755); err != nil { // commondir: a directory
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admin, "config.worktree"), []byte("[core]\n\thooksPath = /tmp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.n.restoreGitLinks(tk, log)
	if st, err := os.Lstat(dotGit); err != nil || !st.Mode().IsRegular() {
		t.Fatalf(".git is %v (%v), want a regular file", st, err)
	}
	if got := git(t, p.workDir, "rev-parse", "--path-format=absolute", "--git-common-dir"); !sameFile(got, p.common) {
		t.Errorf("git common dir = %s, want %s", got, p.common)
	}
	if _, err := os.Lstat(filepath.Join(admin, "config.worktree")); err == nil {
		t.Error("a config.worktree git did not make is still there")
	}
	if _, err := os.Stat(filepath.Join(evil, "x")); err == nil {
		t.Error("the restore wrote through the symlink")
	}

	// A record from before the node kept config.worktree: it is left alone.
	rec, _, _ := h.n.recordedPlacement(tk)
	rec.worktreeConfig = nil
	if err := h.n.writePlacement(tk, rec); err != nil {
		t.Fatal(err)
	}
	cfg := "[core]\n\tsparseCheckout = true\n"
	if err := os.WriteFile(filepath.Join(admin, "config.worktree"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	h.n.restoreGitLinks(tk, log)
	if b, _ := os.ReadFile(filepath.Join(admin, "config.worktree")); string(b) != cfg {
		t.Errorf("unrecorded config.worktree = %q, want it left alone", b)
	}

	// A Ticket in a plain directory has no links.
	plain := core.Ticket{ID: "T-0043", Title: "y"}
	if _, err := h.n.place(plain); err != nil {
		t.Fatal(err)
	}
	h.n.restoreGitLinks(plain, log)
}

// A launch puts the worktree's git links back before it builds the session's sandbox from them: links a session left
// repointed (its node died before the process-end restore) never become the next session's writable roots. Links
// whose git directory cannot be found refuse the launch, and the worktree's .git is set aside.
func TestPrepareRestoresGitLinksFirst(t *testing.T) {
	h := newHarness(t, nil)
	repo := newRepo(t)
	tk := h.submit("Fix the parser", repo)
	tk, a := h.running(tk.ID)
	p, err := h.n.place(tk)
	if err != nil {
		t.Fatal(err)
	}
	common := filepath.Join(repo, ".git")
	admin := worktreeAdmin(common, p.workDir)
	dotGit := filepath.Join(p.workDir, ".git")
	evil := t.TempDir()
	git(t, evil, "init", "-q", "--bare")
	for path, body := range map[string]string{
		dotGit:                            "gitdir: " + evil + "\n",
		filepath.Join(admin, "commondir"): evil + "\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.n.prepare(h.n.newSession(tk, a, p)); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	gitDir, gotCommon := gitDirs(p.workDir)
	if !sameFile(gitDir, admin) || !sameFile(gotCommon, common) {
		t.Fatalf("after prepare the git dirs sessionSandbox reads = %s, %s; want %s, %s", gitDir, gotCommon, admin, common)
	}

	// The git directory gone (or replaced past finding): the launch is refused and .git is set aside.
	if err := os.RemoveAll(admin); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dotGit, []byte("gitdir: "+evil+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.n.prepare(h.n.newSession(tk, a, p)); err == nil {
		t.Fatal("prepare launched with git links it could not check")
	}
	if _, err := os.Lstat(dotGit); !os.IsNotExist(err) {
		t.Errorf(".git is still in place (%v), want it set aside", err)
	}
	if _, err := os.Lstat(dotGit + ".untrusted"); err != nil {
		t.Errorf(".git.untrusted: %v", err)
	}
}
