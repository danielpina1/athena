package core

import (
	"fmt"
	"slices"
	"strings"
)

// Child Tickets: a lead's worker splits work off into children (SpawnChildren), each a Ticket of its own under the
// lead's Mandate, on its own branch. A child whose deps have not all ended has no Assignment yet (R1); each end of a
// child tells its lead and releases its dependents, and the lead's end stops its open children.

// NodeDirectory resolves a node by name or id, for a child's node hint.
type NodeDirectory interface {
	// Resolve returns the id of the node named or identified by nameOrID.
	Resolve(nameOrID string) (id string, ok bool)
	// Name returns a node's name.
	Name(id string) string
}

// Types of the TicketEvents a lead records about its children.
const (
	EventChildSpawned = "child/spawned"
	EventChildEnded   = "child/ended"
	EventChildFailed  = "child/failed"
)

// Outcomes of a child/ended TicketEvent.
const (
	childReady     = "ready"
	childDropped   = "dropped"
	childStopped   = "stopped"
	childContinued = "continued"
)

// codeSplit is the reason of a child's accept Decision; Values: lead.
const codeSplit = "split"

// SpawnChildren splits sub-tasks off the open lead as child Tickets and returns their ids in spec order. A child
// whose deps have all ended starts at once; one that waits gets its Assignment when they have. Refusals are
// UserErrors: a lead whose split plan is not settled, then o.spawn's.
func (e *Engine) SpawnChildren(leadID string, specs []ChildSpec) ([]string, error) {
	var ids []string
	err := e.command(cmdSpawnChildren, func(o *op) error {
		lead, err := o.require(leadID)
		if err != nil {
			return err
		}
		if lead.Ended != nil {
			return userErrorf("ticket %s has ended", leadID)
		}
		if p := lead.Plan; p != nil && (p.State == PlanPlanning || p.State == PlanDraft || p.State == PlanFailed) {
			return userErrorf("%s waits for its split plan to be settled; it cannot split off more work until then", leadID)
		}
		if ids, err = o.spawn(lead, specs); err != nil {
			return err
		}
		return o.tx.PutTicket(lead)
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// ContinueWithout is the owner's choice to go on without an open child: it ends stopped, by a human, and the
// dependents that waited for it start.
func (e *Engine) ContinueWithout(childID string) error {
	return e.command(cmdContinueWithout, func(o *op) error {
		t, err := o.require(childID)
		if err != nil {
			return err
		}
		if t.Lead == "" {
			return userErrorf("%s is not a child ticket", childID)
		}
		if t.Ended != nil {
			return userErrorf("%s has ended", childID)
		}
		if err := o.end(t, EndByHuman, OutcomeStopped, ReasonContinuedWithout); err != nil {
			return err
		}
		return o.tx.PutTicket(t)
	})
}

// splitLimits are the Engine's split limits, with their defaults.
func (o *op) splitLimits() SplitLimits {
	if o.split == nil {
		return SplitLimits{}.WithDefaults()
	}
	return o.split().WithDefaults()
}

// spawn makes the children of specs under lead, which it changes but does not put; the caller puts it. It refuses,
// in order: a lead with no git repository (R2), one nested too deep, an inferred lead under the WIP brake (R14, R25),
// one over the fan-out cap (R15), bad specs (CheckSpecs) and an unknown node hint. It has no lead-state check:
// SpawnChildren and ApproveBreakdown each make their own.
func (o *op) spawn(lead *Ticket, specs []ChildSpec) ([]string, error) {
	lim := o.splitLimits()
	dir := ""
	if lead.Execution != nil {
		dir = lead.Execution.Directory
	}
	if lead.Repo == "" && dir == "" {
		return nil, userErrorf("Splitting needs a git repository: each child works on its own branch.")
	}
	if lead.Depth+1 > lim.MaxHops {
		return nil, userErrorf("Child tickets nest at most %d deep; do this part yourself.", lim.MaxHops)
	}
	if err := o.spawnBrake(lead); err != nil {
		return nil, err
	}
	if len(lead.Children)+len(specs) > lim.MaxChildren {
		return nil, userErrorf("%s may have at most %d child tickets (it has %d); split into fewer pieces or do the rest yourself.",
			lead.ID, lim.MaxChildren, len(lead.Children))
	}
	checked, err := CheckSpecs(specs, lead.Children, lim.MaxChildren)
	if err != nil {
		return nil, err
	}
	nodes := make([]string, len(checked))
	for i, s := range checked {
		if nodes[i], err = o.childNode(lead, s.Node); err != nil {
			return nil, err
		}
	}
	// Every child gets its id first, so a dep on a later sub-task resolves.
	ids := make([]string, len(checked))
	byKey := map[string]string{}
	for i, s := range checked {
		n, err := o.tx.NextSeq(ticketSeq)
		if err != nil {
			return nil, err
		}
		ids[i] = fmt.Sprintf("T-%04d", n)
		byKey[s.Key] = ids[i]
	}
	for i, s := range checked {
		deps, waiting, err := o.resolveDeps(s.Deps, byKey)
		if err != nil {
			return nil, err
		}
		if err := o.makeChild(lead, ids[i], s, nodes[i], deps, waiting); err != nil {
			return nil, err
		}
	}
	lead.Children = append(lead.Children, ids...)
	lead.UpdatedAt = o.at
	o.event(EventChildrenSpawned, lead, "", map[string]any{"children": slices.Clone(ids)})
	return ids, nil
}

// spawnBrake refuses a lead Athena chose itself (inferred, or an Autopilot entry's) while its repository has the WIP
// ceiling's worth waiting on the owner (R14). Nothing is recorded: the worker may try again (R25).
func (o *op) spawnBrake(lead *Ticket) error {
	m := lead.Mandate
	if lead.Repo == "" || o.policy == nil || (m.Strength != StrengthInferred && m.Basis.Via != ViaAutopilot) {
		return nil
	}
	p := o.policy()
	if p == nil {
		return nil
	}
	owed, err := o.owed(lead.Repo)
	if err != nil {
		return err
	}
	if ceiling := p.WIPCeiling(lead.Repo); owed >= ceiling {
		return userErrorf("Athena holds back new work in %s: %d things wait on the owner there (the ceiling is %d). Do this part yourself, or wait.",
			lead.Repo, owed, ceiling)
	}
	return nil
}

// childNode is the node a child runs on: a folder lead's children are pinned to the lead's folder node (its pin, else
// the node it was placed on) and their hint is ignored (R2); a repository lead's child runs on its resolved hint.
func (o *op) childNode(lead *Ticket, hint string) (string, error) {
	if lead.Repo == "" {
		if lead.Execution.Node != "" {
			return lead.Execution.Node, nil
		}
		for i := len(lead.Assignments) - 1; i >= 0; i-- {
			if n := lead.Assignments[i].Node; n != "" {
				return n, nil
			}
		}
		return "", nil
	}
	if hint == "" {
		return "", nil
	}
	if o.nodes != nil {
		if id, ok := o.nodes.Resolve(hint); ok {
			return id, nil
		}
	}
	return "", userErrorf("no such node: %s", hint)
}

// resolveDeps turns a sub-task's deps (keys of this split, else ids of the lead's children) into Ticket ids, and
// lists those not yet ended.
func (o *op) resolveDeps(deps []string, byKey map[string]string) (ids, waiting []string, err error) {
	for _, d := range deps {
		if id, ok := byKey[d]; ok {
			ids = append(ids, id)
			waiting = append(waiting, id) // made in this call: open
			continue
		}
		id, _ := ParseTicketID(d) // CheckSpecs let only a child's id through
		dep, err := o.require(id)
		if err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		if dep.Ended == nil {
			waiting = append(waiting, id)
		}
	}
	return ids, waiting, nil
}

// makeChild stores one child of lead, with its accept Decision and brief; it starts unless it waits on a dep.
func (o *op) makeChild(lead *Ticket, id string, s ChildSpec, node string, deps, waiting []string) error {
	lx := lead.Execution
	if lx == nil {
		lx = &Execution{}
	}
	x := &Execution{Directory: lx.Directory, Runtime: lx.Runtime, Model: lx.Model, Node: node, Base: s.Base}
	if s.Runtime != "" {
		x.Runtime = s.Runtime
	}
	if s.Model != "" {
		x.Model = s.Model
	}
	mandate := lead.Mandate
	mandate.Basis.Rules = slices.Clone(mandate.Basis.Rules)
	decisionMandate := mandate
	decision := &Decision{ID: newID(), Kind: DecisionAccept, TicketID: id, At: o.at, Mandate: &decisionMandate,
		Reasons: []Reason{{Code: codeSplit, Values: map[string]string{"lead": lead.ID}}}}
	t := &Ticket{
		ID: id, Title: s.Title, DecisionID: decision.ID, Mandate: mandate,
		CreatedAt: o.at, UpdatedAt: o.at,
		Origin:    Origin{TriggerID: lead.Origin.TriggerID, TriggerIDs: slices.Clone(lead.Origin.TriggerIDs)},
		Execution: x,
		Repo:      lead.Repo,
		Lead:      lead.ID,
		SplitKey:  s.Key,
		Deps:      deps,
		Waiting:   waiting,
		Depth:     lead.Depth + 1,
	}
	brief := Input{ID: newID(), Kind: InputTask, At: o.at, From: lead.ID, Text: childBrief(lead, t, s)}
	t.Inputs = []Input{brief}
	t.Undelivered = []string{brief.ID}
	assignment := ""
	if len(waiting) == 0 {
		assignment = o.newAssignment(t)
		o.start(t, assignment)
	}
	if err := o.tx.PutDecision(decision); err != nil {
		return err
	}
	if err := o.tx.PutTicket(t); err != nil {
		return err
	}
	o.recordEvent(lead, EventChildSpawned, id, "", s.Title)
	o.event(EventTicketCreated, t, assignment, map[string]any{"decisionId": decision.ID, "lead": lead.ID})
	return nil
}

// childBrief is a child's first Input: its lead, its sub-task, its areas and its branch.
func childBrief(lead, t *Ticket, s ChildSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a child ticket of %s (%s).\n\nSub-task: %s\n\n%s\n", t.ID, lead.ID, lead.Title, s.Title,
		strings.TrimSpace(s.Instructions))
	if len(s.Areas) > 0 {
		fmt.Fprintf(&b, "\nAreas: %s\n", strings.Join(s.Areas, ", "))
	}
	base := "the repository's default branch"
	if s.Base != "" {
		base = s.Base
	}
	fmt.Fprintf(&b, "\nYour branch is %s, from %s.\n", childBranch(t.ID), base)
	fmt.Fprintf(&b, "Commit your work on your branch and report ready; do not push or open a pull request: %s merges your branch.", lead.ID)
	return b.String()
}

// childBranch is a child's branch: athena/<lowercased id>.
func childBranch(id string) string { return "athena/" + strings.ToLower(id) }

// recordEvent adds a TicketEvent to the Ticket's timeline.
func (o *op) recordEvent(t *Ticket, typ, ref, outcome, text string) {
	t.Events = append(t.Events, TicketEvent{Type: typ, At: o.at, Ref: ref, Outcome: outcome, Text: text})
	t.UpdatedAt = o.at
}

// refuseOpenChildren refuses a lead's ready report while any of its children is open (R9).
func (o *op) refuseOpenChildren(t *Ticket) error {
	var open []string
	for _, id := range t.Children {
		c, err := o.tx.GetTicket(id)
		if err != nil {
			return err
		}
		if c != nil && c.Ended == nil {
			open = append(open, id)
		}
	}
	if len(open) == 0 {
		return nil
	}
	return userErrorf("%s has open child tickets (%s): wait_for them, or ask the owner to drop them, before you report ready.",
		t.ID, strings.Join(open, ", "))
}

// childFailed raises the open lead's Attention on a child's failed or blocked report r (R8).
func (o *op) childFailed(t *Ticket, r ReportInput) error {
	if t.Lead == "" {
		return nil
	}
	lead, err := o.tx.GetTicket(t.Lead)
	if err != nil || lead == nil || lead.Ended != nil {
		return err
	}
	how := "failed"
	if r.State == ReportBlocked {
		how = "is blocked"
	}
	o.settleAttention(lead)
	lead.Attention = &Attention{ID: newID(), Child: t.ID, At: o.at,
		Question: fmt.Sprintf("Child %s (%s) %s: %s. Retry it, drop it, or continue without it.", t.ID, t.Title, how, r.Summary)}
	o.recordEvent(lead, EventChildFailed, t.ID, string(r.State), r.Summary)
	return o.tx.PutTicket(lead)
}

// clearChildAttention clears the open lead's Attention when it names the child t.
func (o *op) clearChildAttention(t *Ticket) error {
	if t.Lead == "" {
		return nil
	}
	lead, err := o.tx.GetTicket(t.Lead)
	if err != nil || lead == nil || lead.Ended != nil || lead.Attention == nil || lead.Attention.Child != t.ID {
		return err
	}
	o.settleAttention(lead)
	lead.Attention = nil
	lead.UpdatedAt = o.at
	return o.tx.PutTicket(lead)
}

// endChildren stops the open children of a lead that just ended (R9); their ends tell nobody, as the lead has ended.
func (o *op) endChildren(lead *Ticket) error {
	for _, id := range lead.Children {
		c, err := o.tx.GetTicket(id)
		if err != nil {
			return err
		}
		if c == nil || c.Ended != nil {
			continue
		}
		if err := o.end(c, EndByExecutor, OutcomeStopped, ReasonLeadEnded); err != nil {
			return err
		}
		if err := o.tx.PutTicket(c); err != nil {
			return err
		}
	}
	return nil
}

// childEnded runs when the child t ended while its lead is open. A stop that is not ReasonContinuedWithout also
// stops, transitively, the siblings that depend on an ended one and never started (R8). Then, for each child that
// ended, the lead is told (an InputChild), its Attention about it is cleared, and it leaves the Waiting lists of the
// lead and its siblings (and the lead's Deps); a sibling is told of each dep that completed, and one that no longer
// waits gets its Assignment and starts, as does a lead that waited.
func (o *op) childEnded(t *Ticket) error {
	if t.Lead == "" || o.endingChildren || t.Ended.Reason == ReasonLeadEnded {
		return nil
	}
	lead, err := o.tx.GetTicket(t.Lead)
	if err != nil || lead == nil || lead.Ended != nil {
		return err
	}
	var siblings []*Ticket
	for _, id := range lead.Children {
		if id == t.ID {
			continue
		}
		s, err := o.tx.GetTicket(id)
		if err != nil {
			return err
		}
		if s != nil && s.Ended == nil {
			siblings = append(siblings, s)
		}
	}
	ended := []*Ticket{t}
	if t.Ended.Outcome == OutcomeStopped && t.Ended.Reason != ReasonContinuedWithout {
		if ended, err = o.stopDependents(t, siblings); err != nil {
			return err
		}
	}
	endedIDs := make([]string, len(ended))
	for i, c := range ended {
		endedIDs[i] = c.ID
	}
	for _, s := range siblings {
		if s.Ended != nil {
			continue
		}
		before := len(s.Waiting)
		s.Waiting = slices.DeleteFunc(s.Waiting, func(id string) bool { return slices.Contains(endedIDs, id) })
		changed := len(s.Waiting) != before
		for _, c := range ended {
			if slices.Contains(s.Deps, c.ID) && c.Ended.Outcome == OutcomeCompleted {
				o.addInput(s, Input{ID: newID(), Kind: InputChild, At: o.at, From: c.ID, Text: depReadyText(c)})
				changed = true
			}
		}
		if !changed {
			continue
		}
		o.startHeld(s)
		if err := o.tx.PutTicket(s); err != nil {
			return err
		}
	}
	waited := len(lead.Waiting)
	for _, c := range ended {
		outcome, text := childOutcome(c)
		if lead.Attention != nil && lead.Attention.Child == c.ID {
			o.settleAttention(lead)
			lead.Attention = nil
		}
		lead.Waiting = slices.DeleteFunc(lead.Waiting, func(id string) bool { return id == c.ID })
		lead.Deps = slices.DeleteFunc(lead.Deps, func(id string) bool { return id == c.ID })
		o.recordEvent(lead, EventChildEnded, c.ID, outcome, "")
		o.addInput(lead, Input{ID: newID(), Kind: InputChild, At: o.at, From: c.ID, Text: text})
	}
	if waited > 0 {
		o.startHeld(lead)
	}
	return o.tx.PutTicket(lead)
}

// stopDependents ends, transitively, the open siblings that list an ended child in Deps and never got an Assignment,
// stopped as the child t was. It returns t and every one it stopped, in the order they ended.
func (o *op) stopDependents(t *Ticket, siblings []*Ticket) ([]*Ticket, error) {
	ended := []*Ticket{t}
	o.endingChildren = true
	defer func() { o.endingChildren = false }()
	for changed := true; changed; {
		changed = false
		for _, s := range siblings {
			if s.Ended != nil || len(s.Assignments) > 0 {
				continue
			}
			if !slices.ContainsFunc(ended, func(c *Ticket) bool { return slices.Contains(s.Deps, c.ID) }) {
				continue
			}
			if err := o.end(s, t.Ended.By, OutcomeStopped, t.Ended.Reason); err != nil {
				return nil, err
			}
			if err := o.tx.PutTicket(s); err != nil {
				return nil, err
			}
			ended = append(ended, s)
			changed = true
		}
	}
	return ended, nil
}

// startHeld gives a held Ticket that no longer waits on any dep its first Assignment, and starts it (R1).
func (o *op) startHeld(t *Ticket) {
	if len(t.Waiting) > 0 || len(t.Assignments) > 0 || t.Ended != nil {
		return
	}
	t.Waiting = nil
	o.start(t, o.newAssignment(t))
	t.UpdatedAt = o.at
}

// childOutcome is how a child ended, as its lead's child/ended event and InputChild say it.
func childOutcome(c *Ticket) (outcome, text string) {
	summary := ""
	if a := c.LatestAssignment(); a != nil && a.Report != nil {
		summary = a.Report.Summary
	}
	switch {
	case c.Ended.Outcome == OutcomeStopped && c.Ended.Reason == ReasonContinuedWithout:
		return childContinued, fmt.Sprintf("The owner chose to continue without %s.", c.ID)
	case c.Ended.Outcome == OutcomeStopped && c.Ended.By == EndByHuman:
		return childStopped, fmt.Sprintf("%s was dropped by the owner.", c.ID)
	case c.Ended.Outcome == OutcomeStopped:
		return childStopped, fmt.Sprintf("%s was stopped.", c.ID)
	case c.Ended.Outcome == OutcomeDropped:
		return childDropped, fmt.Sprintf("%s found nothing to do: %s.", c.ID, summary)
	}
	branch := childBranch(c.ID)
	return childReady, fmt.Sprintf("%s (%s) is ready: %s. Its branch %s is at %s; merge it into yours with git merge %s.",
		c.ID, c.Title, summary, branch, head7(c), branch)
}

// depReadyText tells a dependent that its dep c completed, and where its work is.
func depReadyText(c *Ticket) string {
	branch := childBranch(c.ID)
	return fmt.Sprintf("%s (%s), which this ticket depends on, is ready. Its branch %s is at %s; merge what you need with git merge %s.",
		c.ID, c.Title, branch, head7(c), branch)
}

// head7 is the short head of a child's ready report.
func head7(c *Ticket) string {
	a := c.LatestAssignment()
	if a == nil || a.Report == nil {
		return ""
	}
	return truncateRunes(a.Report.Head, 7)
}
