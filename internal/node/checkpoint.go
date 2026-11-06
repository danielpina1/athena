package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

// A session that stops at its usage limit leaves its work in progress in the Ticket's worktree, which the session that
// takes over (on the other runtime, or the same one after its reset) continues in. Before the failover, the node
// records that work in a commit no branch points at, refs/athena/<lowercased ticket id>/checkpoint: the branch, the
// index and the files stay as they are, so no checkpoint commit lands on the pull request, and nothing is lost if a
// session damages the worktree later. (§6.1a says the checkpoint is committed to the Ticket branch; Athena keeps it
// on this side ref instead, docs/dev/codex.md §7.)

const (
	// checkpointTimeout bounds the git calls of one checkpoint.
	checkpointTimeout = 30 * time.Second
	// checkpointName and checkpointEmail are the author and committer of a checkpoint commit.
	checkpointName  = "Athena"
	checkpointEmail = "athena@localhost"
)

// checkpointRef is the ref a Ticket's checkpoint is kept at.
func checkpointRef(ticketID string) string {
	return "refs/athena/" + strings.ToLower(ticketID) + "/checkpoint"
}

// checkpoint commits the work in progress of the git worktree at workDir, on top of its HEAD, to checkpointRef: every
// change and untracked file but the ignored ones (git add -A into an index of its own, GIT_INDEX_FILE, starting from
// HEAD's tree), never moving the branch or touching the worktree's index or files. env is the git environment (nil:
// the node's), the git directories pinned for a node worktree (worktreeGitEnv). It runs git with no shell, the pins of
// gitIn and checkpointTimeout, and returns the commit.
func checkpoint(workDir, ticketID string, env []string) (sha string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), checkpointTimeout)
	defer cancel()
	tmp, err := os.MkdirTemp("", "athena-checkpoint-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	genv := append(gitBase(env), "GIT_INDEX_FILE="+filepath.Join(tmp, "index"),
		"GIT_AUTHOR_NAME="+checkpointName, "GIT_AUTHOR_EMAIL="+checkpointEmail,
		"GIT_COMMITTER_NAME="+checkpointName, "GIT_COMMITTER_EMAIL="+checkpointEmail)
	git := func(args ...string) (string, error) { return gitInCtx(ctx, workDir, genv, args...) }
	head, err := git("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	if _, err := git("read-tree", head); err != nil {
		return "", err
	}
	if _, err := git("add", "-A"); err != nil {
		return "", err
	}
	tree, err := git("write-tree")
	if err != nil {
		return "", err
	}
	if sha, err = git("commit-tree", "--no-gpg-sign", "-p", head, "-m", "checkpoint", tree); err != nil {
		return "", err
	}
	if _, err := git("update-ref", "-m", "athena: checkpoint at a usage limit", checkpointRef(ticketID), sha); err != nil {
		return "", err
	}
	return sha, nil
}

// worktreeEnv is the git environment of the node's git in the Ticket's worktree (placement p, which has a repo): the
// node's minimal environment (a GitHub clone's for a GitHub Ticket) with the git directories Athena made for it pinned
// (worktreeGitEnv).
func (n *Node) worktreeEnv(p placement, t core.Ticket) ([]string, error) {
	common := p.common
	if common == "" {
		var err error
		if common, err = repoGitDir(p.repo); err != nil {
			return nil, err
		}
	}
	admin, err := findWorktreeAdmin(common, p.workDir, n.cfg.WorkRoot, n.log.With("ticket", t.ID))
	if err != nil {
		return nil, err
	}
	base := nodeGitBaseEnv()
	if p.github != "" {
		base = cloneGitEnv(p.repo, p.github)
	}
	return worktreeGitEnv(base, admin, common, p.workDir), nil
}

// checkpointSession checkpoints the work of a worker session that stopped at its usage limit, when it runs in a git
// worktree; a review writes nothing, so its session is not checkpointed. A failure is logged: it never holds up the
// failover, since the next session works in the same worktree anyway.
func (n *Node) checkpointSession(s *session) {
	p := s.place
	if p.repo == "" || s.reviewing() {
		return // not a git worktree, or nothing of the session's own to keep
	}
	if _, err := os.Stat(p.workDir); err != nil {
		return
	}
	env, err := n.worktreeEnv(p, s.t)
	if err == nil {
		if p.github != "" {
			// The clone is shared: no fetch or worktree add of another Ticket moves its refs meanwhile.
			defer lockRepo(p.repo)()
		}
		var sha string
		if sha, err = checkpoint(p.workDir, s.t.ID, env); err == nil {
			s.log.Info("checkpointed the work in progress", "ref", checkpointRef(s.t.ID), "commit", sha)
			return
		}
	}
	s.log.Warn("checkpoint failed; the failover goes on", "dir", p.workDir, "error", fmt.Sprint(err))
}
