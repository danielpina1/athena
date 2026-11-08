package node

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"

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
	// Trust is the trust class of the Trigger's sender.
	Trust(t core.Trigger) string
}

// The restricted tool policy's words to the model and the owner.
const (
	// restrictedEscalateReason is the reason of what a restricted session's rules would escalate.
	restrictedEscalateReason = "Work started by someone outside your allow list cannot ask for this; the owner can rerun it as their own task."
	// restrictedReadReason is the reason a restricted session's file tools may not read a path.
	restrictedReadReason = "Work started by someone outside your allow list reads only its own working directory and the system's tools."
	// restrictedWebReason is the reason a restricted session's web tools are denied.
	restrictedWebReason = "Work started by someone outside your allow list has no web access beyond GitHub."
	// restrictedSandboxSummary heads the Failed report of a restricted session that cannot be sandboxed.
	restrictedSandboxSummary = "This Ticket came from someone outside your allow list, and it runs only inside the sandbox, which this machine cannot start: "
	// restrictedFollowReason is why a restricted session cannot follow other Threads.
	restrictedFollowReason = "work started by someone outside your allow list cannot follow other threads"
	// restrictedRuntimeSummary heads the Failed report of a restricted launch where Claude Code cannot run.
	restrictedRuntimeSummary = "This Ticket came from someone outside your allow list, and such work runs only on Claude Code, which is not available on this machine: "
)

// trustOf is the trust class the worker is told of a Trigger's text: the Policy's, or "" (no Policy: nothing is marked,
// as without a Policy every tool policy is standard).
func (n *Node) trustOf(t core.Trigger) string {
	if n.cfg.Policy == nil {
		return ""
	}
	return n.cfg.Policy.Trust(t)
}

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
		out.AllowRead = slices.Clone(sb.AllowRead)
		out.UnixSockets = slices.Clone(sb.UnixSockets)
	}
	out.Required = true
	out.AllowedDomains = slices.Clone(GitHubDomains)
	return &out
}

// restrictedSandboxConfig is the sandbox configuration of a restricted session working in workDir on the operating
// system goos: the owner's (else the server's defaults: on macOS, commands reach trustd so that gh can verify GitHub's
// certificates), required, without the owner's extra hosts and writable paths, its own cache directory writable instead
// (restrictedCacheEnv), and its commands kept to what the session needs (restrictedReads).
func (n *Node) restrictedSandboxConfig(goos, workDir string) *SandboxConfig {
	c := SandboxConfig{WeakerNetworkIsolation: goos == "darwin"}
	if n.cfg.Sandbox != nil {
		c = *n.cfg.Sandbox
	}
	c.Required = true
	c.AllowedDomains = nil
	c.AllowWrite = []string{restrictedCacheDir(workDir), restrictedTempDir(workDir)}
	deny, allow := n.restrictedReads(goos, workDir)
	c.DenyRead = append(slices.Clone(c.DenyRead), deny...)
	slices.Sort(c.DenyRead)
	c.DenyRead = slices.Compact(c.DenyRead)
	c.AllowRead = allow
	return &c
}

// restrictedTempParent holds the temporary roots of restricted sessions: one directory, so that denying it covers the
// roots of sessions that start later. It lies in /tmp: Claude Code stops using a root too long for a Unix socket.
func restrictedTempParent(uid int) string { return fmt.Sprintf("/tmp/athena-%d", uid) }

// restrictedTempDir is the temporary root of a restricted session working in workDir (CLAUDE_CODE_TMPDIR). Claude Code
// gives a sandboxed command the whole per-user directory in its root as $TMPDIR, so the session needs a root of its own
// for that directory to be denied (claudeTempRoots) without taking its commands' temporary files with it.
func restrictedTempDir(workDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(workDir)))
	return filepath.Join(restrictedTempParent(os.Getuid()), hex.EncodeToString(sum[:4]))
}

// claudeTempRoots are where Claude Code keeps every session's temporary files of this user, a Ticket's background task
// output among them: the default per-user directory, the older shared one, and the per-user directory in the owner's
// CLAUDE_CODE_TMPDIR.
func claudeTempRoots(uid int) []string {
	perUser := fmt.Sprintf("claude-%d", uid)
	roots := []string{filepath.Join("/tmp", perUser), "/tmp/claude"}
	if d := os.Getenv("CLAUDE_CODE_TMPDIR"); filepath.IsAbs(d) {
		roots = append(roots, filepath.Join(d, perUser))
	}
	return roots
}

// makeRestrictedTemp creates the session's temporary root, private to the node's user. It refuses a parent in /tmp it
// did not make: anyone can create names there, and a session's temporary files must not be readable to another user.
func makeRestrictedTemp(workDir string) error {
	parent := restrictedTempParent(os.Getuid())
	if err := os.Mkdir(parent, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	st, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !st.IsDir() || st.Mode().Perm() != 0o700 || !ok || int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not a private directory of this user", parent)
	}
	return os.MkdirAll(restrictedTempDir(workDir), 0o700)
}

// restrictedCacheDir holds a restricted session's package and build caches: in the Ticket's own .athena directory,
// which git ignores, in place of the owner's caches it may not write.
func restrictedCacheDir(workDir string) string { return filepath.Join(workDir, ".athena", "cache") }

// restrictedCaches are the cache variables a restricted session's commands get, each with its directory under
// restrictedCacheDir.
var restrictedCaches = []struct{ env, dir string }{
	{"GOCACHE", "go-build"}, {"GOMODCACHE", "gomod"}, {"npm_config_cache", "npm"}, {"PIP_CACHE_DIR", "pip"}, {"XDG_CACHE_HOME", "xdg"},
	{"XDG_CONFIG_HOME", "config"},
}

// restrictedCacheEnv is the environment of a restricted session's commands: its caches and configuration directory
// (restrictedCaches), Go's module cache left writable, so that the Ticket's directory can be removed when it ends, and
// no global git configuration: the owner's home is unreadable to the session, and git stops when it cannot read
// ~/.gitconfig.
func restrictedCacheEnv(workDir string) []string {
	root := restrictedCacheDir(workDir)
	env := make([]string, 0, len(restrictedCaches)+2)
	for _, c := range restrictedCaches {
		env = append(env, c.env+"="+filepath.Join(root, c.dir))
	}
	return append(env, "GOFLAGS=-modcacherw", "GIT_CONFIG_GLOBAL="+os.DevNull)
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

// restrictedReads are what a restricted session's commands may not read, whatever the owner's sandbox says, and what
// they may read again inside that (Claude Code's allowRead, which takes precedence over denyRead):
//   - denied: the node's secrets (Config.Secrets: the GitHub App key) and the owner's credentials the rules know
//     (credRel), each in its configured and its resolved spelling; the owner's home directory, Athena's state directory,
//     the work root, the transcripts, Claude Code's session transcripts and temporary roots (claudeTempRoots) and those
//     of the restricted sessions, each as a whole, so that what appears there later (another Ticket's directory, output
//     or transcript) is denied too; and on Linux the daemons' sockets (daemonSockets).
//   - read again: what the session needs: its working directory and git directories, its own temporary root, the node
//     socket, the gh shim, the athena binary, the CA bundle, Claude Code's shell snapshots and install, and the toolchains on PATH under the
//     home directory (homeToolchains). Nothing that holds or lies in a secret or credential is read again.
func (n *Node) restrictedReads(goos, workDir string) (deny, allow []string) {
	home, _ := os.UserHomeDir()
	var guarded []string
	for _, k := range n.cfg.Secrets {
		guarded = append(guarded, spellings(k)...)
	}
	if home != "" {
		for _, rel := range credRel {
			guarded = append(guarded, spellings(filepath.Join(home, filepath.FromSlash(rel)))...)
		}
	}
	deny = slices.Clone(guarded)
	wholes := append([]string{home, n.cfg.StateDir, n.cfg.WorkRoot, restrictedTempParent(os.Getuid())}, claudeTempRoots(os.Getuid())...)
	if home != "" {
		wholes = append(wholes, filepath.Join(home, ".claude", "projects"))
	}
	if n.cfg.StateDir != "" {
		wholes = append(wholes, filepath.Dir(TranscriptPath(n.cfg.StateDir, "x")))
	}
	for _, w := range wholes {
		if w != "" {
			deny = append(deny, spellings(w)...)
		}
	}
	deny = append(deny, daemonSockets(goos, os.Getenv("XDG_RUNTIME_DIR"), n.cfg.SocketPath)...)

	gitDir, common := gitDirs(workDir)
	need := []string{workDir, gitDir, common, n.cfg.SocketPath, n.cfg.AthenaBin, restrictedTempDir(workDir)}
	if n.cfg.StateDir != "" {
		need = append(need, filepath.Join(n.cfg.StateDir, ShimsDir))
	}
	if n.cfg.Sandbox != nil {
		need = append(need, n.cfg.Sandbox.CAFile)
	}
	if home != "" {
		need = append(need, filepath.Join(home, ".claude", "shell-snapshots"))
	}
	if bin := n.cfg.ClaudeBinary; bin != "" {
		if p, err := exec.LookPath(bin); err == nil {
			need = append(need, filepath.Dir(resolvedPath(p)))
		}
	}
	need = append(need, homeToolchains(home, os.Getenv("PATH"))...)
	for _, p := range need {
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		for _, v := range spellings(p) {
			if !slices.ContainsFunc(guarded, func(g string) bool { return within(g, v) || within(v, g) }) {
				allow = append(allow, v)
			}
		}
	}
	slices.Sort(deny)
	slices.Sort(allow)
	return slices.Compact(deny), slices.Compact(allow)
}

// spellings are p cleaned and, when different, with its symbolic links resolved: a sandbox may match either.
func spellings(p string) []string {
	c, r := filepath.Clean(p), resolvedPath(p)
	if r == c {
		return []string{c}
	}
	return []string{c, r}
}

// homeToolchains are the toolchains a restricted session's commands may run from the owner's home directory: each
// directory on path that lies below home, and, for one named bin, its parent too when that is a toolchain's own
// install two levels or more below home (~/.nvm/versions/node/v22, not ~/.local), whose libraries the binaries need.
func homeToolchains(home, path string) []string {
	if home == "" {
		return nil
	}
	home = filepath.Clean(home)
	var out []string
	for _, e := range filepath.SplitList(path) {
		if !filepath.IsAbs(e) {
			continue
		}
		e = filepath.Clean(e)
		if e == home || !within(home, e) {
			continue
		}
		out = append(out, e)
		if parent := filepath.Dir(e); filepath.Base(e) == "bin" && strings.Count(relWithin(home, parent), string(filepath.Separator)) >= 1 {
			out = append(out, parent)
		}
	}
	return out
}

// daemonSockets are the paths that hold the sockets of the machine's daemons (Docker, containerd, Podman, and the
// user's runtime directory, but for the node socket in it), on Linux, where Claude Code's sandbox cannot allow one
// socket by path and lets commands reach every one: denied, the sandbox masks them. Elsewhere there are none (macOS
// allows the node socket alone).
func daemonSockets(goos, runtimeDir, nodeSocket string) []string {
	if goos != "linux" {
		return nil
	}
	out := []string{"/var/run/docker.sock", "/run/docker.sock", "/run/containerd", "/run/podman"}
	if runtimeDir != "" && filepath.IsAbs(runtimeDir) {
		out = append(out, denyAllBut(runtimeDir, []string{nodeSocket})...)
	}
	return out
}

// restrictedSystemRoots are the system's directories a restricted session's file tools may read: its programs,
// libraries, headers and configuration, and the developer tools'.
var restrictedSystemRoots = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/opt", "/etc", "/nix",
	"/snap", "/System", "/Library/Developer", "/Applications/Xcode.app"}

// restrictedReadRoots are, besides its working directory, what a restricted session's file tools may read
// (RuleContext.ReadRoots): the system's roots and the toolchains under the home directory.
func restrictedReadRoots() []string {
	home, _ := os.UserHomeDir()
	return append(slices.Clone(restrictedSystemRoots), homeToolchains(home, os.Getenv("PATH"))...)
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
