package node

import (
	"fmt"
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

// restrictedSandboxConfig is the sandbox of a restricted session on a node whose owner turned the sandbox off: the
// server's defaults (on macOS, commands reach trustd so that gh can verify GitHub's certificates).
func restrictedSandboxConfig() *SandboxConfig {
	return &SandboxConfig{Required: true, WeakerNetworkIsolation: runtime.GOOS == "darwin"}
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
