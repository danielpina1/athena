package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
// only through a credential helper, never in a URL, an argument, a file or an environment.
//
// The clone's config and hooks are shared with the worktrees, so a worker can write them. The node's own git on a
// clone therefore trusts none of it: every call runs with command-scope config that disables hooks and fsmonitor, allows
// only the https transport and verifies TLS, and fetches by explicit URL and refspec, never by the remote's name. A
// node-owned system config file (read before every other) maps the clone URL to itself, which beats any insteadOf a
// worker or the owner's global config adds, however long. Only the clone and fetch get a credential helper, scoped to
// https://github.com, and it answers from the node's memory through the socket with a one-call nonce.

const githubHost = "github.com"

// repoNameRe is one part of owner/name as GitHub allows it.
var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

var (
	// githubTarget is where a repository's clone URL leads: the URL itself; tests point it at a local stand-in.
	githubTarget = cloneURL
	// nodeGitExtra is command-scope config the node's git gets after its own; tests allow the file transport.
	nodeGitExtra [][2]string
)

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

// cloneURL is a repository's remote: the https URL, lower-case, without credentials.
func cloneURL(repo string) string {
	return "https://" + githubHost + "/" + strings.ToLower(repo) + ".git"
}

// systemConfig is the node-owned git system config of a clone's git, beside the clone.
func systemConfig(clone string) string { return clone + ".gitconfig" }

// repoLocks serializes the git work on one clone (clone, fetch, worktree add and remove) across Tickets.
var repoLocks sync.Map // clone dir -> *sync.Mutex

func lockRepo(dir string) func() {
	m, _ := repoLocks.LoadOrStore(dir, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// nodeAuth is the credential helper of one clone or fetch: `<bin> git-credential --node <nonce>`, asking socket.
type nodeAuth struct {
	bin, socket, nonce string
}

// nodeGitEnvNames are the variables of the node's environment its git keeps: what finds the owner's git config, the
// network proxies and the CA files. Nothing else of the server's environment reaches a process a clone may start.
var (
	nodeGitEnvNames = map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "TMPDIR": true, "LANG": true,
		"XDG_CONFIG_HOME": true, "HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true, "http_proxy": true,
		"https_proxy": true, "no_proxy": true, "all_proxy": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true}
)

// cloneGitEnv is the environment of the node's git on a clone of repo: the hardening above, and with auth a credential
// helper for https://github.com.
func cloneGitEnv(clone, repo string, auth *nodeAuth) []string {
	var env []string
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); nodeGitEnvNames[name] || strings.HasPrefix(name, "LC_") {
			env = append(env, kv)
		}
	}
	env = append(env, "GIT_CONFIG_SYSTEM="+systemConfig(clone), "GIT_TERMINAL_PROMPT=0")
	cfg := [][2]string{
		{"core.hooksPath", os.DevNull}, {"core.fsmonitor", "false"},
		{"protocol.allow", "never"}, {"protocol.https.allow", "always"}, {"protocol.http.allow", "never"},
		{"protocol.file.allow", "never"}, {"protocol.git.allow", "never"}, {"protocol.ssh.allow", "never"},
		{"protocol.ext.allow", "never"},
		{"http.sslVerify", "true"}, {"http." + cloneURL(repo) + ".sslVerify", "true"},
		{"credential.helper", ""}, // no helper of the owner's or a worker's answers for the node
	}
	if auth != nil {
		env = append(env, "ATHENA_SOCKET="+auth.socket)
		cfg = append(cfg,
			[2]string{"credential.https://" + githubHost + ".helper", "!" + shellQuote(auth.bin) + " git-credential --node " + auth.nonce},
			[2]string{"credential.https://" + githubHost + ".useHttpPath", "true"},
			[2]string{"credential.useHttpPath", "true"})
	}
	return withGitConfig(env, append(cfg, nodeGitExtra...))
}

// writeSystemConfig writes a clone's system config: the clone URL rewritten to githubTarget, which is the URL itself
// outside tests. Being read first, it wins over every insteadOf of the same length in later config.
func writeSystemConfig(clone, repo string) error {
	q := func(s string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) }
	body := fmt.Sprintf("[url \"%s\"]\n\tinsteadOf = %s\n", q(githubTarget(repo)), cloneURL(repo))
	tmp := systemConfig(clone) + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, systemConfig(clone))
}

// nodeGrant is a token the node's helper may fetch, for one repository, while one git call runs.
type nodeGrant struct {
	repo  string
	token secret
}

// grantNode makes a nonce for one git call of the node on repo; revoke ends it.
func (n *Node) grantNode(repo, tok string) (nonce string, revoke func()) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail
	}
	nonce = hex.EncodeToString(b)
	n.grants.Store(nonce, nodeGrant{repo: repo, token: secret(tok)})
	return nonce, func() { n.grants.Delete(nonce) }
}

// EnsureClone makes sure the node has a clone of repo (owner/name) at CloneDir, cloning it the first time and
// fetching it otherwise, and returns its directory and the remote's default branch. tok authenticates the clone and
// the fetch through the node's credential helper; it is never in the clone's config, a URL, an argument or an
// environment.
func (n *Node) EnsureClone(ctx context.Context, repo, tok string) (dir, defaultBranch string, err error) {
	if _, _, err := splitRepo(repo); err != nil {
		return "", "", err
	}
	if n.cfg.StateDir == "" {
		return "", "", fmt.Errorf("no state directory for repository clones")
	}
	dir = CloneDir(n.cfg.StateDir, repo)
	unlock := lockRepo(dir)
	defer unlock()
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", "", err
	}
	if err := writeSystemConfig(dir, repo); err != nil {
		return "", "", err
	}
	nonce, revoke := n.grantNode(repo, tok)
	defer revoke()
	env := cloneGitEnv(dir, repo, &nodeAuth{bin: n.cfg.AthenaBin, socket: n.cfg.SocketPath, nonce: nonce})
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		// A clone that died half-way leaves a directory without a usable repository: start over.
		_ = os.RemoveAll(dir)
		if _, err := runGit(ctx, "", env, "clone", "--quiet", cloneURL(repo), dir); err != nil {
			_ = os.RemoveAll(dir)
			return "", "", fmt.Errorf("clone %s: %w", repo, err)
		}
	} else if _, err := runGit(ctx, dir, env, "fetch", "--quiet", "--prune", cloneURL(repo), "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return "", "", fmt.Errorf("fetch %s: %w", repo, err)
	}
	revoke()
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", "", err
	}
	defaultBranch, err = cloneDefaultBranch(ctx, dir, cloneGitEnv(dir, repo, nil))
	if err != nil {
		return "", "", err
	}
	return dir, defaultBranch, nil
}

// cloneDefaultBranch is the remote's default branch: what origin/HEAD names, else the branch the clone checked out.
func cloneDefaultBranch(ctx context.Context, dir string, env []string) (string, error) {
	if ref, err := runGit(ctx, dir, env, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if b, ok := strings.CutPrefix(ref, "origin/"); ok && b != "" {
			return b, nil
		}
	}
	b, err := runGit(ctx, dir, env, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || b == "" {
		return "", fmt.Errorf("cannot tell the default branch of %s: %v", dir, err)
	}
	return b, nil
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
	clone, def, err := n.EnsureClone(n.ctx, p.github, tok)
	if err != nil {
		return err
	}
	unlock := lockRepo(clone)
	defer unlock()
	return ensureWorktree(clone, p.workDir, branchName(s.t), "origin/"+def, cloneGitEnv(clone, p.github, nil))
}
