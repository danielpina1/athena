package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// GitHub repositories on a node: one clone per repository under <stateDir>/repos/<owner>/<name> (lower-case), shared
// by the worktrees of every Ticket that works in it. The clone's remote is the plain https URL; the token reaches git
// only through a credential helper, never in a URL, an argument or a file.

const (
	githubHost = "github.com"
	// cloneTokenEnv carries the token to the credential helper of a git process the node itself runs (clone, fetch);
	// it is set only in that process's environment, never in a session's.
	cloneTokenEnv = "ATHENA_NODE_GIT_TOKEN"
	// cloneHelper answers git's credential request from cloneTokenEnv; store and erase do nothing.
	cloneHelper = `!f() { test "$1" = get && printf 'username=x-access-token\npassword=%s\n' "$` + cloneTokenEnv + `"; }; f`
)

// repoNameRe is one part of owner/name as GitHub allows it.
var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// splitRepo checks that repo is owner/name and returns both parts.
func splitRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !repoNameRe.MatchString(owner) || !repoNameRe.MatchString(name) ||
		owner == "." || owner == ".." || name == "." || name == ".." {
		return "", "", fmt.Errorf("repository %q is not owner/name", repo)
	}
	return owner, name, nil
}

// CloneDir is the directory of a repository's clone: <stateDir>/repos/<owner>/<name>, lower-case.
func CloneDir(stateDir, repo string) string {
	owner, name, _ := strings.Cut(strings.ToLower(repo), "/")
	return filepath.Join(stateDir, "repos", owner, name)
}

// cloneURL is a repository's remote: the https URL, without credentials.
func cloneURL(repo string) string { return "https://" + githubHost + "/" + repo + ".git" }

// repoLocks serializes the git work on one clone (clone, fetch, worktree add and remove) across Tickets.
var repoLocks sync.Map // clone dir -> *sync.Mutex

func lockRepo(dir string) func() {
	m, _ := repoLocks.LoadOrStore(dir, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// EnsureClone makes sure the node has a clone of repo (owner/name) at CloneDir, cloning it the first time and
// fetching it otherwise, and returns its directory and the remote's default branch. tok authenticates the clone and
// the fetch through a credential helper; it is never written to the clone's config, its remote URL or an argument.
func EnsureClone(ctx context.Context, stateDir, repo, tok string) (dir, defaultBranch string, err error) {
	if _, _, err := splitRepo(repo); err != nil {
		return "", "", err
	}
	if stateDir == "" {
		return "", "", errors.New("no state directory for repository clones")
	}
	dir = CloneDir(stateDir, repo)
	unlock := lockRepo(dir)
	defer unlock()
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", "", err
	}
	env := cloneEnv(tok)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		// A clone that died half-way leaves a directory without a usable repository: start over.
		_ = os.RemoveAll(dir)
		if _, err := runGit(ctx, "", env, "clone", "--quiet", cloneURL(repo), dir); err != nil {
			_ = os.RemoveAll(dir)
			return "", "", fmt.Errorf("clone %s: %w", repo, err)
		}
	} else if _, err := runGit(ctx, dir, env, "fetch", "--quiet", "--prune", "origin"); err != nil {
		return "", "", fmt.Errorf("fetch %s: %w", repo, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", "", err
	}
	defaultBranch, err = cloneDefaultBranch(ctx, dir)
	if err != nil {
		return "", "", err
	}
	return dir, defaultBranch, nil
}

// cloneDefaultBranch is the remote's default branch: what origin/HEAD names, else the branch the clone checked out.
func cloneDefaultBranch(ctx context.Context, dir string) (string, error) {
	if ref, err := runGit(ctx, dir, nil, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if b, ok := strings.CutPrefix(ref, "origin/"); ok && b != "" {
			return b, nil
		}
	}
	b, err := runGit(ctx, dir, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || b == "" {
		return "", fmt.Errorf("cannot tell the default branch of %s: %v", dir, err)
	}
	return b, nil
}

// cloneEnv is the environment of a git process the node runs on a clone: the node's own, with the credential helpers
// replaced by cloneHelper, and the token for it.
func cloneEnv(tok string) []string {
	env := withGitConfig(os.Environ(), [][2]string{
		{"credential.helper", ""}, // drop the owner's helpers (a keychain would store the token)
		{"credential.helper", cloneHelper},
	})
	return append(env, cloneTokenEnv+"="+tok)
}

// withGitConfig appends git config entries to env through GIT_CONFIG_COUNT/KEY_n/VALUE_n, after the entries env
// already carries, so both apply.
func withGitConfig(env []string, kvs [][2]string) []string {
	count := 0
	var out []string
	for _, kv := range env {
		name, v, _ := strings.Cut(kv, "=")
		if name == "GIT_CONFIG_COUNT" {
			count, _ = strconv.Atoi(v)
			continue
		}
		out = append(out, kv)
	}
	for i, kv := range kvs {
		n := strconv.Itoa(count + i)
		out = append(out, "GIT_CONFIG_KEY_"+n+"="+kv[0], "GIT_CONFIG_VALUE_"+n+"="+kv[1])
	}
	return append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(count+len(kvs)))
}

// runGit runs git (in dir, unless empty) with env (the node's, if nil) and returns its trimmed stdout. Its error
// carries git's stderr, which names URLs but never the token.
func runGit(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = append(env, "GIT_TERMINAL_PROMPT=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// prepareGitHub makes a GitHub Ticket's worktree: the first time, it clones or fetches the repository with the
// Assignment's token and adds the worktree on the Ticket's branch from origin/<default branch>; an existing worktree
// (a resumed Ticket) is used as it is.
func (n *Node) prepareGitHub(s *session) error {
	p := s.place
	if st, err := os.Stat(p.workDir); err == nil {
		if !st.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", p.workDir)
		}
		return nil
	}
	tok, err := n.tokens.token(n.ctx, s.a.ID, p.github)
	if err != nil {
		return err
	}
	clone, def, err := EnsureClone(n.ctx, n.cfg.StateDir, p.github, tok)
	if err != nil {
		return err
	}
	unlock := lockRepo(clone)
	defer unlock()
	return ensureWorktree(clone, p.workDir, branchName(s.t), "origin/"+def)
}
