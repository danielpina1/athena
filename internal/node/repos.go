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
	"slices"
	"strconv"
	"strings"
	"sync"
)

// GitHub repositories on a node, in two parts (ruling R26):
//
//   - The mirror, <stateDir>/mirrors/<owner>/<name>.git (lower-case, bare, 0700), is the node's alone: no worktree is
//     ever attached to it, so no worker git writes its config or hooks. It is the only repository the token is used
//     for. Its git reads no global config, only a node-written system file (which maps the clone URL to itself, beating
//     any insteadOf), and runs with command-scope config that disables hooks and fsmonitor, allows only https, verifies
//     TLS and pins the proxy and curl's resolve list. It fetches by explicit URL and refspec, with a credential helper
//     scoped to https://github.com that answers from the node's memory through the socket with a one-call nonce: the
//     token is never in a URL, an argument, a file or an environment.
//   - The clone, <stateDir>/repos/<owner>/<name>, is what the Tickets' worktrees hang off, so its config and hooks are
//     the workers' to write. Its origin is the mirror's path and its push URL the GitHub URL (a worker pushes with its
//     session's own scoped token). The node fetches it from the mirror over the file protocol only, with no credential
//     helper and no token, hooks and fsmonitor off: whatever a worker plants there reaches only that local fetch.

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

// MirrorDir is the node-only bare mirror of a repository: <stateDir>/mirrors/<owner>/<name>.git, lower-case.
func MirrorDir(stateDir, repo string) string {
	owner, name, _ := strings.Cut(strings.ToLower(repo), "/")
	return filepath.Join(stateDir, "mirrors", owner, name+".git")
}

// cloneURL is a repository's remote: the https URL, lower-case, without credentials.
func cloneURL(repo string) string {
	return "https://" + githubHost + "/" + strings.ToLower(repo) + ".git"
}

// systemConfig is the node-owned git system config of a mirror's git, beside the mirror.
func systemConfig(mirror string) string { return mirror + ".gitconfig" }

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

// nodeGitEnvNames are the variables of the node's environment its git keeps: the user's identity and locale, the
// proxies and the CA files. Nothing else of the server's environment reaches a process a node git may start.
var nodeGitEnvNames = map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "TMPDIR": true, "LANG": true,
	"XDG_CONFIG_HOME": true, "HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true, "http_proxy": true,
	"https_proxy": true, "no_proxy": true, "all_proxy": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true}

func nodeGitBaseEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); nodeGitEnvNames[name] || strings.HasPrefix(name, "LC_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_TERMINAL_PROMPT=0")
}

// noHooks are the command-scope settings of every node git: no hook and no fsmonitor of anyone's runs.
var noHooks = [][2]string{{"core.hooksPath", os.DevNull}, {"core.fsmonitor", "false"}}

// envProxy is the proxy the node's own environment names for https, or "".
func envProxy() string {
	for _, v := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if p := os.Getenv(v); p != "" {
			return p
		}
	}
	return ""
}

// mirrorGitEnv is the environment of the node's git on a mirror of repo: no global config, the node's system file,
// the hardening above, and with auth the credential helper for https://github.com.
func mirrorGitEnv(mirror, repo string, auth *nodeAuth) []string {
	env := append(nodeGitBaseEnv(), "GIT_CONFIG_SYSTEM="+systemConfig(mirror), "GIT_CONFIG_GLOBAL="+os.DevNull)
	u := cloneURL(repo)
	// The proxy is pinned to the node's own (none, or its environment's): an http.proxy in a config file never applies.
	proxy := envProxy()
	cfg := append(slices.Clone(noHooks), [][2]string{
		{"protocol.allow", "never"}, {"protocol.https.allow", "always"}, {"protocol.http.allow", "never"},
		{"protocol.file.allow", "never"}, {"protocol.git.allow", "never"}, {"protocol.ssh.allow", "never"},
		{"protocol.ext.allow", "never"},
		{"http.sslVerify", "true"}, {"http." + u + ".sslVerify", "true"},
		{"http.proxy", proxy}, {"http." + u + ".proxy", proxy},
		{"http.curloptResolve", ""}, {"http." + u + ".curloptResolve", ""},
		{"credential.helper", ""}, // no helper of anyone else answers for the node
	}...)
	if auth != nil {
		env = append(env, "ATHENA_SOCKET="+auth.socket)
		cfg = append(cfg,
			[2]string{"credential.https://" + githubHost + ".helper", "!" + shellQuote(auth.bin) + " git-credential --node " + auth.nonce},
			[2]string{"credential.https://" + githubHost + ".useHttpPath", "true"},
			[2]string{"credential.useHttpPath", "true"})
	}
	return withGitConfig(env, append(cfg, nodeGitExtra...))
}

// cloneGitEnv is the environment of the node's git on the worktrees' clone: no system config, no credential helper and
// no token, only the file protocol (the mirror), hooks and fsmonitor off. The owner's global config still applies (the
// checkout of a worktree needs its filters).
func cloneGitEnv(clone, repo string) []string {
	env := append(nodeGitBaseEnv(), "GIT_CONFIG_NOSYSTEM=1")
	return withGitConfig(env, append(slices.Clone(noHooks), [][2]string{
		{"protocol.allow", "never"}, {"protocol.file.allow", "always"}, {"credential.helper", ""},
	}...))
}

// writeSystemConfig writes a mirror's system config: the clone URL rewritten to githubTarget, which is the URL itself
// outside tests. Being read first, it wins over every insteadOf of the same length in later config.
func writeSystemConfig(mirror, repo string) error {
	q := func(s string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) }
	body := fmt.Sprintf("[url \"%s\"]\n\tinsteadOf = %s\n", q(githubTarget(repo)), cloneURL(repo))
	tmp := systemConfig(mirror) + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, systemConfig(mirror))
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

// EnsureClone brings the node's copies of repo (owner/name) up to date and returns the worktrees' clone and the
// remote's default branch: it clones or fetches the mirror from GitHub with tok, through the node's credential helper
// (tok is never in a config, a URL, an argument or an environment), then clones or fetches the clone from the mirror
// with no credential at all.
func (n *Node) EnsureClone(ctx context.Context, repo, tok string) (clone, defaultBranch string, err error) {
	if _, _, err := splitRepo(repo); err != nil {
		return "", "", err
	}
	if n.cfg.StateDir == "" {
		return "", "", fmt.Errorf("no state directory for repository clones")
	}
	mirror, clone := MirrorDir(n.cfg.StateDir, repo), CloneDir(n.cfg.StateDir, repo)
	unlock := lockRepo(clone)
	defer unlock()
	for _, d := range []string{filepath.Dir(mirror), filepath.Dir(clone)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", "", err
		}
	}
	if err := writeSystemConfig(mirror, repo); err != nil {
		return "", "", err
	}
	if err := n.syncMirror(ctx, mirror, repo, tok); err != nil {
		return "", "", err
	}
	// The mirror is the node's own: its HEAD, not the clone's, names the default branch.
	defaultBranch, err = runGit(ctx, mirror, mirrorGitEnv(mirror, repo, nil), "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || defaultBranch == "" {
		return "", "", fmt.Errorf("cannot tell the default branch of %s: %v", repo, err)
	}
	env := cloneGitEnv(clone, repo)
	if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
		_ = os.RemoveAll(clone)
		if _, err := runGit(ctx, "", env, "clone", "--quiet", mirror, clone); err != nil {
			_ = os.RemoveAll(clone)
			return "", "", fmt.Errorf("clone %s from its mirror: %w", repo, err)
		}
	} else if _, err := runGit(ctx, clone, env, "fetch", "--quiet", "--prune", mirror, "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return "", "", fmt.Errorf("fetch %s from its mirror: %w", repo, err)
	}
	// Fetches come from the mirror; a worker's git push goes to GitHub, with its session's own token.
	if _, err := runGit(ctx, clone, env, "config", "remote.origin.url", mirror); err != nil {
		return "", "", err
	}
	if _, err := runGit(ctx, clone, env, "config", "remote.origin.pushurl", cloneURL(repo)); err != nil {
		return "", "", err
	}
	if err := os.Chmod(clone, 0o700); err != nil {
		return "", "", err
	}
	return clone, defaultBranch, nil
}

// syncMirror clones the mirror from GitHub the first time and fetches it otherwise, with the token through a nonce
// valid for that one git call.
func (n *Node) syncMirror(ctx context.Context, mirror, repo, tok string) error {
	nonce, revoke := n.grantNode(repo, tok)
	defer revoke()
	env := mirrorGitEnv(mirror, repo, &nodeAuth{bin: n.cfg.AthenaBin, socket: n.cfg.SocketPath, nonce: nonce})
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		// A clone that died half-way leaves a directory without a usable repository: start over.
		_ = os.RemoveAll(mirror)
		if _, err := runGit(ctx, "", env, "clone", "--bare", "--quiet", cloneURL(repo), mirror); err != nil {
			_ = os.RemoveAll(mirror)
			return fmt.Errorf("clone %s: %w", repo, err)
		}
	} else if _, err := runGit(ctx, mirror, env, "fetch", "--quiet", "--prune", cloneURL(repo), "+refs/heads/*:refs/heads/*"); err != nil {
		return fmt.Errorf("fetch %s: %w", repo, err)
	}
	return os.Chmod(mirror, 0o700)
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
	return ensureWorktree(clone, p.workDir, branchName(s.t), "origin/"+def, cloneGitEnv(clone, p.github))
}
