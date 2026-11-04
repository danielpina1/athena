package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/router"
	"github.com/danielpina1/athena/internal/sources/github"
)

// The control socket's views of the routing state (athena status, triggers, threads, decisions) and of the Policy.

const (
	// listLimit bounds the rows of triggers and decisions.
	listLimit = 200
	// previewRunes bounds a Trigger's text in a view.
	previewRunes = 80
	// sessionShort is how much of the router session id status shows.
	sessionShort = 8
)

// HealthView is what athena status shows besides the Tickets.
type HealthView struct {
	Sources []SourceHealth `json:"sources"`
	// Effects maps each effect kind that needs attention to why; empty when all is well.
	Effects map[string]string `json:"effects"`
	Router  RouterHealth      `json:"router"`
	// GitHubRateRemaining is the installation's rate-limit remaining, when GitHub runs and answered once.
	GitHubRateRemaining *int `json:"githubRateRemaining,omitempty"`
}

// SourceHealth is one Source: its error, empty when it is healthy.
type SourceHealth struct {
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
}

// RouterHealth is the router session's state (off without GitHub, else router.State*) and its session id, short.
type RouterHealth struct {
	State   string `json:"state"`
	Session string `json:"session,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (c *control) health() HealthView {
	h := HealthView{Effects: map[string]string{}, Router: RouterHealth{State: "off"}}
	if c.gh != nil {
		sh := SourceHealth{Name: github.SourceName}
		if err := c.gh.health(); err != nil {
			sh.Error = err.Error()
		}
		h.Sources = append(h.Sources, sh)
		if c.gh.app != nil {
			if n := c.gh.app.Client().RateRemaining(); n >= 0 {
				h.GitHubRateRemaining = &n
			}
		}
	}
	if c.fx != nil {
		for kind, err := range c.fx.Health() {
			h.Effects[kind] = err.Error()
		}
	}
	if c.host != nil {
		h.Router.State = c.host.State()
		if id := c.host.SessionID(); id != "" {
			h.Router.Session = id[:min(len(id), sessionShort)]
		}
		if err := c.host.Health(); err != nil && h.Router.State == router.StateCrashed {
			h.Router.Error = err.Error()
		}
	}
	return h
}

// TriggerView is a Trigger in a list: no Meta, no Channel address, its text cut to one short line.
type TriggerView struct {
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Source    string    `json:"source"`
	Actor     string    `json:"actor"`
	Kind      string    `json:"kind,omitempty"`
	Addressed string    `json:"addressed,omitempty"`
	Thread    string    `json:"thread,omitempty"`
	Text      string    `json:"text,omitempty"`
	// Outcome says what became of it: filtered:<code>, reached/attached Tickets, requeued.
	Outcome string `json:"outcome,omitempty"`
}

func triggerView(t *core.Trigger) TriggerView {
	v := TriggerView{ID: t.ID, At: t.At, Source: t.Source, Actor: t.Actor.ID, Kind: t.Kind, Addressed: t.Addressed,
		Text: clip(t.Text, previewRunes)}
	if len(t.Threads) > 0 {
		v.Thread = t.Threads[0]
	}
	d := t.Disposition
	var parts []string
	if d.Filtered != "" {
		parts = append(parts, "filtered:"+d.Filtered)
	}
	if len(d.Reached) > 0 {
		parts = append(parts, "reached:"+strings.Join(d.Reached, ","))
	}
	if len(d.Attached) > 0 {
		parts = append(parts, "attached:"+strings.Join(d.Attached, ","))
	}
	if len(d.HandedBack) > 0 {
		ids := make([]string, 0, len(d.HandedBack))
		for id := range d.HandedBack {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		parts = append(parts, "handedBack:"+strings.Join(ids, ","))
	}
	if d.Requeued != nil {
		parts = append(parts, "requeued")
	}
	v.Outcome = strings.Join(parts, " ")
	return v
}

// ThreadView is a Thread in a list.
type ThreadView struct {
	Key    string `json:"key"`
	Queued int    `json:"queued"`
	// Eval is the Eval in progress: its id, strength and whether its candidate reached the router.
	Eval *EvalView `json:"eval,omitempty"`
	// Tickets are the subscribed Tickets, each with its flags: T-0001(own,bound,context).
	Tickets   []string `json:"tickets,omitempty"`
	Decisions int      `json:"decisions"`
	Closed    string   `json:"closed,omitempty"`
}

// EvalView is a Thread's Eval in progress.
type EvalView struct {
	ID          string     `json:"id"`
	Strength    string     `json:"strength"`
	StartedAt   time.Time  `json:"startedAt"`
	DeliveredAt *time.Time `json:"deliveredAt,omitempty"`
}

func threadView(th *core.Thread) ThreadView {
	v := ThreadView{Key: th.Key, Queued: len(th.Queued), Decisions: len(th.Decisions)}
	if ev := th.Eval; ev != nil {
		v.Eval = &EvalView{ID: ev.ID, Strength: ev.Strength, StartedAt: ev.StartedAt, DeliveredAt: ev.DeliveredAt}
	}
	for _, s := range th.Subscriptions {
		var flags []string
		if s.Own {
			flags = append(flags, "own")
		}
		if s.Bound {
			flags = append(flags, "bound")
		}
		if s.Context {
			flags = append(flags, "context")
		}
		t := s.TicketID
		if len(flags) > 0 {
			t += "(" + strings.Join(flags, ",") + ")"
		}
		v.Tickets = append(v.Tickets, t)
	}
	if th.Closed != nil {
		v.Closed = string(th.Closed.Outcome)
	}
	return v
}

// DecisionView is a Decision in a list.
type DecisionView struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	Thread   string    `json:"thread,omitempty"`
	TicketID string    `json:"ticketId,omitempty"`
	Triggers []string  `json:"triggers,omitempty"`
	// Reasons are the reason codes, with a said reason's text.
	Reasons     []string `json:"reasons,omitempty"`
	WouldAccept bool     `json:"wouldAccept,omitempty"`
	Dismissed   bool     `json:"dismissed,omitempty"`
}

func decisionView(d *core.Decision) DecisionView {
	v := DecisionView{ID: d.ID, At: d.At, Kind: d.Kind, Thread: d.Thread, TicketID: d.TicketID, Triggers: d.TriggerIDs,
		WouldAccept: d.WouldAccept, Dismissed: d.Dismissed != nil}
	for _, r := range d.Reasons {
		s := r.Code
		if text := r.Values["text"]; text != "" {
			s += ": " + clip(text, previewRunes)
		}
		v.Reasons = append(v.Reasons, s)
	}
	return v
}

var githubThreadRe = regexp.MustCompile(`^github:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)(?:#([0-9]+)|@(.+))$`)

// canonicalThread spells a GitHub Thread key the way the GitHub Source does (lower-case owner/name), so a key typed
// with GitHub's own casing names the same Thread; any other key is returned as it is.
func canonicalThread(key string) string {
	m := githubThreadRe.FindStringSubmatch(strings.TrimSpace(key))
	if m == nil {
		return strings.TrimSpace(key)
	}
	if m[2] != "" {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return key
		}
		return github.ThreadKey(m[1], n)
	}
	return github.BranchThreadKey(m[1], m[3])
}

// PolicyView is the current revision of the Policy settings.
type PolicyView struct {
	// Revision is 0 while nobody saved settings: they are the defaults.
	Revision int             `json:"revision"`
	Version  string          `json:"version"`
	Settings policy.Settings `json:"settings"`
}

func (c *control) policyShow() (PolicyView, error) {
	s, rev := c.policy.current()
	return PolicyView{Revision: rev, Version: policy.Version(s), Settings: s}, nil
}

const staleSettings = "the settings changed meanwhile; show them and try again"

// resumePolicyChange tells the Engine of a change saved but not told before (a failed tell or a crash), at startup.
func (c *control) resumePolicyChange() error { return c.policy.resume(c.tell) }

// tell tells the Engine of a settings change (tellEngine), after the test hook.
func (c *control) tell(from, to policy.Settings) (map[string]string, error) {
	if c.onPolicyChange != nil {
		if err := c.onPolicyChange(from, to); err != nil {
			return nil, err
		}
	}
	return tellEngine(c.eng, from, to)
}

// tellEngine tells the Engine of a settings change: each open Ticket it bears on gets its notice, and the Evals in
// progress start again under the new version.
func tellEngine(eng *core.Engine, from, to policy.Settings) (map[string]string, error) {
	tickets, err := eng.Tickets(false)
	if err != nil {
		return nil, err
	}
	notices := policy.Effects(tickets, from, to)
	return notices, eng.PolicyChanged(policy.Version(to), notices)
}

// PolicySetResult is a saved revision and the open Tickets the change was told to.
type PolicySetResult struct {
	Revision int      `json:"revision"`
	Version  string   `json:"version"`
	Notified []string `json:"notified"`
}

// decodeSettings reads a settings document in either form: the bare settings, or the output of `policy show`
// ({"revision", "version", "settings": {...}}), whose revision and version are ignored. Unknown fields are refused at
// either level.
func decodeSettings(raw json.RawMessage) (policy.Settings, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return policy.Settings{}, err
	}
	if inner, ok := top["settings"]; ok {
		for k := range top {
			if k != "settings" && k != "revision" && k != "version" {
				return policy.Settings{}, fmt.Errorf("unknown field %q", k)
			}
		}
		raw = inner
	}
	var s policy.Settings
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return policy.Settings{}, err
	}
	return s, nil
}

// policySet validates and saves a settings document as a cli revision, then tells the Engine (tell), atomically with
// the save and retried if the tell fails (policyCache.change).
func (c *control) policySet(raw json.RawMessage) (PolicySetResult, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return PolicySetResult{}, userErrorf("policy set needs a settings document")
	}
	s, err := decodeSettings(raw)
	if err != nil {
		return PolicySetResult{}, userErrorf("the settings are not valid JSON: %v", err)
	}
	if err := policy.Validate(s); err != nil {
		return PolicySetResult{}, userErrorf("%v", err)
	}
	rev, cur, notices, err := c.policy.change(s, policy.OriginCLI, c.tell)
	if errors.Is(err, policy.ErrStaleRevision) {
		return PolicySetResult{}, userErrorf(staleSettings)
	}
	if err != nil {
		return PolicySetResult{}, err
	}
	version := policy.Version(cur)
	out := PolicySetResult{Revision: rev, Version: version, Notified: []string{}}
	for id := range notices {
		out.Notified = append(out.Notified, id)
	}
	sort.Strings(out.Notified)
	c.log.Info("policy saved", "revision", rev, "version", version, "origin", policy.OriginCLI, "notified", len(notices))
	return out, nil
}
