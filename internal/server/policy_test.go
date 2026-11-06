package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/store"
)

// policyPair is one change told to the Engine: the versions it went from and to.
type policyPair struct{ from, to string }

// newPolicyControl is a control with only what policy show/set need, over a fresh store; changes records each change
// before the Engine hears of it, and fail (when it returns an error) refuses it.
func newPolicyControl(t *testing.T, st *store.Store) (*control, *[]policyPair, *func() error) {
	t.Helper()
	pc, err := newPolicyCache(st, nil, allScope{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	eng := core.NewEngineWith(st.Repos(), nopExec{}, core.Options{Policy: pc.policy, Scope: allScope{}})
	var mu sync.Mutex
	pairs := &[]policyPair{}
	fail := new(func() error)
	c := &control{eng: eng, st: st, policy: pc, log: discardLogger()}
	c.onPolicyChange = func(from, to policy.Settings) error {
		mu.Lock()
		f := *fail
		mu.Unlock()
		if f != nil {
			if err := f(); err != nil {
				return err
			}
		}
		time.Sleep(2 * time.Millisecond) // a slow Engine: another set must not slip in between
		mu.Lock()
		defer mu.Unlock()
		*pairs = append(*pairs, policyPair{policy.Version(from), policy.Version(to)})
		return nil
	}
	return c, pairs, fail
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func settingsJSON(t *testing.T, allowed ...string) (json.RawMessage, string) {
	s := policy.Defaults(allowed)
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return raw, policy.Version(s)
}

// R30 M1: concurrent sets are told one after another, each from the revision it replaced to the one it saved.
func TestPolicySetConcurrentPairs(t *testing.T) {
	st := openStore(t)
	c, pairs, _ := newPolicyControl(t, st)
	start := policy.Version(policy.Defaults(nil))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		raw, _ := settingsJSON(t, fmt.Sprintf("user%d", i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				_, err := c.policySet(raw)
				var ue *core.UserError
				if errors.As(err, &ue) && ue.Msg == staleSettings {
					continue
				}
				errs <- err
				return
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(*pairs) != 8 {
		t.Fatalf("%d changes told, want 8", len(*pairs))
	}
	prev := start
	for i, p := range *pairs {
		if p.from != prev {
			t.Fatalf("change %d goes from %s, want %s (the revision before it): %+v", i, p.from, prev, *pairs)
		}
		prev = p.to
	}
	if cur, rev := c.policy.current(); rev != 8 || policy.Version(cur) != prev {
		t.Fatalf("current = rev %d version %s, want rev 8 version %s", rev, policy.Version(cur), prev)
	}
}

// R30 M1: a change saved but not told (PolicyChanged failed) is told before the next set, and at startup.
func TestPolicyChangeRetriedAfterFailure(t *testing.T) {
	st := openStore(t)
	c, pairs, fail := newPolicyControl(t, st)
	start := policy.Version(policy.Defaults(nil))
	*fail = func() error { return errors.New("engine unavailable") }
	rawA, verA := settingsJSON(t, "alice")
	if _, err := c.policySet(rawA); err == nil {
		t.Fatal("set told nobody and reported success")
	}
	if cur, rev := c.policy.current(); rev != 1 || policy.Version(cur) != verA {
		t.Fatalf("after the failed tell: rev %d, want the saved revision 1", rev)
	}
	*fail = nil
	rawB, verB := settingsJSON(t, "bob")
	if _, err := c.policySet(rawB); err != nil {
		t.Fatal(err)
	}
	want := []policyPair{{start, verA}, {verA, verB}}
	if fmt.Sprint(*pairs) != fmt.Sprint(want) {
		t.Fatalf("told %v, want %v (the pending change first)", *pairs, want)
	}

	// At startup: a new cache over the same store tells the pending change once.
	*fail = func() error { return errors.New("engine unavailable") }
	rawC, verC := settingsJSON(t, "carol")
	if _, err := c.policySet(rawC); err == nil {
		t.Fatal("set told nobody and reported success")
	}
	c2, pairs2, _ := newPolicyControl(t, st)
	if err := c2.resumePolicyChange(); err != nil {
		t.Fatal(err)
	}
	if err := c2.resumePolicyChange(); err != nil {
		t.Fatal(err)
	}
	if want := []policyPair{{verB, verC}}; fmt.Sprint(*pairs2) != fmt.Sprint(want) {
		t.Fatalf("startup told %v, want %v once", *pairs2, want)
	}
}

// The Engine's pairing default is the saved settings' pairing: off until the owner turns review on, which owner tasks
// that name none then take.
func TestPairingDefaultFromSettings(t *testing.T) {
	st := openStore(t)
	c, _, _ := newPolicyControl(t, st)
	if d := c.policy.pairing(); d != (core.PairingDefaults{MaxRounds: 2}) {
		t.Fatalf("default pairing = %+v", d)
	}
	s := policy.Defaults(nil)
	s.Pairing = policy.Pairing{Review: policy.PairingOn, MaxRounds: 1}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.policySet(raw); err != nil {
		t.Fatal(err)
	}
	if d := c.policy.pairing(); d != (core.PairingDefaults{Mode: core.PairingReview, MaxRounds: 1}) {
		t.Fatalf("pairing after the save = %+v", d)
	}
	eng := core.NewEngineWith(st.Repos(), nopExec{}, core.Options{Pairing: c.policy.pairing})
	tk, err := eng.SubmitOwnerTask("Task", core.TaskOptions{Dir: "/w"})
	if err != nil || tk.Pairing != core.PairingReview {
		t.Fatalf("task pairing = %q %v", tk.Pairing, err)
	}
}

// The node's Policy reads the cache's current Policy at each call: a stranger's work is restricted by default, and a
// saved change applies to the next question, without a new adapter.
func TestNodePolicyReadsCurrentPolicy(t *testing.T) {
	st := openStore(t)
	pc, err := newPolicyCache(st, nil, allScope{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pc.awaitCheck() // the Engine has no Policy: the tool policies still hold
	np := nodePolicy{pc}
	if got := np.ToolPolicy(core.TrustStranger); got != policy.ToolRestricted {
		t.Fatalf("stranger = %q, want restricted", got)
	}
	if got := np.ToolPolicy(core.TrustOwner); got != policy.ToolStandard {
		t.Fatalf("owner = %q, want standard", got)
	}
	s, rev := pc.current()
	s.Trust = map[string]string{core.TrustCollaborator: policy.ToolRestricted, core.TrustStranger: policy.ToolStandard}
	s.Repos = map[string]policy.RepoSettings{"o/r": {Mode: policy.RepoShadow}}
	pc.set(s, rev+1)
	if np.ToolPolicy(core.TrustCollaborator) != policy.ToolRestricted || np.ToolPolicy(core.TrustStranger) != policy.ToolStandard {
		t.Fatalf("after the change: collaborator %q, stranger %q", np.ToolPolicy(core.TrustCollaborator), np.ToolPolicy(core.TrustStranger))
	}
	if !np.Shadow("o/r") || np.Shadow("o/other") {
		t.Fatal("shadow does not follow the current Policy")
	}
}
