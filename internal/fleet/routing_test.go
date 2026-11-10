package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/nodelink"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// executorEnv is newEnv with the fleet as the Engine's Executor.
func executorEnv(t *testing.T, mods ...func(*Config)) (*env, *Fleet) {
	t.Helper()
	e := newEnv(t)
	f := e.fleet(func(c *Config) {
		c.Engine = nil
		for _, m := range mods {
			m(c)
		}
	})
	e.eng = core.NewEngine(e.st.Repos(), f, nil, nil)
	f.SetEngine(e.eng)
	return e, f
}

// answering is a wire node that answers the server's calls and records them in order: diff with a Diff whose Base is
// "on-node", status with two slots and Claude Code ready, anything else with no value.
type answering struct {
	*wireNode
	mu      sync.Mutex
	methods []string
	args    []json.RawMessage
}

func dialAnswering(t *testing.T, f *Fleet, id string) *answering {
	t.Helper()
	h := hello(id)
	h.Caps.Runtimes = []*athenav1.RuntimeCap{{Name: core.RuntimeClaude, Ready: true}}
	a := &answering{wireNode: dialWire(t, f, id, h)}
	a.waitWelcome()
	go func() {
		for {
			select {
			case <-a.done:
				return
			case c := <-a.calls:
				a.mu.Lock()
				a.methods = append(a.methods, c.GetMethod())
				a.args = append(a.args, c.GetArgs())
				a.mu.Unlock()
				var v any
				switch c.GetMethod() {
				case nodelink.ServeDiff:
					v = node.Diff{Base: "on-node"}
				case nodelink.ServeStatus:
					v = node.Status{Slots: 2, Runtimes: []node.RuntimeStatus{{Name: core.RuntimeClaude, Ready: true, Tested: true}}}
				}
				var b []byte
				if v != nil {
					b, _ = json.Marshal(v)
				}
				_ = a.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_NodeResult{
					NodeResult: &athenav1.NodeResult{Id: c.GetId(), Value: b}}})
			}
		}
	}()
	return a
}

// commands are the calls the node got that are not status or interrupt reads, in order.
func (a *answering) commands() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for i, m := range a.methods {
		switch m {
		case nodelink.ServeSetPaused:
			var p nodelink.ArgsSetPaused
			_ = json.Unmarshal(a.args[i], &p)
			if p.Paused {
				m += ":true"
			} else {
				m += ":false"
			}
		case nodelink.ServeDropTokens:
			var d nodelink.ArgsDropTokens
			_ = json.Unmarshal(a.args[i], &d)
			m += ":" + d.Repo
		case nodelink.ServeStatus:
			continue
		}
		out = append(out, m)
	}
	return out
}

func (a *answering) got(m string) bool {
	for _, c := range a.commands() {
		if c == m {
			return true
		}
	}
	return false
}

// TestCallsRouteToTicketsNode: Interrupt and an artifact of a Ticket placed on a remote go to that node; once it is
// offline they fail with ErrNodeOffline, "<name> is offline".
func TestCallsRouteToTicketsNode(t *testing.T) {
	e, f := executorEnv(t)
	e.addNode(nodeID, "mini")
	tk := e.placed(nodeID)
	a := dialAnswering(t, f, nodeID)

	if err := f.Interrupt(tk.ID); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if !a.got(nodelink.ServeInterrupt) {
		t.Fatalf("the node got no interrupt: %v", a.commands())
	}
	d, err := f.TicketDiff(context.Background(), tk.ID)
	if err != nil || d.Base != "on-node" {
		t.Fatalf("diff = %+v, %v", d, err)
	}

	a.close()
	eventually(t, "offline", func() bool { return !f.Remote(nodeID).Online() })
	for name, err := range map[string]error{
		"interrupt": f.Interrupt(tk.ID),
		"diff": func() error {
			_, err := f.TicketDiff(context.Background(), tk.ID)
			return err
		}(),
		"subscribe": func() error {
			_, _, _, err := f.Subscribe(tk.ID, 0)
			return err
		}(),
	} {
		if !errors.Is(err, ErrNodeOffline) || !strings.Contains(err.Error(), "mini is offline") {
			t.Errorf("%s offline = %v", name, err)
		}
	}
}

// TestNodeStateBeforeWork: a remote that connects to a paused server gets the kill switch and the token drops it
// missed before any Start; the switch and drops reach a connected remote too.
func TestNodeStateBeforeWork(t *testing.T) {
	e, f := executorEnv(t)
	e.addNode(nodeID, "mini")
	f.SetPaused(true)
	f.DropTokens("o/held")
	tk, err := e.eng.SubmitOwnerTask("Fix the typo", core.TaskOptions{Node: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "it waits", func() bool { return f.WaitReason(tk.ID) == "Waiting for mini" })

	a := dialAnswering(t, f, nodeID)
	eventually(t, "the Start", func() bool { return a.got(nodelink.ServeStart) })
	cmds := a.commands()
	if len(cmds) < 3 || cmds[0] != nodelink.ServeSetPaused+":true" || cmds[1] != nodelink.ServeDropTokens+":o/held" {
		t.Fatalf("commands = %v, want the pause and the drop before anything", cmds)
	}

	f.SetPaused(false)
	f.DropTokens("x/y")
	eventually(t, "the resume and the drop reach the node", func() bool {
		return a.got(nodelink.ServeSetPaused+":false") && a.got(nodelink.ServeDropTokens+":x/y")
	})
}

// TestTokenDropsExpire: a drop older than a token's life is not sent to a node that connects.
func TestTokenDropsExpire(t *testing.T) {
	var skew atomic.Int64
	e, f := executorEnv(t, func(c *Config) { c.Now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) } })
	e.addNode(nodeID, "mini")
	f.DropTokens("o/old")
	skew.Store(int64(tokenLife + time.Minute))
	a := dialAnswering(t, f, nodeID)
	eventually(t, "the pause", func() bool { return a.got(nodelink.ServeSetPaused + ":false") })
	if a.got(nodelink.ServeDropTokens + ":o/old") {
		t.Fatal("an expired drop was sent")
	}
}
