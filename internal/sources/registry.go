// Package sources holds the Source interfaces and the Registry that routes a Thread key to its Source, after the
// reference (src/sources/kind.ts, registry.ts).
package sources

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

// Source is a system Triggers come from, such as GitHub: it receives them while Run runs and answers which Threads it
// still listens to.
type Source interface {
	// Name is the Source's name, the prefix of its Thread keys (<name>:<key>).
	Name() string
	// Listens reports whether the Source currently receives the Thread.
	Listens(thread string) bool
	// Run receives Triggers until ctx is done.
	Run(ctx context.Context) error
	// Health is nil while the Source works.
	Health() error
}

// SendResult is what a Channel did with a message.
type SendResult string

const (
	// Sent: the message went out.
	Sent SendResult = "sent"
	// Gone: the conversation can never receive it, such as a deleted issue or a locked conversation.
	Gone SendResult = "gone"
)

// SendRequest is a message for a Channel to send.
type SendRequest struct {
	// ReceiptID is the message's stable ID, to make the send idempotent.
	ReceiptID string
	// Attempt counts the sends started before this one (zero for the first). A send may fail after the remote service
	// received it, and the process may die after it did, so above zero the message may already be there.
	Attempt int
	// QueuedAt is when the message was queued: it cannot have been sent before.
	QueuedAt time.Time
	Text     string
}

// Channel is the optional part of a Source that writes to its conversations. Send returns an error for a failure that
// may pass, Gone for one that never will. A retry (Attempt above zero) must not duplicate the message.
type Channel interface {
	Send(ctx context.Context, ch core.ChannelRef, req SendRequest) (SendResult, error)
	// Link is the URL a person opens to see the Channel.
	Link(ch core.ChannelRef) string
}

// ThreadContext is the optional part of a Source that reads a Thread around a Trigger, for an assessment.
type ThreadContext interface {
	Context(ctx context.Context, thread, triggerID string) (ContextWindow, error)
}

// ContextWindow is a Thread as an assessor reads it: messages in full (cut, with Chars set to the original length)
// and the rest as stubs.
type ContextWindow struct {
	Messages []Msg
	Stubs    []Stub
}

// Msg is one message of a ContextWindow.
type Msg struct {
	ID, From, At, Text string
	// Chars is the length of Text before it was cut; zero when it was not.
	Chars int
}

// Stub is a message of a ContextWindow shown only by its start.
type Stub struct{ ID, From, Preview string }

// Registry holds the registered Sources by name. It is the Engine's core.SourceScope: a Thread key routes to the
// Source named by its prefix, and a prefix no Source has does not listen. It is safe for concurrent use.
type Registry struct {
	mu      sync.Mutex
	sources map[string]Source
	running map[string]bool
}

var _ core.SourceScope = (*Registry)(nil)

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{sources: map[string]Source{}, running: map[string]bool{}}
}

// Register adds a Source; registering a name twice, or an empty one, panics, as it is a wiring bug.
func (r *Registry) Register(s Source) {
	name := s.Name()
	r.mu.Lock()
	defer r.mu.Unlock()
	if name == "" || strings.Contains(name, ":") {
		panic("sources: invalid source name " + name)
	}
	if _, dup := r.sources[name]; dup {
		panic("sources: source registered twice: " + name)
	}
	r.sources[name] = s
}

// Source returns the Source registered under name, or nil.
func (r *Registry) Source(name string) Source {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sources[name]
}

// Listens reports whether the Source named by the Thread key's prefix listens to it; false for an unknown prefix.
func (r *Registry) Listens(thread string) bool {
	name, _, ok := strings.Cut(thread, ":")
	if !ok {
		return false
	}
	s := r.Source(name)
	return s != nil && s.Listens(thread)
}

// Start runs every registered Source's Run in its own goroutine until ctx is done. A Source counts as running from
// Start until its Run returns.
func (r *Registry) Start(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, s := range r.sources {
		if r.running[name] {
			continue
		}
		r.running[name] = true
		go func() {
			_ = s.Run(ctx) // a Source reports its own failures through Health
			r.mu.Lock()
			r.running[name] = false
			r.mu.Unlock()
		}()
	}
}

// Running reports whether the named Source is registered and its Run has not returned.
func (r *Registry) Running(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[name]
}
