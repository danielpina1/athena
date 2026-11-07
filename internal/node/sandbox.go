package node

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/danielpina1/athena/internal/runtime/rt"
)

// SandboxConfig turns on Claude Code's sandbox for worker sessions (rt.Sandbox). A worker's Bash commands, and
// everything they start, may then write only to the Ticket's working directory, its git directory (not the git
// config or hooks, and not Athena's files in .athena) and the sandbox's own temporary directory ($TMPDIR), and connect
// only to GitHub, AllowedDomains and the node socket (so `athena git-credential` and `athena gh` keep working).
// Claude Code's own tools (Read, Edit, Write, WebFetch), the hooks and the MCP server run outside the sandbox; the
// hook rules still govern them.
type SandboxConfig struct {
	// Required makes a worker fail to start when the sandbox cannot start (on Linux: bubblewrap or socat missing)
	// instead of running its commands unsandboxed with a warning.
	Required bool
	// AllowedDomains are hosts reachable besides GitHubDomains (a package registry, for example).
	AllowedDomains []string
	// AllowWrite are absolute paths writable besides the working directory (a build cache, for example).
	AllowWrite []string
	// DenyRead are absolute paths sandboxed commands cannot read (the server puts the GitHub App key here).
	DenyRead []string
	// AllowRead are absolute paths readable again inside DenyRead (Claude Code's allowRead, which takes precedence):
	// the node sets them for a restricted session (restrictedReads), never from the owner's configuration.
	AllowRead []string
	// WeakerNetworkIsolation (macOS) lets commands reach the trustd service, which Go programs such as gh verify every
	// TLS certificate through: without it gh cannot reach GitHub. It opens a path out of the sandbox through that
	// service.
	WeakerNetworkIsolation bool
	// CAFile, when set, is a PEM bundle the worker's commands use to verify TLS (SSL_CERT_FILE, GIT_SSL_CAINFO,
	// CURL_CA_BUNDLE): git and curl cannot reach the keychain from inside the sandbox.
	CAFile string
}

// GitHubDomains are the hosts a sandboxed worker may always reach: GitHub's web, git and API hosts and its content
// hosts.
var GitHubDomains = []string{"github.com", "*.github.com", "*.githubusercontent.com"}

// SandboxSupported reports whether Claude Code's sandbox exists on this platform (macOS and Linux).
func SandboxSupported() bool { return runtime.GOOS == "darwin" || runtime.GOOS == "linux" }

// caEnvNames are the variables a CAFile is given in: OpenSSL and Go (SSL_CERT_FILE), git, and curl.
var caEnvNames = []string{"SSL_CERT_FILE", "GIT_SSL_CAINFO", "CURL_CA_BUNDLE"}

// sandboxFor is the sandbox block of a session working in workDir, and the settings env it needs; nil without
// Config.Sandbox.
func (n *Node) sandboxFor(workDir string) (*rt.Sandbox, []string) {
	return n.sandboxWith(n.cfg.Sandbox, workDir)
}

// sandboxWith is sandboxFor with the sandbox configuration c; nil without c.
func (n *Node) sandboxWith(c *SandboxConfig, workDir string) (*rt.Sandbox, []string) {
	if c == nil {
		return nil, nil
	}
	sb := &rt.Sandbox{
		Required:   c.Required,
		AllowWrite: append([]string{workDir}, c.AllowWrite...),
		// Athena's own files, and a repository's git config and hooks (ProtectedPaths).
		DenyWrite:              ProtectedPaths(workDir),
		DenyRead:               slices.Clone(c.DenyRead),
		AllowRead:              slices.Clone(c.AllowRead),
		AllowedDomains:         append(slices.Clone(GitHubDomains), c.AllowedDomains...),
		UnixSockets:            []string{n.cfg.SocketPath},
		AllowAllUnixSockets:    runtime.GOOS == "linux", // Linux cannot allow a socket by path; without this the node socket is unreachable
		WeakerNetworkIsolation: c.WeakerNetworkIsolation,
	}
	// A worktree's commits write to the repository's git directory, outside the working directory; its config and hooks
	// stay read-only (DenyWrite).
	if _, common := gitDirs(workDir); common != "" {
		sb.AllowWrite = append(sb.AllowWrite, common)
	}
	var env []string
	if c.CAFile != "" {
		for _, name := range caEnvNames {
			env = append(env, name+"="+c.CAFile)
		}
	}
	return sb, env
}

// sessionSandbox is sandboxFor for a session working in workDir. A read-only session's (a review's) writes nothing of
// the Ticket's: of allowWrite it keeps only the owner's paths that neither hold nor lie in workDir or the repository's
// git directories (a build cache, so that its tests can run), and workDir and the git directories are denied too, since
// Claude Code's sandbox lets a session write in its working directory whatever allowWrite says. A restricted session's
// (toolpolicy.go) is always there, even with Config.Sandbox nil: required, reaching GitHub alone (restrictedSandbox),
// writing its own caches instead of the owner's, and kept from the node's secrets (restrictedSandboxConfig); its env
// points the caches there (restrictedCacheEnv).
func (n *Node) sessionSandbox(workDir string, readOnly, restricted bool) (*rt.Sandbox, []string) {
	return n.sessionSandboxOn(runtime.GOOS, workDir, readOnly, restricted)
}

// sessionSandboxOn is sessionSandbox on the operating system goos.
func (n *Node) sessionSandboxOn(goos, workDir string, readOnly, restricted bool) (*rt.Sandbox, []string) {
	c := n.cfg.Sandbox
	if restricted {
		c = n.restrictedSandboxConfig(goos, workDir)
	}
	sb, env := n.sandboxWith(c, workDir)
	if restricted {
		sb = restrictedSandbox(sb)
		// The daemons' sockets are masked for writes too: on Linux connecting to one takes no read.
		sb.DenyWrite = append(sb.DenyWrite, daemonSockets(goos, os.Getenv("XDG_RUNTIME_DIR"), n.cfg.SocketPath)...)
		env = append(env, restrictedCacheEnv(workDir)...)
	}
	if sb != nil && readOnly {
		sb.AllowWrite = []string{}
		gitDir, common := gitDirs(workDir)
		var owned []string
		for _, d := range []string{workDir, gitDir, common} {
			if d != "" {
				owned = append(owned, filepath.Clean(d), resolvedPath(d))
			}
		}
		overlaps := func(p string) bool {
			for _, c := range []string{filepath.Clean(p), resolvedPath(p)} {
				if slices.ContainsFunc(owned, func(k string) bool { return within(k, c) || within(c, k) }) {
					return true
				}
			}
			return false
		}
		for _, p := range c.AllowWrite {
			if !overlaps(p) {
				sb.AllowWrite = append(sb.AllowWrite, p)
			}
		}
		if restricted {
			// Its own caches only: a restricted review writes nothing else, the owner's caches included.
			sb.AllowWrite = append(sb.AllowWrite, restrictedCacheDir(workDir))
		}
		sb.DenyWrite = append(sb.DenyWrite, workDir)
		if common != "" {
			sb.DenyWrite = append(sb.DenyWrite, common)
		}
		if gitDir != "" && gitDir != common {
			sb.DenyWrite = append(sb.DenyWrite, gitDir)
		}
	}
	return sb, env
}

// resolvedPath is p, clean, with the symbolic links of its longest existing ancestor resolved (/var/x becomes
// /private/var/x on macOS), so that two spellings of one directory compare equal.
func resolvedPath(p string) string {
	p = filepath.Clean(p)
	for dir, rest := p, ""; ; {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return p
		}
		dir, rest = parent, filepath.Join(filepath.Base(dir), rest)
	}
}

// ProtectedPaths are the paths of a session working in workDir that nothing the session runs may write, each with
// everything below it:
//   - Athena's own files in .athena (settings.json, mcp.json, .gitignore): a worker that rewrote them would run its next
//     process without hooks or sandbox. .athena/gh stays writable: `athena gh` keeps gh's configuration there.
//   - for a repository or worktree, the git config and hooks, which git runs outside any sandbox the next time the owner
//     or the node uses the repository: the common directory's config, config.worktree (read by the main worktree too
//     when the repository has extensions.worktreeConfig: it can name a promisor remote or a program) and hooks, and a
//     worktree's own config.worktree.
//   - the files that say where the git directory is: the git directory's commondir and, for a worktree, its gitdir and
//     the working directory's .git file. A worker that rewrote one would point the owner's or the node's next git at a
//     directory of its own, whose config and hooks it wrote.
//
// Claude Code's sandbox denies these writes (sandboxFor's DenyWrite); Codex's cannot within its writable roots, so for
// Codex sessions the hook rules deny them (RuleContext.Protected).
func ProtectedPaths(workDir string) []string {
	adir := filepath.Join(workDir, ".athena")
	out := []string{filepath.Join(adir, "settings.json"), filepath.Join(adir, "mcp.json"), filepath.Join(adir, ".gitignore")}
	if gitDir, common := gitDirs(workDir); common != "" {
		out = append(out, filepath.Join(common, "config"), filepath.Join(common, "config.worktree"), filepath.Join(common, "hooks"),
			filepath.Join(gitDir, "commondir"))
		if gitDir != common {
			out = append(out, filepath.Join(gitDir, "config.worktree"), filepath.Join(gitDir, "gitdir"), filepath.Join(workDir, ".git"))
		}
	}
	return out
}

// writableRoots are the directories besides its working directory a session's sandbox must let it write, for a runtime
// whose sandbox takes them at start (Codex's):
//   - for a worktree to commit, its git directory and the repository's objects, refs and logs. Codex keeps a .git
//     directory inside a writable root read-only, and a root of the whole common directory does not let a commit through
//     (docs/dev/codex.md §5); the config and hooks stay outside every root;
//   - the owner's Sandbox.AllowWrite (a build cache), less any root that holds a protected path or lies in one: Codex's
//     sandbox has no deny list to keep it read-only. Each one dropped is logged.
func (n *Node) writableRoots(workDir string) []string {
	var roots []string
	if gitDir, common := gitDirs(workDir); common != "" {
		roots = []string{gitDir, filepath.Join(common, "objects"), filepath.Join(common, "refs"), filepath.Join(common, "logs")}
	}
	if n.cfg.Sandbox == nil {
		return roots
	}
	var protected []string
	for _, p := range ProtectedPaths(workDir) {
		if real, ok := realpath(p); ok {
			protected = append(protected, real)
		}
	}
	for _, root := range n.cfg.Sandbox.AllowWrite {
		real, ok := realpath(filepath.Clean(root))
		if !ok {
			n.log.Warn("sandbox write path left out of the codex sandbox: it does not resolve", "path", root)
			continue
		}
		if i := slices.IndexFunc(protected, func(p string) bool { return within(real, p) || within(p, real) }); i >= 0 {
			n.log.Warn("sandbox write path left out of the codex sandbox: it holds a protected path", "path", root, "protected", protected[i])
			continue
		}
		roots = append(roots, filepath.Clean(root))
	}
	return roots
}

// gitDirs returns the git directory of the repository or worktree whose top level is dir and its common directory
// (the same for a repository; the main repository's for a worktree), or "" for both when dir has no .git entry. It
// reads the .git file and the commondir file, without running git.
func gitDirs(dir string) (gitDir, common string) {
	dotGit := filepath.Join(dir, ".git")
	st, err := os.Lstat(dotGit)
	if err != nil {
		return "", ""
	}
	if st.IsDir() {
		return dotGit, dotGit
	}
	b, err := os.ReadFile(dotGit)
	if err != nil {
		return "", ""
	}
	gd, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return "", ""
	}
	gitDir = strings.TrimSpace(gd)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	gitDir = filepath.Clean(gitDir)
	common = gitDir
	if b, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		common = strings.TrimSpace(string(b))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitDir, common)
		}
		common = filepath.Clean(common)
	}
	return gitDir, common
}
