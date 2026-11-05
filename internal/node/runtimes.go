package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/runtime/claude"
	"github.com/danielpina1/athena/internal/runtime/codex"
	"github.com/danielpina1/athena/internal/runtime/rt"
)

// roleWorker is the role of a Ticket's worker session.
const roleWorker = "worker"

// DefaultRuntimes are the runtimes of a Config that names none: Claude Code alone, run from cfg.ClaudeBinary. A server
// that has more (Codex) adds them to these.
func DefaultRuntimes(cfg Config) map[string]rt.Runtime {
	return map[string]rt.Runtime{runtimeClaude: claude.Runtime{Binary: cfg.ClaudeBinary}}
}

// runtimeName is the runtime a Ticket's sessions run on: the one its Execution names; Claude Code when it names none,
// and for auto until the runtime chooser picks one.
func runtimeName(t core.Ticket) string {
	name := runtimeClaude
	if t.Execution != nil && t.Execution.Runtime != "" && t.Execution.Runtime != core.RuntimeAuto {
		name = t.Execution.Runtime
	}
	return name
}

// runtimeFor returns the runtime a Ticket's sessions run on (runtimeName). A runtime this node does not have is an
// error, never another runtime in its place.
func (n *Node) runtimeFor(t core.Ticket) (rt.Runtime, error) {
	name := runtimeName(t)
	r, ok := n.cfg.Runtimes[name]
	if !ok {
		return nil, fmt.Errorf("runtime %q is not available on this node", name)
	}
	return r, nil
}

// readiness is why a runtime cannot run the Ticket's sessions now ("" when it can). Claude Code is taken as ready (its
// sessions fail on their own when it is not, and checking costs a process per launch); another runtime is asked
// (Codex caches its answer), and a runtime that is not ready refuses the session with its Why rather than run one
// Athena's hooks do not control.
func readiness(ctx context.Context, r rt.Runtime) string {
	if r.Name() == runtimeClaude {
		return ""
	}
	info := r.Check(ctx)
	if info.Ready {
		return ""
	}
	why := info.Why
	if why == "" {
		why = "it is not ready"
	}
	return r.Name() + " cannot run this ticket: " + why
}

// invalidator is a runtime whose readiness answer is cached (Codex): a session start that found it uncontrolled drops
// the answer, so the next launch checks (and re-trusts its hooks) again.
type invalidator interface{ Invalidate() }

// uncontrolled reports whether a session start failed because the runtime would run it outside Athena's control (a
// hook or a setting that is not Athena's): its Why is in the error.
func uncontrolled(err error) bool {
	var hu codex.ErrHooksUncontrolled
	var cu codex.ErrConfigUncontrolled
	return errors.As(err, &hu) || errors.As(err, &cu)
}
