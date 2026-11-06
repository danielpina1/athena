package node

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/danielpina1/athena/internal/core"
)

// A worktree's git links are the files that say where its git directory is: the working directory's .git file and
// the git directory's gitdir and commondir, plus the git directory's config.worktree, which git reads as config. A
// Codex session can write all four: its working directory is writable, and so is the worktree's git directory (one of
// its writable roots, sandbox.go), and Codex's sandbox has no deny list, so only the hook rules guard them, by text,
// which an indirect write (a script, a test, a Makefile target) gets past. The node's own git never reads them
// (worktreeGitEnv), but the owner's git in a kept worktree, or an IDE or shell prompt that runs it, follows a
// repointed .git or commondir to a config of the worker's (core.fsmonitor, core.hooksPath, a filter): code that runs
// outside any sandbox. So after each Codex session process, and before a worktree is kept or removed, the node puts
// them back from the Ticket's placement record (restoreGitLinks), never from what the worktree's files say now.

// gitLink is one git link file and what the placement record says it holds: the path it must name (want), relative
// paths resolved against base, after the prefix; body is what the node writes back.
type gitLink struct {
	path, base, prefix, want, body string
}

// restoreGitLinks checks the git links of t's worktree against its placement record and restores each one that
// differs, logging a warning: the .git file names the git directory Athena made (worktreeAdmin, or the one git named
// a worktree made before Athena checked the name), whose gitdir names the .git file and whose commondir names the
// repository's git directory; config.worktree is what git made at the worktree's creation (placement.worktreeConfig),
// left alone for a Ticket placed before the node recorded it. A path that is a symlink or a directory is replaced. A
// Ticket with no worktree, or no record, has nothing to check.
func (n *Node) restoreGitLinks(t core.Ticket, log *slog.Logger) {
	p, ok, err := n.recordedPlacement(t)
	if err != nil {
		log.Warn("git links not checked: the placement record cannot be read", "error", err)
		return
	}
	if !ok || p.repo == "" || p.common == "" {
		return
	}
	if _, err := os.Lstat(p.workDir); err != nil {
		return
	}
	admin, err := n.recordedAdmin(p, log)
	if err != nil {
		log.Warn("git links not checked: the worktree's git directory is not where Athena made it; remove the worktree by hand",
			"dir", p.workDir, "error", err)
		return
	}
	dotGit := filepath.Join(p.workDir, ".git")
	for _, l := range []gitLink{
		{path: dotGit, base: p.workDir, prefix: "gitdir:", want: admin, body: "gitdir: " + admin + "\n"},
		{path: filepath.Join(admin, "gitdir"), base: admin, want: dotGit, body: dotGit + "\n"},
		{path: filepath.Join(admin, "commondir"), base: admin, want: p.common, body: p.common + "\n"},
	} {
		if l.names() {
			continue
		}
		if err := replaceFile(l.path, l.body); err != nil {
			log.Error("restore a git link a session changed", "path", l.path, "error", err)
			continue
		}
		log.Warn("restored a git link a session changed", "path", l.path)
	}
	if p.worktreeConfig == nil {
		return
	}
	path, want := filepath.Join(admin, "config.worktree"), *p.worktreeConfig
	if regularHolds(path, want) {
		return
	}
	if want == "" {
		err = os.RemoveAll(path)
	} else {
		err = replaceFile(path, want)
	}
	if err != nil {
		log.Error("restore the worktree's config a session changed", "path", path, "error", err)
		return
	}
	log.Warn("restored the worktree's config a session changed", "path", path)
}

// recordedAdmin is the git directory of p's worktree: worktreeAdmin when it is a directory, else (a worktree made before
// ensureWorktree checked the name) the one findWorktreeAdmin finds.
func (n *Node) recordedAdmin(p placement, log *slog.Logger) (string, error) {
	admin := worktreeAdmin(p.common, p.workDir)
	if st, err := os.Lstat(admin); err == nil && st.IsDir() {
		return admin, nil
	}
	return findWorktreeAdmin(p.common, p.workDir, n.cfg.WorkRoot, log)
}

// names reports whether the link is a regular file that names what it must.
func (l gitLink) names() bool {
	st, err := os.Lstat(l.path)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	b, err := readSmall(l.path)
	if err != nil {
		return false
	}
	at, ok := strings.CutPrefix(strings.TrimSpace(string(b)), l.prefix)
	if !ok {
		return false
	}
	if at = strings.TrimSpace(at); at == "" {
		return false
	}
	if !filepath.IsAbs(at) {
		at = filepath.Join(l.base, at)
	}
	if filepath.Clean(at) == filepath.Clean(l.want) {
		return true
	}
	got, okGot := realpath(at)
	want, okWant := realpath(l.want)
	return okGot && okWant && got == want
}

// regularHolds reports whether path holds exactly body: a regular file with it, or, for an empty body, nothing.
func regularHolds(path, body string) bool {
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return body == ""
	}
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	b, err := os.ReadFile(path)
	return err == nil && string(b) == body
}

// replaceFile makes path a regular file holding body: whatever is there (a file, a symlink, a directory) goes first.
func replaceFile(path, body string) error {
	if st, err := os.Lstat(path); err == nil && st.IsDir() {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return writeFileAtomic(path, body)
}

// recordWorktreeConfig records in t's placement record the config.worktree git made for its worktree (none: ""), which
// restoreGitLinks restores: only at the Ticket's first launch, before any session ran in the worktree, so the record
// holds nothing a worker wrote. A Ticket that ran before has its config.worktree left alone.
func (n *Node) recordWorktreeConfig(t core.Ticket, log *slog.Logger) {
	if ranBefore(t) {
		return
	}
	p, ok, err := n.recordedPlacement(t)
	if err != nil || !ok || p.repo == "" || p.common == "" || p.worktreeConfig != nil {
		return
	}
	admin, err := n.recordedAdmin(p, log)
	if err != nil {
		return
	}
	body := ""
	path := filepath.Join(admin, "config.worktree")
	if st, err := os.Lstat(path); err == nil {
		if !st.Mode().IsRegular() {
			log.Warn("worktree config not recorded: it is no regular file", "path", path)
			return
		}
		b, err := os.ReadFile(path)
		if err != nil {
			log.Warn("worktree config not recorded", "path", path, "error", err)
			return
		}
		body = string(b)
	}
	p.worktreeConfig = &body
	if err := n.writePlacement(t, p); err != nil {
		log.Warn("worktree config not recorded", "error", err)
	}
}
