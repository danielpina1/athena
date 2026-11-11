package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

// A child Ticket (plan 7) works on its own branch, athena/<child>, and the lead merges the children's branches. A
// hand_off child's branch starts at the lead's last commit (Execution.Base); an up-front child's, which has no Base,
// at the default branch. On the lead's node the Base is in the clone already. A child placed on another node gets it
// from a fork seed: the lead's checkpoint bundle, fetched into the child's seed ref, the branch made at the Base and
// nothing restored. The branches of children (and deps) that ran on another node come back as imports: a bundle of
// the child's checkpoint fetched into refs/athena/<ticket>/import-<child>, then athena/<child> set to its head.
//
// A child's branch is what it delivers, so its clean worktree is removed even with commits no other branch has, and
// its branch and checkpoint ref kept (the server's mirror takes the checkpoint later, for R7); the lead's cleanup
// deletes its children's branches and checkpoint refs in that repository.

// baseMissing is the launch's error for a child whose Base this node does not have.
type baseMissing struct{ base string }

func (e *baseMissing) Error() string {
	return "the lead's commit " + short7(e.base) + " is not on this node"
}

// errBaseMissing is the launch's error for a child whose Base this node does not have.
func errBaseMissing(base string) error { return &baseMissing{base: base} }

// startLate makes a child's new worktree at the default branch def when its Base is not in this clone and no seed
// brings it (its fork seed was lost, or a later Assignment of it moved here): the first message says why, as a fork
// line (SeedForkLate). Under the clone's lock.
func (n *Node) startLate(s *session, clone, def string, env []string) error {
	if err := ensureWorktree(clone, s.place.workDir, branchName(s.t), def, env); err != nil {
		return err
	}
	rec := seedRecord{TicketID: s.t.ID, AssignmentID: s.a.ID, From: "another node", Unsent: SeedForkLate,
		Created: time.Now(), Note: forkLateLine(s.t.Lead)}
	err := os.MkdirAll(filepath.Join(n.cfg.StateDir, "seeds"), 0o700)
	if err == nil {
		err = n.writeSeed(s.t.ID, rec)
	}
	if err != nil {
		s.log.Warn("keep the fork line", "error", err)
	}
	s.log.Warn("the lead's commit is not on this node; the child starts from the default branch", "lead", s.t.Lead,
		"commit", s.t.Execution.Base)
	return nil
}

func short7(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// worktreeBase is the commit a new worktree of t starts at in the repository whose git runs through git: its
// Execution.Base when it has one, which must be a commit there, else def.
func worktreeBase(git func(args ...string) (string, error), t core.Ticket, def string) (string, error) {
	if t.Execution == nil || t.Execution.Base == "" {
		return def, nil
	}
	base := t.Execution.Base
	if !shaRe.MatchString(base) {
		return "", fmt.Errorf("the lead's commit %q is not a commit name", base)
	}
	if _, err := git("cat-file", "-e", base+"^{commit}"); err != nil {
		return "", errBaseMissing(base)
	}
	return base, nil
}

// repoWorktree makes a folder Ticket's worktree of its repository: from its Base (a child), else from HEAD. An
// existing worktree is used as it is.
func repoWorktree(p placement, t core.Ticket) error {
	if _, err := os.Stat(p.workDir); err == nil {
		return ensureWorktreeIn(p.repo, p.common, p.workDir, branchName(t), "HEAD", nil)
	}
	renv := repoGitEnv(nil, p.common, p.repo)
	base, err := worktreeBase(func(args ...string) (string, error) { return gitIn(p.repo, renv, args...) }, t, "HEAD")
	if err != nil {
		return err
	}
	return ensureWorktreeIn(p.repo, p.common, p.workDir, branchName(t), base, nil)
}

// forkLine is the first message's line of a child whose branch came from a fork seed.
func forkLine(lead, base string) string {
	return "Your branch starts from " + lead + "'s branch at " + short7(base) + ", fetched from the server."
}

// forkWorktree builds a child's new worktree from a fork seed (under the clone's lock): the lead's checkpoint bundle
// is fetched into the child's seed ref, and its branch made at the seed's Base, which the bundle must hold; nothing is
// restored. A bundle git refuses is logged, and the launch then needs the Base in the clone already.
func (n *Node) forkWorktree(ctx context.Context, s *session, git func(args ...string) (string, error), clone string,
	env []string, rec seedRecord) error {
	t, p := s.t, s.place
	if t.Lead == "" {
		return errors.New("a fork seed for a Ticket that has no lead")
	}
	if _, err := n.fetchSeed(t, git, rec); err != nil {
		s.log.Warn("the fork seed cannot be fetched", "error", err)
	} else if _, err := git("merge-base", "--is-ancestor", rec.Base, seedRefOf(t)); err != nil {
		s.log.Warn("the fork seed does not hold the lead's commit", "base", rec.Base, "error", err)
	}
	if _, err := git("cat-file", "-e", rec.Base+"^{commit}"); err != nil {
		return errBaseMissing(rec.Base)
	}
	branch := branchName(t)
	_, _ = git("worktree", "prune")
	if _, err := git("rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		if _, err := git("update-ref", "refs/heads/"+branch, rec.Base); err != nil {
			return err
		}
	}
	if err := ensureWorktree(clone, p.workDir, branch, rec.Base, env); err != nil {
		return err
	}
	n.finishSeed(s, git, rec, forkLine(t.Lead, rec.Base))
	s.log.Info("worktree forked from the lead", "lead", t.Lead, "commit", rec.Base)
	return nil
}

// forkLateLine is the first message's line of a child placed away from its lead whose lead's commit did not reach
// the server in time.
func forkLateLine(lead string) string {
	return lead + "'s commit did not reach the server in time; you start from the default branch."
}

// forkLateWorktree builds a new worktree for a child whose fork seed came without the lead's checkpoint (SeedForkLate),
// under the clone's lock: its branch starts at base, the default branch unless this clone has the lead's commit.
func (n *Node) forkLateWorktree(s *session, git func(args ...string) (string, error), clone, base string, env []string,
	rec seedRecord) error {
	t, p := s.t, s.place
	note := forkLateLine(t.Lead)
	if t.Execution != nil && t.Execution.Base != "" && base == t.Execution.Base {
		note = forkLine(t.Lead, base) // the lead's commit was here already
	}
	branch := branchName(t)
	_, _ = git("worktree", "prune")
	if _, err := git("rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		if _, err := git("update-ref", "refs/heads/"+branch, base); err != nil {
			return err
		}
	}
	if err := ensureWorktree(clone, p.workDir, branch, base, env); err != nil {
		return err
	}
	n.finishSeed(s, git, rec, note)
	s.log.Info("worktree started without the lead's commit", "lead", t.Lead, "commit", base)
	return nil
}

// BranchImport is the server's word that the branch of Child, a Ticket that ran on another node, is SHA: Bundle is a
// git bundle of Child's checkpoint ref from the server's mirror, which holds SHA. TicketID is the Ticket that needs it
// (the lead, or a dependent), whose repository takes the branch.
type BranchImport struct{ TicketID, Child, SHA, Bundle string }

// ImportBranch sets athena/<child> in the repository of bi.TicketID on this node to bi.SHA, from bi's bundle: the
// bundle is fetched into refs/athena/<ticket>/import-<child>, then the branch is forced to SHA, which the fetched ref
// must hold. A branch a worktree on this node has checked out is never moved. A branch already at SHA is left as it
// is, with nothing fetched. The bundle file stays the caller's, except on a node with no clone of a GitHub Ticket's
// repository yet: there the bundle is taken and kept, and the import is done at the clone step of the Ticket's launch.
func (n *Node) ImportBranch(bi BranchImport) error {
	if !seedTicketRe.MatchString(bi.TicketID) || !seedTicketRe.MatchString(bi.Child) || strings.HasPrefix(bi.Child, "-") {
		return &core.UserError{Msg: "bad branch import: ticket or child id"}
	}
	if !shaRe.MatchString(bi.SHA) {
		return &core.UserError{Msg: "bad branch import: the head is not a commit name"}
	}
	eng := n.engine()
	if eng == nil {
		return errors.New("no engine to read the ticket from")
	}
	t, _, err := eng.Ticket(bi.TicketID)
	if err != nil {
		return err
	}
	p, err := n.place(t)
	if err != nil {
		return err
	}
	if p.repo == "" {
		return fmt.Errorf("ticket %s has no git repository on this node", t.ID)
	}
	var env []string
	if p.github != "" {
		defer lockRepo(p.repo)()
		if _, err := os.Stat(filepath.Join(p.repo, ".git")); err != nil {
			// The node clones with an Assignment's token only: the import waits for the clone step of the Ticket's
			// launch (applyImports), under the same lock.
			return n.keepImport(bi)
		}
		env = cloneGitEnv(p.repo, p.github)
	}
	common := p.common
	if common == "" {
		if common, err = repoGitDir(p.repo); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(n.ctx, seedTimeout)
	defer cancel()
	renv := repoGitEnv(env, common, p.repo)
	git := func(args ...string) (string, error) { return gitInCtx(ctx, p.repo, renv, args...) }
	return importBranch(git, bi, n.log)
}

// importBranch is ImportBranch in the repository git runs in.
func importBranch(git func(args ...string) (string, error), bi BranchImport, log *slog.Logger) error {
	branch := "refs/heads/" + branchName(core.Ticket{ID: bi.Child})
	if at, err := git("rev-parse", "--verify", "--quiet", branch); err == nil && at == bi.SHA {
		return nil // imported already
	}
	list, err := git("worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(list, "\n") {
		if line == "branch "+branch {
			return fmt.Errorf("%s is checked out in a worktree on this node: it is not moved", strings.TrimPrefix(branch, "refs/heads/"))
		}
	}
	if !filepath.IsAbs(bi.Bundle) {
		return fmt.Errorf("the bundle of %s's branch is not a file path", bi.Child)
	}
	ref := ticketRefs(bi.TicketID) + "import-" + strings.ToLower(bi.Child)
	if err := fetchBundle(git, bi.Bundle, checkpointRef(bi.Child), ref); err != nil {
		return fmt.Errorf("fetch the branch of %s: %w", bi.Child, err)
	}
	if _, err := git("merge-base", "--is-ancestor", bi.SHA, ref); err != nil {
		return fmt.Errorf("the branch of %s shipped here does not hold %s", bi.Child, short7(bi.SHA))
	}
	if _, err := git("branch", "-f", strings.TrimPrefix(branch, "refs/heads/"), bi.SHA); err != nil {
		return err
	}
	log.Info("branch imported", "ticket", bi.TicketID, "child", bi.Child, "commit", bi.SHA)
	return nil
}

// fetchBundle fetches the ref src of the bundle file into dst, with every object checked and no protocol but the
// bundle file.
func fetchBundle(git func(args ...string) (string, error), bundle, src, dst string) error {
	_, err := git("-c", "protocol.allow=never", "-c", "protocol.file.allow=always", "-c", "transfer.fsckObjects=true",
		"-c", "fetch.fsckObjects=true", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "fetch", "--quiet", "--no-tags",
		"--no-write-fetch-head", "--no-auto-gc", "--no-recurse-submodules", bundle, "+"+src+":"+dst)
	return err
}

// deleteChildBranches deletes the branches and checkpoint refs of the lead t's children in the repository git runs
// in, once the lead's worktree is gone. A branch a worktree still has checked out stays (git refuses it), and so does
// that child's checkpoint ref.
func deleteChildBranches(git func(args ...string) (string, error), t core.Ticket, log *slog.Logger) {
	for _, c := range t.Children {
		branch := branchName(core.Ticket{ID: c})
		if _, err := git("rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
			if _, err := git("branch", "-D", branch); err != nil {
				log.Warn("remove the child's branch", "ticket", t.ID, "branch", branch, "error", err)
				continue
			}
		}
		ref := checkpointRef(c)
		if _, err := git("rev-parse", "--verify", "--quiet", ref); err != nil {
			continue
		}
		if _, err := git("update-ref", "-d", ref); err != nil {
			log.Warn("remove the child's checkpoint ref", "ticket", t.ID, "ref", ref, "error", err)
		}
	}
}

// keepImport keeps an import for a Ticket whose clone this node has not made yet: the bundle is taken into
// <StateDir>/imports, and applyImports does the import once the clone is there. A later import of the same child
// replaces it.
func (n *Node) keepImport(bi BranchImport) error {
	if n.cfg.StateDir == "" {
		return errors.New("no state directory for imports")
	}
	dir := filepath.Join(n.cfg.StateDir, "imports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file := filepath.Join(dir, strings.ToLower(bi.TicketID)+"-"+strings.ToLower(bi.Child)+".bundle")
	if err := takeFile(bi.Bundle, file, MaxCheckpointBundle); err != nil {
		return fmt.Errorf("keep the branch of %s: %w", bi.Child, err)
	}
	bi.Bundle = file
	key := strings.ToLower(bi.TicketID)
	n.importMu.Lock()
	defer n.importMu.Unlock()
	if n.imports == nil {
		n.imports = map[string][]BranchImport{}
	}
	n.imports[key] = append(slices.DeleteFunc(n.imports[key], func(x BranchImport) bool {
		return strings.EqualFold(x.Child, bi.Child)
	}), bi)
	n.log.Info("branch kept for the clone step", "ticket", bi.TicketID, "child", bi.Child, "commit", bi.SHA)
	return nil
}

// applyImports does the imports kept for the Ticket (keepImport) in its clone, whose git runs through git, under the
// clone's lock. One that fails is logged: the lead's Input still names the branch.
func (n *Node) applyImports(ticketID string, git func(args ...string) (string, error)) {
	key := strings.ToLower(ticketID)
	n.importMu.Lock()
	kept := n.imports[key]
	delete(n.imports, key)
	n.importMu.Unlock()
	for _, bi := range kept {
		if err := importBranch(git, bi, n.log); err != nil {
			n.log.Warn("import a kept branch", "ticket", bi.TicketID, "child", bi.Child, "error", err)
		}
		if err := os.Remove(bi.Bundle); err != nil && !errors.Is(err, os.ErrNotExist) {
			n.log.Warn("remove a kept branch's bundle", "file", bi.Bundle, "error", err)
		}
	}
}
