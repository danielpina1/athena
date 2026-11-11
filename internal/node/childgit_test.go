package node

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielpina1/athena/internal/core"
)

// TestChildWorktreeFromBase: a child's worktree starts at its Execution.Base, not at the repository's HEAD; a Base the
// repository lacks fails the launch; with no Base the worktree starts from HEAD as before.
func TestChildWorktreeFromBase(t *testing.T) {
	repo := newRepo(t)
	base := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "main moves on")
	head := git(t, repo, "rev-parse", "HEAD")
	common := filepath.Join(repo, ".git")
	root := t.TempDir()

	child := core.Ticket{ID: "T-2", Lead: "T-1", Execution: &core.Execution{Directory: repo, Base: base}}
	wt := filepath.Join(root, "t-2")
	if err := repoWorktree(placement{workDir: wt, repo: repo, common: common}, child); err != nil {
		t.Fatal(err)
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != base {
		t.Fatalf("child HEAD = %s, want its base %s", got, base)
	}
	if got := git(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "athena/t-2" {
		t.Fatalf("child branch = %s", got)
	}

	missing := strings.Repeat("d", 40)
	lost := core.Ticket{ID: "T-3", Lead: "T-1", Execution: &core.Execution{Directory: repo, Base: missing}}
	err := repoWorktree(placement{workDir: filepath.Join(root, "t-3"), repo: repo, common: common}, lost)
	if err == nil || err.Error() != "the lead's commit ddddddd is not on this node" {
		t.Fatalf("a missing base: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "t-3")); !os.IsNotExist(err) {
		t.Fatalf("a worktree was made for a missing base: %v", err)
	}

	plain := core.Ticket{ID: "T-4", Execution: &core.Execution{Directory: repo}}
	wt4 := filepath.Join(root, "t-4")
	if err := repoWorktree(placement{workDir: wt4, repo: repo, common: common}, plain); err != nil {
		t.Fatal(err)
	}
	if got := git(t, wt4, "rev-parse", "HEAD"); got != head {
		t.Fatalf("plain HEAD = %s, want %s", got, head)
	}
}

// leadCheckpoint makes, in a clone of repo standing for the lead's node, a commit on the lead's branch and work in
// progress on top, checkpointed; it returns the lead's HEAD, the checkpoint and a bundle of the checkpoint ref.
func leadCheckpoint(t *testing.T, repo, leadID, msg string) (head, cp, bundle string) {
	t.Helper()
	other := t.TempDir()
	git(t, other, "clone", "-q", repo, ".")
	git(t, other, "checkout", "-q", "-b", branchName(core.Ticket{ID: leadID}))
	writeFile(t, filepath.Join(other, "lead.txt"), "committed by the lead\n")
	git(t, other, "add", "lead.txt")
	git(t, other, "commit", "-q", "-m", msg)
	head = git(t, other, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(other, "wip.txt"), "not committed\n")
	var err error
	if cp, err = checkpoint(other, leadID, nil, "test"); err != nil {
		t.Fatal(err)
	}
	bundle = filepath.Join(t.TempDir(), "lead.bundle")
	git(t, other, "bundle", "create", "-q", bundle, checkpointRef(leadID))
	return head, cp, bundle
}

// TestForkSeedStartsAtBase: a child seeded with a fork of its lead's checkpoint starts its own branch at the lead's
// commit, with nothing of the lead's uncommitted work restored, the seed ref gone, and the fork line for its first
// message.
func TestForkSeedStartsAtBase(t *testing.T) {
	src := newRepo(t)
	base, cp, bundle := leadCheckpoint(t, src, "T-1", "lead work")
	clone := t.TempDir()
	git(t, clone, "clone", "-q", src, ".")
	def := git(t, clone, "rev-parse", "HEAD")

	n := New(Config{WorkRoot: t.TempDir(), StateDir: t.TempDir()}, nil)
	child := core.Ticket{ID: "T-2", Lead: "T-1", Repo: "o/r", Execution: &core.Execution{Base: base}}
	a := core.Assignment{ID: "a1"}
	if err := n.Seed(Seed{TicketID: child.ID, AssignmentID: a.ID, Bundle: bundle, SHA: cp, Fork: true, Base: base, From: "studio"}); err != nil {
		t.Fatal(err)
	}
	rec, ok := n.seedFor(child, a.ID)
	if !ok || !rec.Fork || rec.Base != base {
		t.Fatalf("seed record = %+v, %v", rec, ok)
	}
	wt := filepath.Join(t.TempDir(), "t-2")
	s := n.newSession(child, a, placement{workDir: wt, repo: clone, common: filepath.Join(clone, ".git")})
	if err := n.seedWorktree(s, clone, "main", def, nil, rec); err != nil {
		t.Fatal(err)
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != base {
		t.Fatalf("child HEAD = %s, want the lead's commit %s", got, base)
	}
	if got := git(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "athena/t-2" {
		t.Fatalf("child branch = %s", got)
	}
	if st := git(t, wt, "status", "--porcelain"); st != "" {
		t.Fatalf("the child's worktree is not clean: %q", st)
	}
	if _, err := os.Stat(filepath.Join(wt, "wip.txt")); !os.IsNotExist(err) {
		t.Fatalf("the lead's uncommitted work was restored: %v", err)
	}
	if out := git(t, clone, "for-each-ref", "refs/athena/"); out != "" {
		t.Fatalf("refs left in the clone: %s", out)
	}
	note, fork := n.takeMovedNote(s)
	if want := "Your branch starts from T-1's branch at " + base[:7] + ", fetched from the server."; note != want || !fork {
		t.Fatalf("note = %q (fork %v), want %q", note, fork, want)
	}

	// A fork seed must carry the lead's checkpoint and a commit to start at.
	for _, sd := range []Seed{
		{TicketID: "T-2", AssignmentID: "a2", Fork: true, Base: base},
		{TicketID: "T-2", AssignmentID: "a2", Bundle: bundle, SHA: cp, Fork: true, Base: "HEAD"},
	} {
		if err := n.Seed(sd); err == nil {
			t.Errorf("Seed(%+v) was accepted", sd)
		}
	}
}

// TestForkSeedBaseNotInBundle: a fork seed whose base its bundle does not hold, on a node without that commit, fails
// the launch with the lead's commit named.
func TestForkSeedBaseNotInBundle(t *testing.T) {
	src := newRepo(t)
	_, cp, bundle := leadCheckpoint(t, src, "T-1", "lead work")
	clone := t.TempDir()
	git(t, clone, "clone", "-q", src, ".")
	n := New(Config{WorkRoot: t.TempDir(), StateDir: t.TempDir()}, nil)
	missing := strings.Repeat("e", 40)
	child := core.Ticket{ID: "T-2", Lead: "T-1", Repo: "o/r", Execution: &core.Execution{Base: missing}}
	a := core.Assignment{ID: "a1"}
	if err := n.Seed(Seed{TicketID: child.ID, AssignmentID: a.ID, Bundle: bundle, SHA: cp, Fork: true, Base: missing, From: "studio"}); err != nil {
		t.Fatal(err)
	}
	rec, _ := n.seedFor(child, a.ID)
	wt := filepath.Join(t.TempDir(), "t-2")
	s := n.newSession(child, a, placement{workDir: wt, repo: clone, common: filepath.Join(clone, ".git")})
	err := n.seedWorktree(s, clone, "main", git(t, clone, "rev-parse", "HEAD"), nil, rec)
	if err == nil || !strings.Contains(err.Error(), "the lead's commit eeeeeee is not on this node") {
		t.Fatalf("seedWorktree = %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("a worktree was made: %v", err)
	}
}

// TestImportBranch: a child's branch shipped as a bundle of its checkpoint becomes athena/<child> in the lead's
// repository; the same commit again is a no-op, and a branch a worktree here has checked out is never moved.
func TestImportBranch(t *testing.T) {
	repo := newRepo(t)
	lead := core.Ticket{ID: "T-1", Execution: &core.Execution{Directory: repo}}
	n := New(Config{WorkRoot: t.TempDir(), StateDir: t.TempDir()}, ticketEngine{tickets: map[string]core.Ticket{"T-1": lead}})

	childHead, _, bundle := leadCheckpoint(t, repo, "T-2", "child work") // the child's work, checkpointed on its node
	bi := BranchImport{TicketID: "T-1", Child: "T-2", SHA: childHead, Bundle: bundle}
	if err := n.ImportBranch(bi); err != nil {
		t.Fatal(err)
	}
	if got := git(t, repo, "rev-parse", "refs/heads/athena/t-2"); got != childHead {
		t.Fatalf("athena/t-2 = %s, want %s", got, childHead)
	}
	if got := git(t, repo, "for-each-ref", "--format=%(refname)", "refs/athena/t-1/"); got != "refs/athena/t-1/import-t-2" {
		t.Fatalf("import refs = %q", got)
	}
	// The same commit again: nothing to fetch, nothing changes (the bundle is gone and not needed).
	if err := os.Remove(bundle); err != nil {
		t.Fatal(err)
	}
	if err := n.ImportBranch(bi); err != nil {
		t.Fatalf("a repeated import: %v", err)
	}

	// A newer head of the child's, while a worktree here has its branch checked out: refused, the branch stays.
	newer, _, bundle2 := leadCheckpoint(t, repo, "T-2", "newer child work")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", "-q", wt, "athena/t-2")
	err := n.ImportBranch(BranchImport{TicketID: "T-1", Child: "T-2", SHA: newer, Bundle: bundle2})
	if err == nil || !strings.Contains(err.Error(), "checked out") {
		t.Fatalf("an import over a checked-out branch: %v", err)
	}
	if got := git(t, repo, "rev-parse", "refs/heads/athena/t-2"); got != childHead {
		t.Fatalf("athena/t-2 moved to %s", got)
	}
	git(t, repo, "worktree", "remove", wt)

	// A commit the bundle does not hold is refused.
	if err := n.ImportBranch(BranchImport{TicketID: "T-1", Child: "T-2", SHA: strings.Repeat("f", 40), Bundle: bundle2}); err == nil {
		t.Fatal("an import of a commit the bundle lacks succeeded")
	}
	if err := n.ImportBranch(BranchImport{TicketID: "T-1", Child: "T-2", SHA: newer, Bundle: bundle2}); err != nil {
		t.Fatal(err)
	}
	if got := git(t, repo, "rev-parse", "refs/heads/athena/t-2"); got != newer {
		t.Fatalf("athena/t-2 = %s, want %s", got, newer)
	}
	for _, bad := range []BranchImport{
		{TicketID: "../x", Child: "T-2", SHA: newer, Bundle: bundle2},
		{TicketID: "T-1", Child: "-x", SHA: newer, Bundle: bundle2},
		{TicketID: "T-1", Child: "T-2", SHA: "HEAD", Bundle: bundle2},
		{TicketID: "T-9", Child: "T-2", SHA: newer, Bundle: bundle2},
	} {
		if err := n.ImportBranch(bad); err == nil {
			t.Errorf("ImportBranch(%+v) succeeded", bad)
		}
	}
}

// TestChildCleanupKeepsBranch: a child's clean worktree is removed although its commits are on no other branch. Its
// branch, which is what it delivers, is kept, and so is its checkpoint ref, which the server's mirror may not have
// taken yet: CheckpointBundle still sends the head the ready report named. Its seed refs go. The lead's cleanup on the
// same node then deletes the child's branch and checkpoint ref.
func TestChildCleanupKeepsBranch(t *testing.T) {
	repo := newRepo(t)
	work := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	n := New(Config{WorkRoot: work, StateDir: t.TempDir(), Logger: log}, nil)
	common := filepath.Join(repo, ".git")
	child := core.Ticket{ID: "T-2", Lead: "T-1", Execution: &core.Execution{Directory: repo}}
	p := placement{workDir: filepath.Join(work, "t-2"), repo: repo, common: common}
	if err := n.recordPlacement(child, p); err != nil {
		t.Fatal(err)
	}
	if err := repoWorktree(p, child); err != nil {
		t.Fatal(err)
	}
	git(t, p.workDir, "commit", "-q", "--allow-empty", "-m", "child work")
	head, err := n.childReadyHead(n.newSession(child, core.Assignment{ID: "a1"}, p))
	if err != nil || head != git(t, p.workDir, "rev-parse", "HEAD") {
		t.Fatalf("childReadyHead = %q, %v", head, err)
	}
	git(t, repo, "update-ref", ticketRefs(child.ID)+"seed", head)
	cleanupWorktree(p, child, work, log)
	if _, err := os.Stat(p.workDir); !os.IsNotExist(err) {
		t.Fatalf("the child's clean worktree was kept: %v", err)
	}
	if got := git(t, repo, "rev-parse", "refs/heads/athena/t-2"); got != head {
		t.Fatalf("athena/t-2 = %s, want %s", got, head)
	}
	if refs := git(t, repo, "for-each-ref", "--format=%(refname)", ticketRefs(child.ID)); refs != checkpointRef(child.ID) {
		t.Fatalf("the child's refs = %q, want its checkpoint only", refs)
	}
	var b bytes.Buffer
	sha, err := n.CheckpointBundle(context.Background(), child.ID, "", &b)
	if err != nil {
		t.Fatalf("CheckpointBundle after the child ended: %v", err)
	}
	bundle := filepath.Join(t.TempDir(), "child.bundle")
	if err := os.WriteFile(bundle, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := t.TempDir()
	git(t, fresh, "init", "-q")
	git(t, fresh, "fetch", "-q", bundle, checkpointRef(child.ID)+":refs/x")
	if got := git(t, fresh, "rev-parse", "refs/x"); got != sha {
		t.Fatalf("the bundle holds %s, want %s", got, sha)
	}
	git(t, fresh, "merge-base", "--is-ancestor", head, "refs/x") // the reported head is in it

	lead := core.Ticket{ID: "T-1", Children: []string{"T-2"}}
	lwt := filepath.Join(work, "t-1")
	if err := ensureWorktree(repo, lwt, branchName(lead), "HEAD", nil); err != nil {
		t.Fatal(err)
	}
	cleanupWorktree(placement{workDir: lwt, repo: repo, common: common}, lead, work, log)
	if refs := git(t, repo, "for-each-ref", "refs/athena/", "refs/heads/athena/t-2"); refs != "" {
		t.Fatalf("refs left after the lead's cleanup: %s", refs)
	}
	if _, err := n.CheckpointBundle(context.Background(), child.ID, "", &b); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("CheckpointBundle after the lead's cleanup = %v, want ErrNoCheckpoint", err)
	}
}

// TestLeadCleanupDeletesChildBranches: removing a lead's worktree deletes its children's branches and its imports in
// that repository; another Ticket's branch stays.
func TestLeadCleanupDeletesChildBranches(t *testing.T) {
	repo := newRepo(t)
	root := t.TempDir()
	lead := core.Ticket{ID: "T-1", Children: []string{"T-2", "T-3"}}
	wt := filepath.Join(root, "t-1")
	if err := ensureWorktree(repo, wt, branchName(lead), "HEAD", nil); err != nil {
		t.Fatal(err)
	}
	head := git(t, repo, "rev-parse", "HEAD")
	for _, b := range []string{"athena/t-2", "athena/t-3", "athena/t-4"} {
		git(t, repo, "branch", b, head)
	}
	git(t, repo, "update-ref", "refs/athena/t-1/import-t-2", head)
	git(t, repo, "update-ref", "refs/athena/t-1/import-t-3", head)
	git(t, repo, "update-ref", checkpointRef("T-2"), head)
	git(t, repo, "update-ref", checkpointRef("T-4"), head)
	cleanupWorktree(placement{workDir: wt, repo: repo, common: filepath.Join(repo, ".git")}, lead, root, slog.New(slog.DiscardHandler))
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("the lead's worktree was kept: %v", err)
	}
	if got := git(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads/athena/"); got != "refs/heads/athena/t-1\nrefs/heads/athena/t-4" {
		t.Fatalf("branches left = %q", got)
	}
	if refs := git(t, repo, "for-each-ref", "--format=%(refname)", "refs/athena/"); refs != checkpointRef("T-4") {
		t.Fatalf("refs left = %q, want another Ticket's checkpoint only", refs)
	}
}

// TestForkSeedLateLine: a child whose lead's commit did not reach the server in time is seeded with no bundle: its
// worktree starts from the default branch, and its first message says why, as a fork line.
func TestForkSeedLateLine(t *testing.T) {
	src := newRepo(t)
	clone := t.TempDir()
	git(t, clone, "clone", "-q", src, ".")
	def := git(t, clone, "rev-parse", "HEAD")
	n := New(Config{WorkRoot: t.TempDir(), StateDir: t.TempDir()}, nil)
	child := core.Ticket{ID: "T-2", Lead: "T-1", Repo: "o/r", Execution: &core.Execution{Base: strings.Repeat("e", 40)}}
	a := core.Assignment{ID: "a1"}
	if err := n.Seed(Seed{TicketID: child.ID, AssignmentID: a.ID, From: "mini", Unsent: SeedForkLate}); err != nil {
		t.Fatal(err)
	}
	rec, ok := n.seedFor(child, a.ID)
	if !ok || rec.Unsent != SeedForkLate {
		t.Fatalf("seed record = %+v, %v", rec, ok)
	}
	wt := filepath.Join(t.TempDir(), "t-2")
	s := n.newSession(child, a, placement{workDir: wt, repo: clone, common: filepath.Join(clone, ".git")})
	if err := n.seedWorktree(s, clone, "main", def, nil, rec); err != nil {
		t.Fatal(err)
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != def {
		t.Fatalf("child HEAD = %s, want the default branch's %s", got, def)
	}
	note, fork := n.takeMovedNote(s)
	if want := "T-1's commit did not reach the server in time; you start from the default branch."; note != want || !fork {
		t.Fatalf("note = %q (fork %v), want %q", note, fork, want)
	}
}

// TestImportBranchBeforeClone: a branch shipped to a node that has no clone of the repository yet is kept, and
// imported at the clone step of the Ticket's launch.
func TestImportBranchBeforeClone(t *testing.T) {
	src := newRepo(t)
	state := t.TempDir()
	lead := core.Ticket{ID: "T-1", Repo: "o/r"}
	n := New(Config{WorkRoot: t.TempDir(), StateDir: state}, ticketEngine{tickets: map[string]core.Ticket{"T-1": lead}})
	childHead, _, bundle := leadCheckpoint(t, src, "T-2", "child work")
	if err := n.ImportBranch(BranchImport{TicketID: "T-1", Child: "T-2", SHA: childHead, Bundle: bundle}); err != nil {
		t.Fatalf("an import before the clone: %v", err)
	}
	if _, err := os.Stat(bundle); !os.IsNotExist(err) {
		t.Fatalf("the bundle was not taken: %v", err)
	}

	clone := CloneDir(state, "o/r")
	if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(clone), "clone", "-q", src, clone)
	n.applyImports("T-1", seedGit(context.Background(), clone, cloneGitEnv(clone, "o/r")))
	if got := git(t, clone, "rev-parse", "refs/heads/athena/t-2"); got != childHead {
		t.Fatalf("athena/t-2 = %s, want %s", got, childHead)
	}
	if entries, _ := os.ReadDir(filepath.Join(state, "imports")); len(entries) != 0 {
		t.Fatalf("kept imports left: %v", entries)
	}
	// Taken once: nothing left to apply.
	n.applyImports("T-1", seedGit(context.Background(), clone, cloneGitEnv(clone, "o/r")))
}
