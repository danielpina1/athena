package node

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/runtime/rt"
)

// Policy is what the node reads of the owner's Policy (Config.Policy): the tool policy of a trust class, the allow
// rules and the repositories in shadow mode. Each call reads the current Policy.
type Policy interface {
	// ToolPolicy is the tool policy (policy.ToolStandard, policy.ToolRestricted) of work a sender of the class started.
	ToolPolicy(class string) string
	// Allows reports whether an allow rule matches the tool, rule and repository.
	Allows(tool, rule, repo string) bool
	// Shadow reports whether the repository's work is held for the owner.
	Shadow(repo string) bool
}

// The restricted tool policy's words to the model and the owner.
const (
	// restrictedEscalateReason is the reason of what a restricted session's rules would escalate.
	restrictedEscalateReason = "Work started by someone outside your allow list cannot ask for this; the owner can rerun it as their own task."
	// restrictedWebReason is the reason a restricted session's web tools are denied.
	restrictedWebReason = "Work started by someone outside your allow list has no web access beyond GitHub."
	// restrictedSandboxSummary heads the Failed report of a restricted session that cannot be sandboxed.
	restrictedSandboxSummary = "This Ticket came from someone outside your allow list, and it runs only inside the sandbox, which this machine cannot start: "
	// restrictedFollowReason is why a restricted session cannot follow other Threads.
	restrictedFollowReason = "work started by someone outside your allow list cannot follow other threads"
	// restrictedRuntimeSummary is the Failed report of a restricted launch pinned to a runtime other than Claude Code.
	restrictedRuntimeSummary = "This Ticket came from someone outside your allow list, and such work runs only on Claude Code, whose sandbox can keep it from your secrets."
)

// toolPolicy is the tool policy of the Ticket's sessions: the Policy's for the trust class of its Mandate. Without a
// Policy it is standard; a tool policy the node does not know is restricted.
func (n *Node) toolPolicy(t core.Ticket) string {
	if n.cfg.Policy == nil {
		return policy.ToolStandard
	}
	if n.cfg.Policy.ToolPolicy(t.Mandate.TrustClass()) == policy.ToolStandard {
		return policy.ToolStandard
	}
	return policy.ToolRestricted
}

// restrictedSandbox is a copy of sb for a restricted session: Required, and reaching GitHub alone (GitHubDomains; the
// owner's AllowedDomains are dropped). A nil sb gives a sandbox with nothing else.
func restrictedSandbox(sb *rt.Sandbox) *rt.Sandbox {
	var out rt.Sandbox
	if sb != nil {
		out = *sb
		out.AllowWrite, out.DenyWrite, out.DenyRead = slices.Clone(sb.AllowWrite), slices.Clone(sb.DenyWrite), slices.Clone(sb.DenyRead)
		out.UnixSockets = slices.Clone(sb.UnixSockets)
	}
	out.Required = true
	out.AllowedDomains = slices.Clone(GitHubDomains)
	return &out
}

// restrictedSandboxConfig is the sandbox configuration of a restricted session working in workDir: the owner's (else
// the server's defaults: on macOS, commands reach trustd so that gh can verify GitHub's certificates), required, without
// the owner's extra hosts and writable paths, its own cache directory writable instead (restrictedCacheEnv), and its
// commands kept from reading the node's secrets (restrictedDenyRead).
func (n *Node) restrictedSandboxConfig(workDir string) *SandboxConfig {
	c := SandboxConfig{WeakerNetworkIsolation: runtime.GOOS == "darwin"}
	if n.cfg.Sandbox != nil {
		c = *n.cfg.Sandbox
	}
	c.Required = true
	c.AllowedDomains = nil
	c.AllowWrite = []string{restrictedCacheDir(workDir)}
	c.DenyRead = append(slices.Clone(c.DenyRead), n.restrictedDenyRead(workDir)...)
	slices.Sort(c.DenyRead)
	c.DenyRead = slices.Compact(c.DenyRead)
	return &c
}

// restrictedCacheDir holds a restricted session's package and build caches: in the Ticket's own .athena directory,
// which git ignores, in place of the owner's caches it may not write.
func restrictedCacheDir(workDir string) string { return filepath.Join(workDir, ".athena", "cache") }

// restrictedCaches are the cache variables a restricted session's commands get, each with its directory under
// restrictedCacheDir.
var restrictedCaches = []struct{ env, dir string }{
	{"GOCACHE", "go-build"}, {"GOMODCACHE", "gomod"}, {"npm_config_cache", "npm"}, {"PIP_CACHE_DIR", "pip"}, {"XDG_CACHE_HOME", "xdg"},
}

// restrictedCacheEnv is the environment of a restricted session's commands: its caches (restrictedCaches), and Go's
// module cache left writable, so that the Ticket's directory can be removed when it ends.
func restrictedCacheEnv(workDir string) []string {
	root := restrictedCacheDir(workDir)
	env := make([]string, 0, len(restrictedCaches)+1)
	for _, c := range restrictedCaches {
		env = append(env, c.env+"="+filepath.Join(root, c.dir))
	}
	return append(env, "GOFLAGS=-modcacherw")
}

// makeRestrictedCaches creates the cache directories of restrictedCacheEnv.
func makeRestrictedCaches(workDir string) error {
	root := restrictedCacheDir(workDir)
	for _, c := range restrictedCaches {
		if err := os.MkdirAll(filepath.Join(root, c.dir), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// restrictedDenyRead are the paths a restricted session's commands may not read, whatever the owner's sandbox says:
// the node's secrets (Config.Secrets: the GitHub App key), the owner's credentials the rules know (credRel), and
// everything in the state directory but what the session needs: its working directory, its git directories, the node
// socket, the CA bundle and the athena binary.
func (n *Node) restrictedDenyRead(workDir string) []string {
	out := slices.Clone(n.cfg.Secrets)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, rel := range credRel {
			out = append(out, filepath.Join(home, rel))
		}
	}
	if n.cfg.StateDir == "" {
		return out
	}
	keep := []string{workDir, n.cfg.SocketPath, n.cfg.AthenaBin}
	if n.cfg.Sandbox != nil {
		keep = append(keep, n.cfg.Sandbox.CAFile)
	}
	gitDir, common := gitDirs(workDir)
	keep = append(keep, gitDir, common)
	return append(out, denyAllBut(n.cfg.StateDir, keep)...)
}

// denyAllBut lists what to deny of root so that only the paths in keep (and what lies below them) stay readable: root
// itself when it holds none of them, else each of its entries that holds none, and, in an entry that holds one, the
// same again. A kept path above root keeps nothing of it. Paths compare with their symbolic links resolved; the list
// is in root's resolved spelling.
func denyAllBut(root string, keep []string) []string {
	root = resolvedPath(root)
	var resolved []string
	for _, k := range keep {
		if k != "" {
			resolved = append(resolved, resolvedPath(k))
		}
	}
	var walk func(dir string) []string
	walk = func(dir string) []string {
		if slices.Contains(resolved, dir) {
			return nil // kept, with everything below it
		}
		if !slices.ContainsFunc(resolved, func(k string) bool { return within(dir, k) }) {
			return []string{dir}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return []string{dir}
		}
		var out []string
		for _, e := range entries {
			out = append(out, walk(filepath.Join(dir, e.Name()))...)
		}
		return out
	}
	return walk(root)
}

// sandboxUnavailableError is a restricted session's launch on a machine that cannot sandbox it.
type sandboxUnavailableError struct{ cause string }

func (e *sandboxUnavailableError) Error() string { return restrictedSandboxSummary + e.cause }

// checkRestrictedSandbox is nil when the session may start: it is standard, or this machine can sandbox it.
func (n *Node) checkRestrictedSandbox(s *session) error {
	if !s.restricted() || n.canSandbox() {
		return nil
	}
	return &sandboxUnavailableError{cause: fmt.Sprintf("the sandbox runs on macOS and Linux, and this machine runs %s", runtime.GOOS)}
}
