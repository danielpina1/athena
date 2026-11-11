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

// mirrorConfig is the whole config of a mirror, rewritten before each of the node's git calls on it: a worker can
// reach the mirror's files by path, so nothing it writes there (a remote, a proxy, a CA file, an include) survives.
// A bare repository needs nothing else; the fetches name their URL and refspec.
const mirrorConfig = "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n"

// mirrorStray are entries of a git directory that would make git read config or another repository from elsewhere
// (a linked worktree's commondir and gitdir, worktree config, a nested .git that local transports try first) or
// define remotes outside config (remotes/, branches/); a mirror has none of them, so the node removes any it finds.
// The rest of the directory carries no config: info/ holds attributes, exclude and the like, which a bare fetch does
// not read; objects/ and refs/ hold data, and hooks never run (core.hooksPath).
var mirrorStray = []string{".git", "commondir", "gitdir", "config.worktree", "remotes", "branches"}

// unknownHead is the branch a mirror's HEAD names before GitHub said which is the default: a valid, unborn branch.
const unknownHead = "athena-unknown"

// sanitizeMirror rewrites a mirror's config to mirrorConfig and its HEAD to name branch (the default branch GitHub
// advertised, or unknownHead before it is known), atomically, and removes mirrorStray. Neither file is ever read to
// choose a worktree's base: that comes from GitHub's advertisement (syncMirror).
func sanitizeMirror(mirror, branch string) error {
	for _, name := range mirrorStray {
		if err := os.RemoveAll(filepath.Join(mirror, name)); err != nil {
			return err
		}
	}
	if branch == "" {
		branch = unknownHead
	}
	if err := writeFileAtomic(filepath.Join(mirror, "HEAD"), "ref: refs/heads/"+branch+"\n"); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(mirror, "config"), mirrorConfig)
}

// mirrorGit runs git on a mirror: its config and HEAD are made the node's first, and the repository is named by
// GIT_DIR, so no discovery (a planted .git) chooses another.
func mirrorGit(ctx context.Context, mirror, branch string, env []string, args ...string) (string, error) {
	if err := sanitizeMirror(mirror, branch); err != nil {
		return "", fmt.Errorf("reset the mirror's config: %w", err)
	}
	return runGit(ctx, "", append(env, "GIT_DIR="+mirror), args...)
}

// writeFileAtomic replaces path with body (0600): it writes a new temporary file beside it (O_EXCL, a random name, so
// nothing planted is followed), syncs it and renames it over path, which replaces a symlink there rather than follow it.
func writeFileAtomic(path, body string) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".athena-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.WriteString(body); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
		{"http.proxy", proxy}, {"http." + u + ".proxy", proxy}, {"remote." + u + ".proxy", proxy},
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

// cloneGitEnv is the environment of the node's git on the worktrees' clone: the clone's node-written system file
// (which maps the mirror's path to itself), no credential helper and no token, only the file protocol (the mirror),
// hooks and fsmonitor off. The owner's global config still applies (the checkout of a worktree needs its filters).
func cloneGitEnv(clone, repo string) []string {
	env := append(nodeGitBaseEnv(), "GIT_CONFIG_SYSTEM="+systemConfig(clone))
	return withGitConfig(env, append(slices.Clone(noHooks), [][2]string{
		{"protocol.allow", "never"}, {"protocol.file.allow", "always"}, {"credential.helper", ""},
	}...))
}

// writeSystemConfig writes the system config of the node's git on dir: from rewritten to to. Being read first, it wins
// over every insteadOf of the same length in later config. For a mirror it maps the clone URL to githubTarget (the URL
// itself outside tests); for the clone, the mirror's path to itself.
func writeSystemConfig(dir, from, to string) error {
	q := func(s string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) }
	return writeFileAtomic(systemConfig(dir), fmt.Sprintf("[url \"%s\"]\n\tinsteadOf = %s\n", q(to), q(from)))
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

// EnsureClone brings the node's copies of repo (owner/name) up to date and returns the worktrees' clone, the remote's
// default branch and base, the commit GitHub advertises for it, which new worktrees start from. It asks GitHub for its
// HEAD and fetches the mirror from it with tok, through the node's credential helper (tok is never in a config, a
// URL, an argument or an environment), then clones or fetches the clone from the mirror with no credential at all, and
// checks the clone has base: a fetch a worker redirected fails the launch rather than set its base.
func (n *Node) EnsureClone(ctx context.Context, repo, tok string) (clone, defaultBranch, base string, err error) {
	if _, _, err := splitRepo(repo); err != nil {
		return "", "", "", err
	}
	if n.cfg.StateDir == "" {
		return "", "", "", fmt.Errorf("no state directory for repository clones")
	}
	mirror, clone := MirrorDir(n.cfg.StateDir, repo), CloneDir(n.cfg.StateDir, repo)
	unlock := lockRepo(clone)
	defer unlock()
	for _, d := range []string{filepath.Dir(mirror), filepath.Dir(clone)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", "", "", err
		}
	}
	if err := writeSystemConfig(mirror, cloneURL(repo), githubTarget(repo)); err != nil {
		return "", "", "", err
	}
	if err := writeSystemConfig(clone, mirror, mirror); err != nil {
		return "", "", "", err
	}
	defaultBranch, base, err = n.syncMirror(ctx, mirror, repo, tok)
	if err != nil {
		return "", "", "", err
	}
	env := cloneGitEnv(clone, repo)
	// Once it exists, git's directories are the clone's own, never found from its files.
	cenv := repoGitEnv(env, filepath.Join(clone, ".git"), clone)
	if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
		_ = os.RemoveAll(clone)
		if _, err := runGit(ctx, "", env, "clone", "--quiet", mirror, clone); err != nil {
			_ = os.RemoveAll(clone)
			return "", "", "", fmt.Errorf("clone %s from its mirror: %w", repo, err)
		}
	} else if _, err := runGit(ctx, clone, cenv, "fetch", "--quiet", "--prune", mirror, "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return "", "", "", fmt.Errorf("fetch %s from its mirror: %w", repo, err)
	}
	env = cenv
	if _, err := runGit(ctx, clone, env, "cat-file", "-e", base+"^{commit}"); err != nil {
		return "", "", "", fmt.Errorf("the clone of %s lacks the %s commit %s GitHub advertised, after its fetch: %w", repo, defaultBranch, base, err)
	}
	// Fetches come from the mirror; a worker's git push goes to GitHub, with its session's own token.
	if _, err := runGit(ctx, clone, env, "config", "remote.origin.url", mirror); err != nil {
		return "", "", "", err
	}
	if _, err := runGit(ctx, clone, env, "config", "remote.origin.pushurl", cloneURL(repo)); err != nil {
		return "", "", "", err
	}
	if err := os.Chmod(clone, 0o700); err != nil {
		return "", "", "", err
	}
	return clone, defaultBranch, base, nil
}

// syncMirror brings the mirror up to date with GitHub and returns the default branch and its commit as GitHub
// advertised them: one token-bearing call asks for HEAD (ls-remote --symref), a second fetches the branches and that
// exact commit, each with its own single-use nonce. Nothing in the mirror's files is read to choose them.
func (n *Node) syncMirror(ctx context.Context, mirror, repo, tok string) (branch, sha string, err error) {
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		// No mirror yet, or one that died half-way: start over with an empty one.
		_ = os.RemoveAll(mirror)
		if _, err := runGit(ctx, "", mirrorGitEnv(mirror, repo, nil), "init", "--quiet", "--bare", "--template=", mirror); err != nil {
			_ = os.RemoveAll(mirror)
			return "", "", fmt.Errorf("make the mirror of %s: %w", repo, err)
		}
	}
	out, err := n.tokenGit(ctx, mirror, repo, tok, "", "ls-remote", "--symref", cloneURL(repo), "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("ask %s for its default branch: %w", repo, err)
	}
	if branch, sha, err = parseRemoteHead(ctx, out); err != nil {
		return "", "", fmt.Errorf("%s: %w", repo, err)
	}
	if _, err := n.tokenGit(ctx, mirror, repo, tok, branch, "fetch", "--quiet", "--prune", cloneURL(repo),
		"+refs/heads/*:refs/heads/*", sha); err != nil {
		return "", "", fmt.Errorf("fetch %s: %w", repo, err)
	}
	if n.afterMirrorFetch != nil {
		n.afterMirrorFetch(mirror)
	}
	if _, err := mirrorGit(ctx, mirror, branch, mirrorGitEnv(mirror, repo, nil), "cat-file", "-e", sha+"^{commit}"); err != nil {
		return "", "", fmt.Errorf("the mirror of %s lacks the %s commit %s GitHub advertised: %w", repo, branch, sha, err)
	}
	if err := sanitizeMirror(mirror, branch); err != nil {
		return "", "", err
	}
	return branch, sha, os.Chmod(mirror, 0o700)
}

// tokenGit runs one token-bearing git call on a mirror, with a nonce of its own, revoked when the call returns.
func (n *Node) tokenGit(ctx context.Context, mirror, repo, tok, branch string, args ...string) (string, error) {
	nonce, revoke := n.grantNode(repo, tok)
	defer revoke()
	return mirrorGit(ctx, mirror, branch, mirrorGitEnv(mirror, repo, &nodeAuth{bin: n.cfg.AthenaBin, socket: n.cfg.SocketPath, nonce: nonce}), args...)
}

// shaRe is a full object name, SHA-1 or SHA-256.
var shaRe = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// parseRemoteHead reads `git ls-remote --symref <url> HEAD`: the branch HEAD names and its commit. The branch must be
// a valid branch name (git check-ref-format), as it goes into the mirror's HEAD and a refspec.
func parseRemoteHead(ctx context.Context, out string) (branch, sha string, err error) {
	for _, line := range strings.Split(out, "\n") {
		ref, name, ok := strings.Cut(line, "\t")
		if !ok || name != "HEAD" {
			continue
		}
		if b, isSym := strings.CutPrefix(ref, "ref: refs/heads/"); isSym {
			branch = b
		} else if shaRe.MatchString(ref) {
			sha = ref
		}
	}
	if branch == "" || sha == "" {
		return "", "", fmt.Errorf("the remote advertises no default branch and commit for HEAD")
	}
	if strings.ContainsAny(branch, " \t\r\n\\") || strings.HasPrefix(branch, "-") {
		return "", "", fmt.Errorf("the remote's default branch %q is not a branch name", branch)
	}
	if _, err := runGit(ctx, "", nodeGitBaseEnv(), "check-ref-format", "refs/heads/"+branch); err != nil {
		return "", "", fmt.Errorf("the remote's default branch %q is not a branch name", branch)
	}
	return branch, sha, nil
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
// Assignment's token and adds the worktree on the Ticket's branch from origin/<default branch>, or, for a Ticket that
// moved here from another node, from its seed (seed.go); an existing worktree (a resumed Ticket) is used as it is,
// unless a seed of the Assignment moves the Ticket's work back to it (reseedWorktree).
func (n *Node) prepareGitHub(s *session) error {
	p := s.place
	rec, seeded := n.seedFor(s.t, s.a.ID)
	seeded = seeded && rec.Note == ""
	if st, err := os.Stat(p.workDir); err == nil {
		if !st.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", p.workDir)
		}
		if seeded {
			unlock := lockRepo(p.repo)
			defer unlock()
			return n.reseedWorktree(s, rec)
		}
		return nil
	}
	tok, err := n.tokens.token(n.ctx, s.a.ID, p.github, n.heldForOwner(s))
	if err != nil {
		return err
	}
	clone, defaultBranch, base, err := n.EnsureClone(n.ctx, p.github, tok)
	if err != nil {
		return err
	}
	unlock := lockRepo(clone)
	defer unlock()
	env := cloneGitEnv(clone, p.github)
	// A child starts from its lead's commit (Execution.Base), when this clone has it (childgit.go).
	cenv := repoGitEnv(env, filepath.Join(clone, ".git"), clone)
	childBase, baseErr := worktreeBase(func(args ...string) (string, error) { return gitIn(clone, cenv, args...) }, s.t, base)
	if seeded {
		// The Ticket moved here from another node: its work comes from the server's checkpoint (seed.go), or, for a
		// child forked from its lead elsewhere, its branch from the lead's (childgit.go).
		if baseErr == nil {
			base = childBase
		}
		return n.seedWorktree(s, clone, defaultBranch, base, env, rec)
	}
	if baseErr != nil {
		return baseErr
	}
	return ensureWorktree(clone, p.workDir, branchName(s.t), childBase, env)
}
