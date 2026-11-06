package core

import (
	"slices"
	"strconv"
	"strings"
	"time"
)

// The WIP brake: Athena stops starting work it chose itself (an inferred candidate) in a repository while too much
// already waits on the owner there. The held candidates are remembered as Brakes and let go, oldest first, as the
// owner catches up.

// CodeWIPBrake is the reason of the Decision that holds an inferred candidate back; Values: repo, owed (the count of
// things waiting on the owner) and ceiling.
const CodeWIPBrake = "wip_brake"

// Brake is an inferred candidate held back in a repository, until the owner has room for it.
type Brake struct {
	TriggerID string    `json:"triggerId"`
	Repo      string    `json:"repo"`
	Thread    string    `json:"thread"`
	At        time.Time `json:"at"`
}

// OwedAttention counts what waits on the owner in a repository: the open Tickets in a person stage, plus the pending
// Approvals of other open Tickets there. Each Ticket counts once.
func OwedAttention(tickets []Ticket, approvals []Approval, repo string) int {
	counted := map[string]bool{}
	inRepo := map[string]bool{}
	for i := range tickets {
		t := &tickets[i]
		if t.Ended != nil || !strings.EqualFold(t.Repo, repo) {
			continue
		}
		inRepo[t.ID] = true
		if TicketStage(t).Stage == "person" {
			counted[t.ID] = true
		}
	}
	for _, a := range approvals {
		if a.State == ApprovalPending && inRepo[a.TicketID] {
			counted[a.TicketID] = true
		}
	}
	return len(counted)
}

// owed is OwedAttention over the stored Tickets and Approvals.
func (o *op) owed(repo string) (int, error) {
	open, err := o.tx.OpenTickets()
	if err != nil {
		return 0, err
	}
	pending, err := o.tx.PendingApprovals()
	if err != nil {
		return 0, err
	}
	tickets := make([]Ticket, len(open))
	for i, t := range open {
		tickets[i] = *t
	}
	approvals := make([]Approval, len(pending))
	for i, a := range pending {
		approvals[i] = *a
	}
	return OwedAttention(tickets, approvals, repo), nil
}

// brake holds the admitted inferred Triggers of a batch back: one ignore Decision with the brake's reason, and a
// Brake for each. The batch's other Triggers get their own gate reasons.
func (o *op) brake(th *Thread, repo string, owed, ceiling int, admitted []string, notAdmitted []string, reasons []Reason) error {
	if len(notAdmitted) > 0 {
		if err := o.record(th, &Decision{Kind: DecisionIgnore, TriggerIDs: notAdmitted, Reasons: reasons}); err != nil {
			return err
		}
	}
	err := o.record(th, &Decision{Kind: DecisionIgnore, TriggerIDs: admitted, Reasons: []Reason{{Code: CodeWIPBrake,
		Values: map[string]string{"repo": repo, "owed": strconv.Itoa(owed), "ceiling": strconv.Itoa(ceiling)}}}})
	if err != nil {
		return err
	}
	for _, id := range admitted {
		if err := o.tx.PutBrake(Brake{TriggerID: id, Repo: repo, Thread: th.Key, At: o.at}); err != nil {
			return err
		}
	}
	th.Expired = 0
	return o.tx.PutThread(th)
}

// ReleaseBrakes queues the braked candidates of each repository that has room again for a fresh Eval, oldest first.
// A repository has room for its ceiling less what waits on the owner and less the Evals already running there, so a
// backlog never reaches the owner in one pass. It returns the Triggers it released.
func (e *Engine) ReleaseBrakes() ([]string, error) {
	if e.policy == nil {
		return nil, nil
	}
	p := e.policy()
	if p == nil {
		return nil, nil
	}
	var brakes []Brake
	if err := e.repos.Tx(func(tx Tx) (err error) { brakes, err = tx.Brakes(); return }); err != nil {
		return nil, err
	}
	byRepo := map[string][]Brake{}
	var repos []string
	for _, b := range brakes {
		if _, ok := byRepo[b.Repo]; !ok {
			repos = append(repos, b.Repo)
		}
		byRepo[b.Repo] = append(byRepo[b.Repo], b)
	}
	slices.Sort(repos)
	var released []string
	var firstErr error
	for _, repo := range repos {
		repo, list := repo, byRepo[repo]
		slices.SortStableFunc(list, func(a, b Brake) int { return a.At.Compare(b.At) })
		err := e.command(cmdEvaluateDue, func(o *op) error {
			ids, err := o.release(p, repo, list)
			released = append(released, ids...)
			return err
		})
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return released, firstErr
}

func (o *op) release(p Policy, repo string, list []Brake) ([]string, error) {
	owed, err := o.owed(repo)
	if err != nil {
		return nil, err
	}
	running, err := o.evalsRunning(repo)
	if err != nil {
		return nil, err
	}
	room := p.WIPCeiling(repo) - owed - running
	var released []string
	for _, b := range list {
		if room <= 0 {
			break
		}
		tr, err := o.tx.GetTrigger(b.TriggerID)
		if err != nil {
			return nil, err
		}
		var th *Thread
		if tr != nil && len(tr.Threads) > 0 {
			if th, err = o.thread(tr.Threads[0]); err != nil {
				return nil, err
			}
		}
		if th == nil || (th.Eval != nil && slices.Contains(th.Eval.TriggerIDs, b.TriggerID)) {
			if err := o.tx.DeleteBrake(b.TriggerID); err != nil {
				return nil, err
			}
			continue
		}
		owners, err := followupOwners(tr, o.tx.GetTicket, th)
		if err != nil {
			return nil, err
		}
		if err := o.tx.DeleteBrake(b.TriggerID); err != nil {
			return nil, err
		}
		if len(owners) > 0 {
			continue // a Ticket's worker decides it now
		}
		o.origin = OriginRouter
		if err := o.requeue(tr, th); err != nil {
			return nil, err
		}
		released = append(released, b.TriggerID)
		room--
	}
	return released, nil
}

// evalsRunning counts the Evals in progress on Threads of a repository.
func (o *op) evalsRunning(repo string) (int, error) {
	threads, err := o.tx.ActiveThreads()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, th := range threads {
		if th.Eval != nil && strings.EqualFold(th.Meta["repo"], repo) {
			n++
		}
	}
	return n, nil
}

// HasBrakes tells whether any candidate is held by the brake: a cheap query that tells whether EvaluateDue has
// anything to release.
func (e *Engine) HasBrakes() (bool, error) {
	var held bool
	err := e.repos.Tx(func(tx Tx) error {
		brakes, err := tx.Brakes()
		held = len(brakes) > 0
		return err
	})
	return held, err
}
