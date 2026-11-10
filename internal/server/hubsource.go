package server

import (
	"cmp"
	"encoding/json"
	"fmt"
	goruntime "runtime"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/fleet"
	"github.com/danielpina1/athena/internal/hub"
	"github.com/danielpina1/athena/internal/pause"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/store"
	"github.com/danielpina1/athena/internal/web"
)

// recentTriggerThreads is how many of the newest Triggers name the recent Threads: Threads keep no time of their own.
const recentTriggerThreads = 200

const (
	// repoTickets is how many of the newest Tickets name the known repositories and folders.
	repoTickets = 200
	// maxRepos bounds the known repositories the dashboard is sent.
	maxRepos = 50
)

// hubSource is what the hub reads: the Engine's reads, the store's for what the Engine does not list, and the
// Dispatcher's health and Policy. tls, when set, adds the web certificate to the health.
type hubSource struct {
	d   *Dispatcher
	tls *web.TLSManager
}

var _ hub.Source = hubSource{}

func newHubSource(d *Dispatcher) hubSource { return hubSource{d: d} }

// Tickets returns every open Ticket and the endedLimit most recently ended; the store applies the limit.
func (s hubSource) Tickets(endedLimit int) ([]core.Ticket, error) {
	open, err := s.d.eng.Tickets(false)
	if err != nil {
		return nil, err
	}
	ended, err := s.d.st.RecentlyEnded(endedLimit)
	if err != nil {
		return nil, err
	}
	// The two reads are two transactions: a Ticket that ended between them is in both lists.
	seen := make(map[string]bool, len(open))
	for _, t := range open {
		seen[t.ID] = true
	}
	for _, t := range ended {
		if !seen[t.ID] {
			open = append(open, t)
		}
	}
	return open, nil
}

func (s hubSource) Ticket(id string) (core.Ticket, core.Stage, error) { return s.d.eng.Ticket(id) }

func (s hubSource) PendingApprovals() ([]core.Approval, error) { return s.d.eng.PendingApprovals() }

func (s hubSource) AnsweredApprovals(limit int) ([]core.Approval, error) {
	return s.d.st.AnsweredApprovals(limit)
}

func (s hubSource) TicketAnsweredApprovals(ticketID string) ([]core.Approval, error) {
	return s.d.st.TicketAnsweredApprovals(ticketID)
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
	if s.tls != nil {
		ok, msg, _ := s.tls.Health()
		out = append(out, hub.HealthItem{Name: "tls", OK: ok, Message: msg})
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

// Nodes lists the nodes: the Fleet's (the server's own first, then the others by name, revoked ones too), or, with no
// Fleet, the embedded node alone.
func (s hubSource) Nodes() []hub.NodeInfo {
	if s.d.fleet == nil {
		if s.d.node == nil {
			return []hub.NodeInfo{{ID: fleet.LocalID, Local: true, State: fleet.StateOnline, OS: goruntime.GOOS, Arch: goruntime.GOARCH}}
		}
		st := s.d.node.Status()
		return []hub.NodeInfo{{ID: fleet.LocalID, Name: st.Name, Local: true, State: fleet.StateOnline,
			OS: goruntime.GOOS, Arch: goruntime.GOARCH, Status: st}}
	}
	views := s.d.fleet.Nodes()
	out := make([]hub.NodeInfo, 0, len(views))
	for _, v := range views {
		i := hub.NodeInfo{ID: v.ID, Name: v.Name, Local: v.Local, State: v.State, LastSeen: v.LastSeen, Version: v.Version,
			Status: v.Status, Caps: v.Caps}
		if v.Local {
			i.OS, i.Arch, i.LastSeen = goruntime.GOOS, goruntime.GOARCH, time.Time{}
		}
		out = append(out, i)
	}
	// The local node first, then by name (the Fleet says so; the order is the dashboard's, so it is kept here).
	slices.SortStableFunc(out, func(a, b hub.NodeInfo) int {
		switch {
		case a.Local != b.Local:
			if a.Local {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// Placement is the node the Ticket's latest Assignment runs on and whether the Fleet holds it waiting for a node. A
// pinned Ticket not placed yet shows its pin; an unpinned pending Assignment not placed yet shows no node (empty id and
// name); anything else with no node recorded ran before there were nodes, on the server's own.
func (s hubSource) Placement(t *core.Ticket) (string, string, string) {
	waiting := ""
	if s.d.fleet != nil {
		waiting = s.d.fleet.WaitReason(t.ID)
	}
	id := fleet.LocalID
	a := t.LatestAssignment()
	switch {
	case a != nil && a.Node != "":
		id = a.Node
	case t.Execution != nil && t.Execution.Node != "":
		id = t.Execution.Node // pinned and not placed yet
	case a != nil && a.State == core.AssignmentPending:
		return "", "", waiting
	}
	return id, s.d.nodeName(id), waiting
}

// GitFolder reports whether dir has a .git entry, as the placeFacts pass last found it (see Repos).
func (s hubSource) GitFolder(dir string) bool { return s.d.facts.lookup(dir).git }

// Trust classifies a Trigger's sender under the current Policy; there is none until settings are saved or checked.
func (s hubSource) Trust(t core.Trigger) string {
	if p := s.d.policy.policy(); p != nil {
		return p.Trust(t)
	}
	return ""
}

// Repos lists where a task can be placed: the configured GitHub repositories and the repository or folder of each of
// the newest Tickets, the last used first. A configured repository no Ticket used goes after them, by name; together
// they are at most maxRepos. Then come the local git repositories the last scan found under the localRepoRoots and
// not listed already (under any spelling through symbolic links), by name, at most maxLocalRepos (kind git, never
// used). A folder is a git repository when it has a .git entry. Repos runs on the hub's loop and reads the store
// only: whether a used folder has a .git entry, and where it resolves, are what the placeFacts pass found off the
// loop (a folder it has not probed yet is a plain folder until it has).
func (s hubSource) Repos() ([]*athenav1.RepoView, error) {
	places, err := s.d.st.RecentPlacements(repoTickets)
	if err != nil {
		return nil, err
	}
	type entry struct {
		kind string
		at   time.Time
	}
	known := map[string]entry{}
	use := func(name, kind string, at time.Time) {
		if e, ok := known[name]; !ok || at.After(e.at) {
			known[name] = entry{kind, at}
		}
	}
	for _, p := range places {
		if p.Repo != "" {
			use(p.Repo, "github", p.At)
		}
		if p.Dir != "" {
			kind := "dir"
			if s.d.facts.lookup(p.Dir).git {
				kind = "git"
			}
			use(p.Dir, kind, p.At)
		}
	}
	if s.d.gh != nil && s.d.gh.cfg != nil {
		for _, r := range s.d.gh.cfg.Repos {
			if _, ok := known[r]; !ok {
				known[r] = entry{kind: "github"}
			}
		}
	}
	names := make([]string, 0, len(known))
	for name := range known {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int {
		// The used ones first, the newest first; then the never used. Ties go by name.
		ea, eb := known[a], known[b]
		if ea.at.IsZero() != eb.at.IsZero() {
			if ea.at.IsZero() {
				return 1
			}
			return -1
		}
		if c := eb.at.Compare(ea.at); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	if len(names) > maxRepos {
		names = names[:maxRepos]
	}
	out := make([]*athenav1.RepoView, len(names), len(names)+maxLocalRepos)
	listed := make(map[string]bool, len(names))
	for i, name := range names {
		e := known[name]
		listed[name] = true
		if e.kind != "github" {
			// A found repository is a resolved path: a used folder spelled through a link is the same place.
			listed[s.d.facts.lookup(name).resolved] = true
		}
		out[i] = &athenav1.RepoView{Name: name, Kind: e.kind}
		if !e.at.IsZero() {
			out[i].LastUsedAt = timestamppb.New(e.at)
		}
	}
	if err := s.brakeNumbers(out); err != nil {
		return nil, err
	}
	local := 0
	for _, dir := range s.d.local.list() {
		if local == maxLocalRepos {
			break
		}
		if !listed[dir] {
			local++
			out = append(out, &athenav1.RepoView{Name: dir, Kind: "git"})
		}
	}
	return out, nil
}

// brakeNumbers fills, for each GitHub repository, what waits on the owner there, the ceiling it is held to, how many
// candidates the brake holds and whether its work is held for the owner. Without a Policy yet the ceiling is the
// default and the mode live.
func (s hubSource) brakeNumbers(repos []*athenav1.RepoView) error {
	hasGitHub := false
	for _, r := range repos {
		hasGitHub = hasGitHub || r.Kind == "github"
	}
	if !hasGitHub {
		return nil
	}
	var tickets []core.Ticket
	var approvals []core.Approval
	var brakes []core.Brake
	err := s.d.st.Tx(func(tx *store.Tx) error {
		open, err := tx.OpenTickets()
		if err != nil {
			return err
		}
		for _, t := range open {
			tickets = append(tickets, *t)
		}
		pending, err := tx.PendingApprovals()
		if err != nil {
			return err
		}
		for _, a := range pending {
			approvals = append(approvals, *a)
		}
		brakes, err = tx.Brakes()
		return err
	})
	if err != nil {
		return err
	}
	var pol core.Policy
	if s.d.policy != nil {
		pol = s.d.policy.policy()
	}
	for _, r := range repos {
		if r.Kind != "github" {
			continue
		}
		r.Owed = int32(core.OwedAttention(tickets, approvals, r.Name))
		r.Ceiling, r.Mode = policy.DefaultWIPCeiling, "live"
		if pol != nil {
			r.Ceiling = int32(pol.WIPCeiling(r.Name))
			if pol.Shadow(r.Name) {
				r.Mode = "shadow"
			}
		}
		for _, b := range brakes {
			if strings.EqualFold(b.Repo, r.Name) {
				r.Braked++
			}
		}
	}
	return nil
}

// Notifications returns the kinds an owner can mute and the ones they muted.
func (s hubSource) Notifications() ([]string, []string, error) {
	muted, err := web.MutedKinds(s.d.st)
	if err != nil {
		return nil, nil, err
	}
	return web.NotificationKinds(), muted, nil
}

// Prompts says which prompt parts the owner overrode.
func (s hubSource) Prompts() (*athenav1.PromptsView, error) { return promptsView(s.d.st) }

// Control is the kill switch (not paused when the server has none).
func (s hubSource) Control() pause.State {
	if s.d.pause == nil {
		return pause.State{}
	}
	return s.d.pause.State()
}

// Offers is the standing allow rules on offer: the owner's approval streaks, under the current Policy and what they
// dismissed.
func (s hubSource) Offers() ([]hub.Offer, error) {
	streaks, err := s.d.st.ApprovalStreaks()
	if err != nil || len(streaks) == 0 {
		return nil, err
	}
	cur, _, err := s.d.policy.saved()
	if err != nil {
		return nil, err
	}
	dismissed, err := s.d.dismissedOffers()
	if err != nil {
		return nil, err
	}
	return hub.Offers(streaks, cur, dismissed), nil
}
