package node

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/policy"
)

// slowPolicy is a Policy whose ToolPolicy blocks, once armed, until released: a remote server that is slow to answer.
type slowPolicy struct {
	armed   atomic.Bool
	waiting atomic.Int32
	release chan struct{}
}

func (p *slowPolicy) ToolPolicy(string) string {
	if p.armed.Load() {
		p.waiting.Add(1)
		<-p.release
	}
	return policy.ToolStandard
}
func (*slowPolicy) Allows(string, string, string) bool { return false }
func (*slowPolicy) Shadow(string) bool                 { return false }
func (*slowPolicy) Trust(core.Trigger) string          { return core.TrustOwner }

// A launch waiting on a slow Policy holds no lock the node's loop needs: Status and another Ticket's Stop go on.
func TestSlowToolPolicyBlocksNoOtherSession(t *testing.T) {
	p := &slowPolicy{release: make(chan struct{})}
	h := newHarness(t, func(c *Config) { c.Policy = p })
	released := false
	release := func() {
		if !released {
			released = true
			close(p.release)
		}
	}
	t.Cleanup(release)

	first := h.submit("first task", t.TempDir())
	tkA, a := h.running(first.ID)
	p.armed.Store(true)
	h.submit("second task", t.TempDir())
	h.eventually("the second launch asks the Policy", func() bool { return p.waiting.Load() > 0 })

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.n.Status()
		h.n.Stop(tkA, a, "test")
		for h.n.liveSession(first.ID) != nil {
			time.Sleep(5 * time.Millisecond)
		}
		_ = h.n.Status()
	}()
	select {
	case <-done:
	case <-time.After(waitTimeout):
		t.Fatal("Status or Stop of another Ticket waited for the slow Policy")
	}
	release()
}
