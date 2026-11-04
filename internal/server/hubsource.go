package server

import (
	"encoding/json"
	"fmt"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/hub"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/store"
)

// recentTriggerThreads is how many of the newest Triggers name the recent Threads: Threads keep no time of their own.
const recentTriggerThreads = 200

// hubSource is what the hub reads: the Engine's reads, the store's for what the Engine does not list, and the
// Dispatcher's health and Policy.
type hubSource struct{ d *Dispatcher }

var _ hub.Source = hubSource{}

func newHubSource(d *Dispatcher) hubSource { return hubSource{d} }

func (s hubSource) Tickets(includeEnded bool) ([]core.Ticket, error) {
	return s.d.eng.Tickets(includeEnded)
}

func (s hubSource) Ticket(id string) (core.Ticket, core.Stage, error) { return s.d.eng.Ticket(id) }

func (s hubSource) PendingApprovals() ([]core.Approval, error) { return s.d.eng.PendingApprovals() }

func (s hubSource) AnsweredApprovals(limit int) ([]core.Approval, error) {
	return s.d.st.AnsweredApprovals(limit)
}

func (s hubSource) Approval(id string) (core.Approval, error) {
	var a *core.Approval
	err := s.d.st.Tx(func(tx *store.Tx) error {
		var err error
		a, err = tx.GetApproval(id)
		return err
	})
	if err != nil {
		return core.Approval{}, err
	}
	if a == nil {
		return core.Approval{}, userErrorf("approval %s not found", id)
	}
	return *a, nil
}

func (s hubSource) RecentTriggers(limit int) ([]core.Trigger, error) {
	return s.d.st.Triggers("", limit)
}

func (s hubSource) Trigger(id string) (core.Trigger, error) { return s.d.eng.Trigger(id) }

func (s hubSource) ActiveThreads() ([]core.Thread, error) { return s.d.eng.ActiveThreads() }

// RecentThreads returns the Threads of the newest Triggers, newest first and at most limit of them, then the active
// Threads those did not name.
func (s hubSource) RecentThreads(limit int) ([]core.Thread, error) {
	trigs, err := s.d.st.Triggers("", recentTriggerThreads)
	if err != nil {
		return nil, err
	}
	var keys []string
	seen := map[string]bool{}
	for _, t := range trigs {
		for _, k := range t.Threads {
			if !seen[k] && (limit <= 0 || len(keys) < limit) {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	var out []core.Thread
	err = s.d.st.Tx(func(tx *store.Tx) error {
		for _, k := range keys {
			th, err := tx.GetThread(k)
			if err != nil {
				return err
			}
			if th != nil {
				out = append(out, *th)
			}
		}
		active, err := tx.ActiveThreads()
		if err != nil {
			return err
		}
		for _, th := range active {
			if !seen[th.Key] {
				seen[th.Key] = true
				out = append(out, *th)
			}
		}
		return nil
	})
	return out, err
}

func (s hubSource) Thread(key string) (core.Thread, error) { return s.d.eng.Thread(key) }

func (s hubSource) Decisions(thread string, limit int) ([]core.Decision, error) {
	return s.d.eng.Decisions(thread, limit)
}

func (s hubSource) Decision(id string) (core.Decision, error) {
	var d *core.Decision
	err := s.d.st.Tx(func(tx *store.Tx) error {
		var err error
		d, err = tx.GetDecision(id)
		return err
	})
	if err != nil {
		return core.Decision{}, err
	}
	if d == nil {
		return core.Decision{}, userErrorf("decision %s not found", id)
	}
	return *d, nil
}

// Health is athena status's health view as items: each Source, the router, and the effects (one item per failing
// kind, else one healthy "effects"). Every part of it is in memory or the local store: no network call.
func (s hubSource) Health() []hub.HealthItem {
	h := s.d.health()
	var out []hub.HealthItem
	for _, src := range h.Sources {
		out = append(out, hub.HealthItem{Name: "source:" + src.Name, OK: src.Error == "", Message: src.Error})
	}
	r := hub.HealthItem{Name: "router", OK: h.Router.Error == "", Message: h.Router.State}
	if h.Router.Error != "" {
		r.Message += ": " + h.Router.Error
	}
	out = append(out, r)
	if len(h.Effects) == 0 {
		out = append(out, hub.HealthItem{Name: "effects", OK: true})
	}
	for kind, msg := range h.Effects {
		out = append(out, hub.HealthItem{Name: "effects:" + kind, Message: msg})
	}
	return out
}

// Policy reads the policy store, not the cache: the hub reads on the commit, before the cache takes the new revision.
func (s hubSource) Policy() (int, string, string, error) {
	cur, rev, err := s.d.policy.saved()
	if err != nil {
		return 0, "", "", err
	}
	raw, err := json.Marshal(cur)
	if err != nil {
		return 0, "", "", fmt.Errorf("policy: encode settings: %w", err)
	}
	return rev, policy.Version(cur), string(raw), nil
}

func (s hubSource) NodeStatus() node.Status {
	if s.d.node == nil {
		return node.Status{}
	}
	return s.d.node.Status()
}
