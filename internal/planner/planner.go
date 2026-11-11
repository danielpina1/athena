// Package planner turns one request into a Breakdown: the sub-tasks a lead Ticket splits into, with their deps, for
// the owner to approve. A Strategy is chosen by name in the server's configuration.
package planner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/danielpina1/athena/internal/core"
)

// Request is what a Strategy plans: the lead Ticket's task, where it runs, and what its sub-tasks may use.
type Request struct {
	TicketID, Title, Text, Repo, Dir string
	// MaxTasks is the most sub-tasks the split may have.
	MaxTasks int
	// Runtimes and Nodes are the hints a sub-task may name.
	Runtimes, Nodes []string
}

// Breakdown is a planned split.
type Breakdown = core.Breakdown

// Strategy plans a split.
type Strategy interface {
	Name() string
	Plan(ctx context.Context, r Request) (Breakdown, error)
}

// Config chooses and configures the Strategy. Zero fields take the defaults.
type Config struct {
	// Strategy is a strategy's name (default "llm").
	Strategy string
	// Model is the llm strategy's model (default "haiku").
	Model string
	// Binary is the `claude` binary the llm strategy runs (default "claude").
	Binary string
	// StateDir is the server's state directory: the planner's transcript and working directory live under it.
	StateDir string
}

// Defaults of Config.
const (
	DefaultStrategy = "llm"
	DefaultModel    = "haiku"
	DefaultBinary   = "claude"
)

// ErrOff is what the "off" Strategy's Plan returns.
var ErrOff = errors.New("planning is off in this server's configuration")

// strategies builds each known Strategy from a Config with its defaults filled.
var strategies = map[string]func(Config) Strategy{
	"llm": func(c Config) Strategy {
		return LLM{C: &Claude{Binary: c.Binary, Model: c.Model, Dir: filepath.Join(c.StateDir, "planner"), StateDir: c.StateDir}}
	},
	"off": func(Config) Strategy { return off{} },
}

// New returns the Strategy cfg names. An unknown name is an error that lists the known ones.
func New(cfg Config) (Strategy, error) {
	if cfg.Strategy == "" {
		cfg.Strategy = DefaultStrategy
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.Binary == "" {
		cfg.Binary = DefaultBinary
	}
	build, ok := strategies[cfg.Strategy]
	if !ok {
		known := make([]string, 0, len(strategies))
		for name := range strategies {
			known = append(known, name)
		}
		slices.Sort(known)
		return nil, fmt.Errorf("planner: unknown strategy %q (known: %s)", cfg.Strategy, strings.Join(known, ", "))
	}
	return build(cfg), nil
}

// off is the Strategy of a server that does not plan.
type off struct{}

func (off) Name() string { return "off" }

func (off) Plan(context.Context, Request) (Breakdown, error) { return Breakdown{}, ErrOff }
