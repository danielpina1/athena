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
	c := n.cfg.Sandbox
	if c == nil {
		return nil, nil
	}
	sb := &rt.Sandbox{
		Required:   c.Required,
		AllowWrite: append([]string{workDir}, c.AllowWrite...),
		// Athena's own files, and a repository's git config and hooks (ProtectedPaths).
		DenyWrite:              ProtectedPaths(workDir),
		DenyRead:               slices.Clone(c.DenyRead),
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
// whose sandbox takes them at start (Codex's), so that a worktree can commit: the worktree's git directory and the
// repository's objects, refs and logs. Codex keeps a .git directory inside a writable root read-only, and a root of the
// whole common directory does not let a commit through (docs/dev/codex.md §5); the config and hooks stay outside every
// root. Nil outside a git repository.
func writableRoots(workDir string) []string {
	gitDir, common := gitDirs(workDir)
	if common == "" {
		return nil
	}
	return []string{gitDir, filepath.Join(common, "objects"), filepath.Join(common, "refs"), filepath.Join(common, "logs")}
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
