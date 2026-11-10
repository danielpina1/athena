package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
//
// A Ticket with a Repo is checkpointed the same way at the end of every turn that ended well, and Config.OnCheckpoint
// hears of each checkpoint: the server keeps the latest in its mirror (internal/fleet), so the Ticket can be retried
// on another node from it. CheckpointBundle is what travels: a git bundle of the ref, after the last one sent.

const (
	// checkpointTimeout bounds the git calls of one checkpoint.
	checkpointTimeout = 30 * time.Second
	// checkpointName and checkpointEmail are the author and committer of a checkpoint commit.
	checkpointName  = "Athena"
	checkpointEmail = "athena@localhost"
	// bundleTimeout bounds the git calls of one CheckpointBundle: the first bundle of a Ticket holds its whole history.
	bundleTimeout = 5 * time.Minute
	// MaxCheckpointBundle is the largest checkpoint bundle a node sends and the server takes.
	MaxCheckpointBundle = 512 << 20
)

var (
	// ErrNoCheckpoint is a Ticket the node keeps no checkpoint of.
	ErrNoCheckpoint = errors.New("no checkpoint of this ticket")
	// ErrCheckpointUnchanged is a CheckpointBundle asked for what its base already holds: there is nothing to send.
	ErrCheckpointUnchanged = errors.New("the checkpoint is unchanged since its base")
	// ErrCheckpointTooBig is a bundle over MaxCheckpointBundle: it is never sent.
	ErrCheckpointTooBig = fmt.Errorf("the checkpoint bundle is over %d MiB", MaxCheckpointBundle>>20)
)

// bundleCap is the bound CheckpointBundle stops at: MaxCheckpointBundle; a variable for tests.
var bundleCap int64 = MaxCheckpointBundle

// CheckpointRef is the ref a Ticket's checkpoint is kept at, on a node and in the server's mirror.
func CheckpointRef(ticketID string) string { return checkpointRef(ticketID) }

// checkpointRef is the ref a Ticket's checkpoint is kept at.
func checkpointRef(ticketID string) string {
	return "refs/athena/" + strings.ToLower(ticketID) + "/checkpoint"
}

// checkpoint commits the work in progress of the git worktree at workDir, on top of its HEAD, to checkpointRef: every
// change and untracked file but the ignored ones (git add -A into an index of its own, GIT_INDEX_FILE, starting from
// HEAD's tree), never moving the branch or touching the worktree's index or files. env is the git environment (nil:
// the node's), the git directories pinned for a node worktree (worktreeGitEnv). It runs git with no shell, the pins of
// gitIn and checkpointTimeout, and returns the commit. When the ref already holds the same work on the same HEAD, that
// commit is the checkpoint and the ref stays. why goes into the ref's log.
func checkpoint(workDir, ticketID string, env []string, why string) (sha string, err error) {
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
	// The index of its own starts as a copy of the worktree's, so git's stat cache skips the files that did not change
	// (a turn end is no full re-hash); the worktree's own index is only read. When there is none, or the copy does not
	// take, it starts from HEAD's tree.
	index := filepath.Join(tmp, "index")
	tree := ""
	if seedIndex(ctx, workDir, gitBase(env), index) {
		if _, err = git("add", "-A"); err == nil {
			tree, err = git("write-tree")
		}
		if err != nil {
			tree = ""
			_ = os.Remove(index)
		}
	}
	if tree == "" {
		if _, err := git("read-tree", head); err != nil {
			return "", err
		}
		if _, err := git("add", "-A"); err != nil {
			return "", err
		}
		if tree, err = git("write-tree"); err != nil {
			return "", err
		}
	}
	ref := checkpointRef(ticketID)
	if prev, err := git("rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil && prev != "" {
		if out, err := git("rev-parse", prev+"^{tree}", prev+"^@"); err == nil && out == tree+"\n"+head {
			return prev, nil // nothing changed since the last checkpoint
		}
	}
	if sha, err = git("commit-tree", "--no-gpg-sign", "-p", head, "-m", "checkpoint", tree); err != nil {
		return "", err
	}
	if _, err := git("update-ref", "-m", "athena: checkpoint ("+why+")", ref, sha); err != nil {
		return "", err
	}
	return sha, nil
}

// seedIndex copies the worktree's own index (git's --git-path index, under env) to index; false when there is none
// or it cannot be copied.
func seedIndex(ctx context.Context, workDir string, env []string, index string) bool {
	var clean []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
			clean = append(clean, kv)
		}
	}
	src, err := gitInCtx(ctx, workDir, clean, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil || src == "" {
		return false
	}
	in, err := os.Open(src)
	if err != nil {
		return false
	}
	defer in.Close()
	out, err := os.OpenFile(index, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(index)
		return false
	}
	return true
}

// worktreeEnv is the git environment of the node's git in the Ticket's worktree (placement p, which has a repo): the
// node's minimal environment (a GitHub clone's for a GitHub Ticket) with the git directories Athena made for it pinned
// (worktreeGitEnv).
func (n *Node) worktreeEnv(p placement, t core.Ticket) ([]string, error) {
	admin, common, err := n.worktreeGitDirs(p, t)
	if err != nil {
		return nil, err
	}
	base := nodeGitBaseEnv()
	if p.github != "" {
		base = cloneGitEnv(p.repo, p.github)
	}
	return worktreeGitEnv(base, admin, common, p.workDir), nil
}

// worktreeGitDirs are the git directory Athena made for the Ticket's worktree (findWorktreeAdmin) and its repository's
// common directory, for worktreeGitEnv.
func (n *Node) worktreeGitDirs(p placement, t core.Ticket) (admin, common string, err error) {
	common = p.common
	if common == "" {
		if common, err = repoGitDir(p.repo); err != nil {
			return "", "", err
		}
	}
	admin, err = findWorktreeAdmin(common, p.workDir, n.cfg.WorkRoot, n.log.With("ticket", t.ID))
	return admin, common, err
}

// checkpointSession checkpoints the work of a worker session that stopped at its usage limit, when it runs in a git
// worktree; a review writes nothing, so its session is not checkpointed. A failure is logged: it never holds up the
// failover, since the next session works in the same worktree anyway.
func (n *Node) checkpointSession(s *session) {
	n.checkpointWork(s, "at a usage limit")
}

// turnCheckpoint checkpoints the work of a Ticket with a Repo at the end of a turn that ended well. A failure is
// logged: the session goes on.
func (n *Node) turnCheckpoint(s *session) {
	if s.t.Repo == "" {
		return
	}
	n.checkpointWork(s, "at a turn end")
}

// checkpointWork checkpoints the session's worktree (when it runs in one and is not a review) and tells
// Config.OnCheckpoint of a Ticket with a Repo.
func (n *Node) checkpointWork(s *session, why string) {
	p := s.place
	if p.repo == "" || s.reviewing() {
		return // not a git worktree, or nothing of the session's own to keep
	}
	if _, err := os.Stat(p.workDir); err != nil {
		return
	}
	env, err := n.worktreeEnv(p, s.t)
	if err == nil {
		var sha string
		sha, err = func() (string, error) {
			if p.github != "" {
				// The clone is shared: no fetch or worktree add of another Ticket moves its refs meanwhile.
				defer lockRepo(p.repo)()
			}
			return checkpoint(p.workDir, s.t.ID, env, why)
		}()
		if err == nil {
			s.log.Info("checkpointed the work in progress", "ref", checkpointRef(s.t.ID), "commit", sha, "when", why)
			if s.t.Repo != "" && n.cfg.OnCheckpoint != nil {
				n.cfg.OnCheckpoint(s.t.ID, s.t.Repo, checkpointRef(s.t.ID), sha)
			}
			return
		}
	}
	s.log.Warn("checkpoint failed; the session goes on", "dir", p.workDir, "when", why, "error", fmt.Sprint(err))
}

// CheckpointBundle writes to w a git bundle of the Ticket's checkpoint ref after since (a checkpoint sent before; ""
// for the whole ref), and returns the commit the bundle carries. ErrNoCheckpoint: the node keeps none for the Ticket;
// ErrCheckpointUnchanged: the ref is at since, and nothing is written. A since this repository does not know is an
// error: send the whole ref instead. git runs with no shell, the pins of gitIn and the Ticket's git directories pinned,
// bounded by ctx and bundleTimeout; a bundle over MaxCheckpointBundle is stopped as it grows: ErrCheckpointTooBig. The
// bundle streams straight to w (no copy of it is kept); on an error w may hold part of one, which the caller drops.
func (n *Node) CheckpointBundle(ctx context.Context, ticketID, since string, w io.Writer) (sha string, err error) {
	if since != "" && !shaRe.MatchString(since) {
		return "", fmt.Errorf("checkpoint base %q is not a commit name", since)
	}
	p, ok, err := n.recordedPlacement(core.Ticket{ID: ticketID})
	if err != nil {
		return "", err
	}
	if !ok || p.repo == "" {
		return "", ErrNoCheckpoint
	}
	common := p.common
	if common == "" {
		if common, err = repoGitDir(p.repo); err != nil {
			return "", err
		}
	}
	base := nodeGitBaseEnv()
	if p.github != "" {
		base = cloneGitEnv(p.repo, p.github)
	}
	env := repoGitEnv(base, common, p.repo)
	ctx, cancel := context.WithTimeout(ctx, bundleTimeout)
	defer cancel()
	git := func(args ...string) (string, error) { return gitInCtx(ctx, p.repo, env, args...) }
	ref := checkpointRef(ticketID)
	head, err := git("rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil || head == "" {
		return "", ErrNoCheckpoint
	}
	if head == since {
		return head, ErrCheckpointUnchanged
	}
	args := []string{"bundle", "create", "-", ref}
	if since != "" {
		args = append(args, "^"+since)
	}
	return bundleTo(ctx, p.repo, env, ref, w, args)
}

// bundleTo runs `git <args>` (a bundle create to stdout) in dir with env and the pins of gitIn, streaming its output
// to w, and returns the commit the bundle's header names for ref (the ref may have moved since it was read). Past
// bundleCap it kills git and returns ErrCheckpointTooBig, so an oversized bundle is never built whole. On an error w
// may hold part of a bundle: the caller drops it.
func bundleTo(ctx context.Context, dir string, env []string, ref string, w io.Writer, args []string) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append(append(slices.Clone(gitPins), "-C", dir), args...)...)
	cmd.WaitDelay = time.Second
	cmd.Env = append(slices.Clone(env), "GIT_TERMINAL_PROMPT=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	hdr := &headerTap{w: w}
	n, cerr := io.Copy(hdr, io.LimitReader(stdout, bundleCap+1))
	tooBig := n > bundleCap
	if tooBig || cerr != nil {
		cancel()
		_, _ = io.Copy(io.Discard, stdout)
	}
	werr := cmd.Wait()
	switch {
	case tooBig:
		return "", ErrCheckpointTooBig
	case cerr != nil:
		return "", cerr
	case werr != nil:
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), werr, strings.TrimSpace(stderr.String()))
	}
	sha := hdr.head(ref)
	if !shaRe.MatchString(sha) {
		return "", fmt.Errorf("the bundle names no commit for %s", ref)
	}
	return sha, nil
}

// headerTap passes a bundle through to w and keeps its header: the lines before the first empty one.
type headerTap struct {
	w    io.Writer
	buf  []byte
	done bool
}

func (h *headerTap) Write(p []byte) (int, error) {
	if !h.done {
		h.buf = append(h.buf, p...)
		if i := strings.Index(string(h.buf), "\n\n"); i >= 0 {
			h.buf, h.done = h.buf[:i], true
		} else if len(h.buf) > 1<<20 {
			h.done = true // no header ends this early: head finds nothing
		}
	}
	return h.w.Write(p)
}

// head is the commit the header names for ref: a "<sha> <ref>" line (a prerequisite starts with "-").
func (h *headerTap) head(ref string) string {
	for _, line := range strings.Split(string(h.buf), "\n") {
		if sha, name, ok := strings.Cut(line, " "); ok && name == ref && !strings.HasPrefix(sha, "-") {
			return sha
		}
	}
	return ""
}

// StreamBundle runs `git <args>` (a bundle create to stdout) in dir with env and the node's pins, streaming the bundle
// to w, and returns the commit its header names for ref; past MaxCheckpointBundle it stops git and returns
// ErrCheckpointTooBig. On an error w may hold part of a bundle. The server's mirror makes its seeds with it.
func StreamBundle(ctx context.Context, dir string, env []string, ref string, w io.Writer, args []string) (string, error) {
	return bundleTo(ctx, dir, env, ref, w, args)
}

// RunGit runs git in dir with env (the process's, if nil), with no shell and the node's pins (no hook, no fsmonitor),
// killed once ctx ends; it returns git's trimmed stdout. The server's checkpoint mirror (internal/fleet) runs its git
// through it.
func RunGit(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	return gitInCtx(ctx, dir, env, args...)
}

// CheckRepo refuses a repository name that is not owner/name, each a plain name.
func CheckRepo(repo string) error {
	_, _, err := splitRepo(repo)
	return err
}
