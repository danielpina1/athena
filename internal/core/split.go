package core

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits of a split, overridable by Policy `split` settings.
const (
	DefaultMaxChildren   = 5
	DefaultMaxHops       = 3
	DefaultMessageLimit  = 10
	DefaultMessageWindow = 10 * time.Minute
	DefaultAutoApprove   = 0
	// WaitForMax is how long wait_for waits before answering "not yet".
	WaitForMax = 20 * time.Second
	// PlanTimeout bounds one planner turn.
	PlanTimeout = 2 * time.Minute
)

// ChildSpec describes one sub-task. Key is local to one call or breakdown; a Deps entry is a Key there first, and
// otherwise a T-prefixed id of a child of the lead.
type ChildSpec struct {
	Key          string   `json:"key,omitempty"`
	Title        string   `json:"title,omitempty"`
	Instructions string   `json:"instructions,omitempty"`
	Areas        []string `json:"areas,omitempty"`
	Deps         []string `json:"deps,omitempty"`
	Runtime      string   `json:"runtime,omitempty"`
	Node         string   `json:"node,omitempty"`
	// Model is an optional model hint.
	Model string `json:"model,omitempty"`
	// Base is a commit to branch from (hand_off only).
	Base string `json:"base,omitempty"`
}

// Overlap names two sub-tasks that touch the same areas.
type Overlap struct {
	A     string   `json:"a"`
	B     string   `json:"b"`
	Areas []string `json:"areas,omitempty"`
}

// Breakdown is a planned split.
type Breakdown struct {
	Revision int         `json:"revision,omitempty"`
	Tasks    []ChildSpec `json:"tasks,omitempty"`
	Overlaps []Overlap   `json:"overlaps,omitempty"`
	Note     string      `json:"note,omitempty"`
	Strategy string      `json:"strategy,omitempty"`
}

// States of a PlanState.
const (
	PlanPlanning = "planning"
	PlanDraft    = "draft"
	PlanFailed   = "failed"
	PlanApproved = "approved"
	PlanRejected = "rejected"
)

// PlanState is where a plan-first Ticket's split stands.
type PlanState struct {
	State     string     `json:"state"`
	Breakdown *Breakdown `json:"breakdown,omitempty"`
	Error     string     `json:"error,omitempty"`
	At        time.Time  `json:"at,omitzero"`
}

// SplitLimits are the limits of splitting and of agent messages; a zero field takes its default.
type SplitLimits struct {
	MaxChildren   int           `json:"maxChildren,omitempty"`
	MaxHops       int           `json:"maxHops,omitempty"`
	MessageLimit  int           `json:"messageLimit,omitempty"`
	AutoApprove   int           `json:"autoApprove,omitempty"`
	MessageWindow time.Duration `json:"messageWindow,omitempty"`
}

// WithDefaults fills the zero fields from the defaults.
func (l SplitLimits) WithDefaults() SplitLimits {
	if l.MaxChildren == 0 {
		l.MaxChildren = DefaultMaxChildren
	}
	if l.MaxHops == 0 {
		l.MaxHops = DefaultMaxHops
	}
	if l.MessageLimit == 0 {
		l.MessageLimit = DefaultMessageLimit
	}
	if l.AutoApprove == 0 {
		l.AutoApprove = DefaultAutoApprove
	}
	if l.MessageWindow == 0 {
		l.MessageWindow = DefaultMessageWindow
	}
	return l
}

// AgentMessage is a message from one agent's Ticket to another's; ReplyTo is set on an answer.
type AgentMessage struct {
	ID      string    `json:"id"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	Text    string    `json:"text"`
	ReplyTo string    `json:"replyTo,omitempty"`
	Hops    int       `json:"hops,omitempty"`
	At      time.Time `json:"at,omitzero"`
}

// AgentView is what an agent sees of another Ticket. Relation is lead, child, sibling or repo.
type AgentView struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Stage    string `json:"stage"`
	Action   string `json:"action,omitempty"`
	Relation string `json:"relation"`
	Node     string `json:"node,omitempty"`
	NodeName string `json:"nodeName,omitempty"`
	Runtime  string `json:"runtime,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Repo     string `json:"repo,omitempty"`
}

// WaitResult is the answer of wait_for.
type WaitResult struct {
	Done    bool          `json:"done"`
	Ticket  *AgentView    `json:"ticket,omitempty"`
	Summary string        `json:"summary,omitempty"`
	Head    string        `json:"head,omitempty"`
	Answer  *AgentMessage `json:"answer,omitempty"`
	Note    string        `json:"note,omitempty"`
}

var specKeyRE = regexp.MustCompile(`^[a-z0-9-]{1,16}$`)

// CheckSpecs validates sub-tasks against the ids of the lead's existing children and the cap max. It returns the
// specs with empty Keys numbered "s1", "s2", ...; it knows no lead id.
func CheckSpecs(specs []ChildSpec, existing []string, max int) ([]ChildSpec, error) {
	room := max - len(existing)
	if len(specs) < 1 || len(specs) > room {
		return nil, userErrorf("a split needs between 1 and %d sub-tasks; this one has %d", room, len(specs))
	}
	out := make([]ChildSpec, len(specs))
	copy(out, specs)
	for i := range out {
		if out[i].Key == "" {
			out[i].Key = "s" + strconv.Itoa(i+1)
		}
	}
	keys := map[string]bool{}
	for _, s := range out {
		if !specKeyRE.MatchString(s.Key) {
			return nil, userErrorf("sub-task key %q must be 1 to 16 characters of a-z, 0-9 and -", s.Key)
		}
		if !strings.ContainsAny(s.Key, "abcdefghijklmnopqrstuvwxyz") {
			return nil, userErrorf("sub-task key %q needs a letter, so it cannot be mistaken for a ticket id", s.Key)
		}
		if keys[s.Key] {
			return nil, userErrorf("sub-task key %q is used twice", s.Key)
		}
		keys[s.Key] = true
	}
	have := map[string]bool{}
	for _, id := range existing {
		have[id] = true
	}
	for _, s := range out {
		if n := utf8.RuneCountInString(s.Title); n < 1 || n > 80 {
			return nil, userErrorf("sub-task %q needs a title of 1 to 80 characters", s.Key)
		}
		if strings.TrimSpace(s.Instructions) == "" {
			return nil, userErrorf("sub-task %q needs instructions", s.Key)
		}
		if s.Runtime != "" && !ValidRuntime(s.Runtime) {
			return nil, userErrorf("sub-task %q has an unknown runtime %q", s.Key, s.Runtime)
		}
		if utf8.RuneCountInString(s.Model) > 64 {
			return nil, userErrorf("sub-task %q has a model hint over 64 characters", s.Key)
		}
		for _, d := range s.Deps {
			if d == s.Key {
				return nil, userErrorf("sub-task %q depends on itself", s.Key)
			}
			if keys[d] {
				continue
			}
			if id, ok := ParseTicketID(d); ok && strings.HasPrefix(strings.ToUpper(d), "T-") && have[id] {
				continue
			}
			return nil, userErrorf("sub-task %q depends on %q, which is not a sub-task of this split or a child of the lead", s.Key, d)
		}
	}
	if cycle := specCycle(out); cycle != nil {
		return nil, userErrorf("the sub-tasks' dependencies form a cycle: %s", strings.Join(cycle, " → "))
	}
	return out, nil
}

// specCycle runs Kahn's algorithm over the Key-to-Key deps and, when some tasks never become free, returns one
// cycle among them as keys, first key repeated at the end; nil when there is none.
func specCycle(specs []ChildSpec) []string {
	indeg := map[string]int{}
	next := map[string][]string{}
	deps := map[string][]string{}
	for _, s := range specs {
		indeg[s.Key] += 0
	}
	for _, s := range specs {
		for _, d := range s.Deps {
			if _, ok := indeg[d]; !ok {
				continue // an existing child: already made, no cycle through it
			}
			indeg[s.Key]++
			next[d] = append(next[d], s.Key)
			deps[s.Key] = append(deps[s.Key], d)
		}
	}
	var queue []string
	for _, s := range specs {
		if indeg[s.Key] == 0 {
			queue = append(queue, s.Key)
		}
	}
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		delete(indeg, k)
		for _, n := range next[k] {
			if indeg[n]--; indeg[n] == 0 {
				queue = append(queue, n)
			}
		}
	}
	if len(indeg) == 0 {
		return nil
	}
	// Every remaining task has a remaining dep; walk deps from the first one until a key repeats.
	var start string
	for _, s := range specs {
		if _, ok := indeg[s.Key]; ok {
			start = s.Key
			break
		}
	}
	seen := map[string]int{}
	var path []string
	for k := start; ; {
		if i, ok := seen[k]; ok {
			cyc := append([]string(nil), path[i:]...)
			// Each key depends on the next one in the list.
			return append(cyc, k)
		}
		seen[k] = len(path)
		path = append(path, k)
		for _, d := range deps[k] {
			if _, ok := indeg[d]; ok {
				k = d
				break
			}
		}
	}
}

// Overlaps reports the pairs of tasks, in task order, whose areas overlap: equal, or one a path-segment prefix of
// the other. A trailing "/" and a leading "./" are ignored.
func Overlaps(tasks []ChildSpec) []Overlap {
	var out []Overlap
	for i := range tasks {
		for j := i + 1; j < len(tasks); j++ {
			var hit []string
			for _, a := range tasks[i].Areas {
				for _, b := range tasks[j].Areas {
					if areasOverlap(a, b) {
						hit = append(hit, a)
						break
					}
				}
			}
			if len(hit) > 0 {
				out = append(out, Overlap{A: tasks[i].Key, B: tasks[j].Key, Areas: hit})
			}
		}
	}
	return out
}

func cleanArea(a string) string {
	a = strings.TrimSpace(a)
	for strings.HasPrefix(a, "./") {
		a = a[2:]
	}
	return strings.TrimRight(a, "/")
}

func areasOverlap(a, b string) bool {
	a, b = cleanArea(a), cleanArea(b)
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// ParseTicketID gives "T-%04d" from "T-12", "t-0012" or "12".
func ParseTicketID(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == 'T' || s[0] == 't') && s[1] == '-' {
		s = s[2:]
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || s[0] == '+' || s[0] == '-' {
		return "", false
	}
	return fmt.Sprintf("T-%04d", n), true
}
