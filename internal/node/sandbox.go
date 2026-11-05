package node

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/danielpina1/athena/internal/runtime/claude"
)

// SandboxConfig turns on Claude Code's sandbox for worker sessions (claude.Sandbox). A worker's Bash commands, and
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
func (n *Node) sandboxFor(workDir string) (*claude.Sandbox, []string) {
	c := n.cfg.Sandbox
	if c == nil {
		return nil, nil
	}
	adir := filepath.Join(workDir, ".athena")
	sb := &claude.Sandbox{
		Required:   c.Required,
		AllowWrite: append([]string{workDir}, c.AllowWrite...),
		// Athena's own files: a worker that rewrote its settings would run its next process without hooks or sandbox.
		// .athena/gh stays writable: `athena gh` keeps gh's configuration there.
		DenyWrite:              []string{filepath.Join(adir, "settings.json"), filepath.Join(adir, "mcp.json"), filepath.Join(adir, ".gitignore")},
		DenyRead:               slices.Clone(c.DenyRead),
		AllowedDomains:         append(slices.Clone(GitHubDomains), c.AllowedDomains...),
		UnixSockets:            []string{n.cfg.SocketPath},
		AllowAllUnixSockets:    runtime.GOOS == "linux", // Linux cannot allow a socket by path; without this the node socket is unreachable
		WeakerNetworkIsolation: c.WeakerNetworkIsolation,
	}
	// A worktree's commits write to the repository's git directory, outside the working directory. Its config and
	// hooks stay read-only: git runs what they name, outside the sandbox, the next time the owner or the node uses the
	// repository.
	if gitDir, common := gitDirs(workDir); common != "" {
		sb.AllowWrite = append(sb.AllowWrite, common)
		// The common directory's config.worktree is read by the main worktree too, when the repository has
		// extensions.worktreeConfig: it can name a promisor remote or a program git runs outside the sandbox.
		sb.DenyWrite = append(sb.DenyWrite, filepath.Join(common, "config"), filepath.Join(common, "config.worktree"), filepath.Join(common, "hooks"))
		if gitDir != common {
			sb.DenyWrite = append(sb.DenyWrite, filepath.Join(gitDir, "config.worktree"))
		}
	}
	var env []string
	if c.CAFile != "" {
		for _, name := range caEnvNames {
			env = append(env, name+"="+c.CAFile)
		}
	}
	return sb, env
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
