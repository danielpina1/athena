package node

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/danielpina1/athena/internal/core"
)

// wipWorktree is a worktree of a new repository (which ignores *.log) on branch athena/t-1, with work in progress: a
// tracked file changed, a new file staged, an untracked file and an ignored one.
func wipWorktree(t *testing.T) (repo, dir string) {
	t.Helper()
	repo = newRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".gitignore")
	git(t, repo, "commit", "-q", "-m", "ignore logs")
	dir = filepath.Join(t.TempDir(), "wt")
	if err := ensureWorktree(repo, dir, "athena/t-1", "HEAD", nil); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"README": "changed\n", "staged.txt": "staged\n", "new.txt": "new\n", "debug.log": "noise\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", "staged.txt")
	return repo, dir
}

// files reads the worktree's files (its .git file aside).
func files(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

// A checkpoint leaves the branch, the index and the files as they were: it goes through an index of its own.
func TestCheckpointKeepsBranchIndexAndFiles(t *testing.T) {
	repo, dir := wipWorktree(t)
	head, branch := git(t, dir, "rev-parse", "HEAD"), git(t, repo, "rev-parse", "refs/heads/athena/t-1")
	status, staged := git(t, dir, "status", "--porcelain", "--untracked-files=all"), git(t, dir, "diff", "--cached", "--name-status")
	before := files(t, dir)

	sha, err := checkpoint(dir, "T-1", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if sha == "" || sha == head {
		t.Fatalf("checkpoint = %q (HEAD %s)", sha, head)
	}
	if got := git(t, dir, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved: %s, want %s", got, head)
	}
	if got := git(t, repo, "rev-parse", "refs/heads/athena/t-1"); got != branch {
		t.Errorf("branch moved: %s, want %s", got, branch)
	}
	if got := git(t, dir, "status", "--porcelain", "--untracked-files=all"); got != status {
		t.Errorf("status:\n%s\nwant\n%s", got, status)
	}
	if got := git(t, dir, "diff", "--cached", "--name-status"); got != staged {
		t.Errorf("staged:\n%s\nwant\n%s", got, staged)
	}
	after := files(t, dir)
	if len(after) != len(before) {
		t.Errorf("files %v, want %v", after, before)
	}
	for name, body := range before {
		if after[name] != body {
			t.Errorf("%s = %q, want %q", name, after[name], body)
		}
	}
}

// The checkpoint ref names a commit on HEAD whose tree is the work in progress: changed, staged and untracked files,
// not the ignored one. The node's git directories, pinned, reach the same.
func TestCheckpointRefPointsAtWIP(t *testing.T) {
	repo, dir := wipWorktree(t)
	head := git(t, dir, "rev-parse", "HEAD")
	common := filepath.Join(repo, ".git")
	admin, err := findWorktreeAdmin(common, dir, filepath.Dir(dir), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	sha, err := checkpoint(dir, "T-1", worktreeGitEnv(nodeGitBaseEnv(), admin, common, dir), "test")
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, repo, "rev-parse", "refs/athena/t-1/checkpoint"); got != sha {
		t.Fatalf("ref = %s, want %s", got, sha)
	}
	if got := git(t, repo, "rev-parse", sha+"^"); got != head {
		t.Errorf("parent = %s, want HEAD %s", got, head)
	}
	if got := git(t, repo, "log", "-1", "--format=%s", sha); got != "checkpoint" {
		t.Errorf("message = %q", got)
	}
	tree := strings.Fields(git(t, repo, "ls-tree", "--name-only", sha))
	want := []string{".gitignore", "README", "new.txt", "staged.txt"}
	if strings.Join(tree, " ") != strings.Join(want, " ") {
		t.Errorf("tree = %v, want %v", tree, want)
	}
	if got := git(t, repo, "show", sha+":README"); got != "changed" {
		t.Errorf("README in the checkpoint = %q", got)
	}
	// A second checkpoint moves the ref to the newer work.
	if err := os.WriteFile(filepath.Join(dir, "more.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := checkpoint(dir, "T-1", nil, "test")
	if err != nil || again == sha || git(t, repo, "rev-parse", "refs/athena/t-1/checkpoint") != again {
		t.Fatalf("second checkpoint %s (%v), ref %s", again, err, git(t, repo, "rev-parse", "refs/athena/t-1/checkpoint"))
	}
	// Not a git directory: an error, nothing written.
	if _, err := checkpoint(t.TempDir(), "T-2", nil, "test"); err == nil {
		t.Fatal("a checkpoint of a plain directory succeeded")
	}
}

// The checkpoint ref goes with the Ticket's worktree: removed with it, kept while the worktree is kept.
func TestCleanupRemovesTheCheckpointRef(t *testing.T) {
	repo := newRepo(t)
	root := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	for _, c := range []struct {
		id    string
		dirty bool
	}{{"T-1", false}, {"T-2", true}} {
		tk := core.Ticket{ID: c.id}
		dir := filepath.Join(root, strings.ToLower(c.id))
		if err := ensureWorktree(repo, dir, branchName(tk), "HEAD", nil); err != nil {
			t.Fatal(err)
		}
		if c.dirty {
			if err := os.WriteFile(filepath.Join(dir, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		sha, err := checkpoint(dir, c.id, nil, "test")
		if err != nil {
			t.Fatal(err)
		}
		// The seed's refs go too; another Ticket's whose id starts alike stays.
		git(t, repo, "update-ref", ticketRefs(c.id)+"superseded-1", sha)
		git(t, repo, "update-ref", ticketRefs(c.id)+"seed", sha)
		git(t, repo, "update-ref", checkpointRef(c.id+"0"), sha)
		cleanupWorktree(placement{workDir: dir, repo: repo, common: filepath.Join(repo, ".git")}, tk, root, log)
		_, statErr := os.Stat(dir)
		refs := git(t, repo, "for-each-ref", "--format=%(refname)", ticketRefs(c.id))
		if other := git(t, repo, "for-each-ref", checkpointRef(c.id+"0")); other == "" {
			t.Errorf("%s: another Ticket's checkpoint went with it", c.id)
		}
		if c.dirty && (statErr != nil || !strings.Contains(refs, checkpointRef(c.id))) {
			t.Errorf("%s: kept worktree (%v) lost its checkpoint (refs %q)", c.id, statErr, refs)
		}
		if !c.dirty && (!os.IsNotExist(statErr) || refs != "") {
			t.Errorf("%s: removed worktree (%v) left its checkpoint (refs %q)", c.id, statErr, refs)
		}
	}
}

// A review session that stops at its limit writes nothing to checkpoint: its checkpoint would only re-add the
// implementer's tree and move the Ticket's side ref. A worker's session is checkpointed.
func TestReviewSessionIsNotCheckpointed(t *testing.T) {
	h := newHarness(t, nil)
	repo, dir := wipWorktree(t)
	p := placement{workDir: dir, repo: repo, common: filepath.Join(repo, ".git")}
	tk := core.Ticket{ID: "T-1"}
	ref := checkpointRef(tk.ID)
	h.n.checkpointSession(h.n.newSession(tk, core.Assignment{ID: "r1", Role: core.RoleReview}, p))
	if refs := git(t, repo, "for-each-ref", ref); refs != "" {
		t.Fatalf("a review was checkpointed: %q", refs)
	}
	h.n.checkpointSession(h.n.newSession(tk, core.Assignment{ID: "a1"}, p))
	if refs := git(t, repo, "for-each-ref", ref); refs == "" {
		t.Fatal("the worker was not checkpointed")
	}
}

// checkpointCall is one OnCheckpoint call.
type checkpointCall struct{ ticketID, repo, ref, sha string }

// Every turn of a Ticket with a Repo that ends well checkpoints its work in progress and tells OnCheckpoint: two turns,
// two calls (at least: a turn that files no Report is nudged into one more). The branch never moves, and the latest
// checkpoint holds what the worktree held at the second turn's end. CheckpointBundle then bundles it, whole or after
// a base.
func TestCheckpointEveryTurnEnd(t *testing.T) {
	var mu sync.Mutex
	var calls []checkpointCall
	got := func() []checkpointCall {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
	h := newGHHarness(t, &fakeCreds{}, func(c *Config) {
		c.OnCheckpoint = func(ticketID, repo, ref, sha string) {
			mu.Lock()
			calls = append(calls, checkpointCall{ticketID, repo, ref, sha})
			mu.Unlock()
		}
	})
	t.Setenv("ATHENA_FAKE_LATER_FIXTURE", claudeFixture(t, "simple-turn.jsonl"))
	tk, _ := h.accept("github:issuecomment:1", "o/r", 12, "")
	h.eventually("the first turn's checkpoint", func() bool { return len(got()) >= 1 })
	wd, err := WorkingDir(h.cfg.WorkRoot, tk)
	if err != nil {
		t.Fatal(err)
	}
	branch := git(t, wd, "rev-parse", "refs/heads/"+branchName(tk))
	first := len(got())
	if err := os.WriteFile(filepath.Join(wd, "wip.txt"), []byte("half done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.AddPersonInput(tk.ID, "keep going"); err != nil {
		t.Fatal(err)
	}
	h.eventually("the second turn's checkpoint", func() bool {
		cs := got()
		if len(cs) <= first {
			return false
		}
		return strings.Contains(git(t, wd, "ls-tree", "--name-only", cs[len(cs)-1].sha), "wip.txt")
	})
	cs := got()
	ref := "refs/athena/" + strings.ToLower(tk.ID) + "/checkpoint"
	for _, c := range cs {
		if c.ticketID != tk.ID || c.repo != "o/r" || c.ref != ref || c.sha == "" {
			t.Fatalf("call %+v", c)
		}
		if parent := git(t, wd, "rev-parse", c.sha+"^"); parent != branch {
			t.Fatalf("checkpoint %s is on %s, not the branch's head %s", c.sha, parent, branch)
		}
	}
	if now := git(t, wd, "rev-parse", "refs/heads/"+branchName(tk)); now != branch {
		t.Fatalf("the branch moved: %s, was %s", now, branch)
	}
	if got := git(t, wd, "rev-parse", "HEAD"); got != branch {
		t.Fatalf("HEAD moved to %s", got)
	}
	last := cs[len(cs)-1].sha
	if git(t, wd, "rev-parse", ref) != last {
		t.Fatal("the ref is not at the last checkpoint")
	}

	// The whole bundle, then one after a base; the same sha again is unchanged.
	ctx := context.Background()
	var full bytes.Buffer
	sha, err := h.n.CheckpointBundle(ctx, tk.ID, "", &full)
	if err != nil || sha != last || full.Len() == 0 {
		t.Fatalf("CheckpointBundle = %s, %v (%d bytes); want %s", sha, err, full.Len(), last)
	}
	var incr bytes.Buffer
	if sha, err := h.n.CheckpointBundle(ctx, tk.ID, cs[0].sha, &incr); err != nil || sha != last || incr.Len() == 0 {
		t.Fatalf("incremental CheckpointBundle = %s, %v", sha, err)
	}
	if _, err := h.n.CheckpointBundle(ctx, tk.ID, last, io.Discard); !errors.Is(err, ErrCheckpointUnchanged) {
		t.Fatalf("CheckpointBundle at the base = %v, want ErrCheckpointUnchanged", err)
	}
	if _, err := h.n.CheckpointBundle(ctx, "T-9999", "", io.Discard); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("CheckpointBundle of an unknown Ticket = %v, want ErrNoCheckpoint", err)
	}
	// Past the cap, git is stopped and nothing is written.
	defer func(old int64) { bundleCap = old }(bundleCap)
	bundleCap = int64(full.Len()) / 2
	var cut bytes.Buffer
	if _, err := h.n.CheckpointBundle(ctx, tk.ID, "", &cut); !errors.Is(err, ErrCheckpointTooBig) || int64(cut.Len()) > bundleCap+1 {
		t.Fatalf("CheckpointBundle over the cap = %v, %d bytes written", err, cut.Len())
	}
}

// A checkpoint of an unchanged worktree is the one already made: its ref does not move.
func TestCheckpointUnchangedReused(t *testing.T) {
	_, dir := wipWorktree(t)
	first, err := checkpoint(dir, "T-1", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	again, err := checkpoint(dir, "T-1", nil, "test")
	if err != nil || again != first {
		t.Fatalf("second checkpoint = %s, %v; want %s", again, err, first)
	}
}

// The index of a checkpoint starts from the worktree's own: a tracked file deleted, staged or not, is gone from the
// checkpoint, and the worktree's index is as it was.
func TestCheckpointFromWorktreeIndex(t *testing.T) {
	_, dir := wipWorktree(t)
	if err := os.Remove(filepath.Join(dir, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	staged := git(t, dir, "diff", "--cached", "--name-status")
	sha, err := checkpoint(dir, "T-1", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	tree := git(t, dir, "ls-tree", "--name-only", sha)
	if strings.Contains(tree, ".gitignore") || !strings.Contains(tree, "staged.txt") || !strings.Contains(tree, "new.txt") {
		t.Fatalf("tree = %q", tree)
	}
	if got := git(t, dir, "diff", "--cached", "--name-status"); got != staged {
		t.Fatalf("the worktree's index changed:\n%s\nwant\n%s", got, staged)
	}
}
