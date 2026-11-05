package node

import (
	"fmt"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/runtime/claude"
	"github.com/danielpina1/athena/internal/runtime/rt"
)

// roleWorker is the role of a Ticket's worker session.
const roleWorker = "worker"

// defaultRuntimes are the runtimes of a Config that names none: Claude Code alone.
func defaultRuntimes(cfg Config) map[string]rt.Runtime {
	return map[string]rt.Runtime{runtimeClaude: claude.Runtime{Binary: cfg.ClaudeBinary}}
}

// runtimeFor returns the runtime a Ticket's sessions run on: the one its Execution names, Claude Code when it names
// none. A runtime this node does not have is an error, never another runtime in its place.
func (n *Node) runtimeFor(t core.Ticket) (rt.Runtime, error) {
	name := runtimeClaude
	if t.Execution != nil && t.Execution.Runtime != "" {
		name = t.Execution.Runtime
	}
	r, ok := n.cfg.Runtimes[name]
	if !ok {
		return nil, fmt.Errorf("runtime %q is not available on this node", name)
	}
	return r, nil
}

// writableRoots are the directories besides its working directory a session must write to: the repository's git
// directory, for a worktree whose commits land there (gitDirs).
func writableRoots(workDir string) []string {
	if _, common := gitDirs(workDir); common != "" {
		return []string{common}
	}
	return nil
}
