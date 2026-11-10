package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/nodelink"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/sources/github"
	"github.com/danielpina1/athena/prompts"
)

// decodeStrict decodes a call's args, refusing unknown fields: a UserError, since the same args would fail again.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &core.UserError{Msg: "bad arguments: " + err.Error()}
	}
	return nil
}

// args decodes a node→server call's args into its nodelink.Args struct.
func args(call *athenav1.NodeCall) (any, error) {
	a, ok := nodelink.NewArgs(call.GetMethod())
	if !ok || call.GetMethod() == nodelink.MethodLines {
		return nil, &core.UserError{Msg: fmt.Sprintf("unknown call %q", call.GetMethod())}
	}
	if err := decodeStrict(call.GetArgs(), a); err != nil {
		return nil, err
	}
	return a, nil
}

// errNotOnNode refuses a node's call about a Ticket that is not (or, for a late fact, never was) on it.
func errNotOnNode(ticketID string) error {
	return &core.UserError{Msg: "ticket " + ticketID + " is not on this node"}
}

// A node acts only on its own work. Three scopes, narrowest first:
//
//   - assignmentOn: a call naming an Assignment needs that Assignment placed on the node (it may have ended: a late
//     fact about the node's own past Assignment is the Engine's to judge);
//   - currentOn: a call about the Ticket as a whole needs the Ticket's latest Assignment on the node and not ended,
//     the rule repoOnNode and latestOn use, so a node a Ticket moved away from cannot act on it;
//   - pastOn: a late fact that names no Assignment (InputsRead, AllowedByRule) needs one of the Ticket's Assignments
//     to have been placed on the node.

// assignmentOn checks that the Ticket's Assignment assignmentID is placed on the node; with no assignmentID, that the
// Ticket is currently on it.
func assignmentOn(eng *core.Engine, nodeID, ticketID, assignmentID string) error {
	if assignmentID == "" {
		return currentOn(eng, nodeID, ticketID)
	}
	t, _, err := eng.Ticket(ticketID)
	if err != nil {
		return err
	}
	for _, a := range t.Assignments {
		if a.ID == assignmentID {
			if a.Node == nodeID {
				return nil
			}
			break
		}
	}
	return &core.UserError{Msg: "assignment " + assignmentID + " of ticket " + ticketID + " is not on this node"}
}

// currentOn checks that the Ticket's latest Assignment is placed on the node and not ended.
func currentOn(eng *core.Engine, nodeID, ticketID string) error {
	t, _, err := eng.Ticket(ticketID)
	if err != nil {
		return err
	}
	if a := t.LatestAssignment(); a == nil || a.Node != nodeID || a.State == core.AssignmentEnded {
		return errNotOnNode(ticketID)
	}
	return nil
}

// pastOn checks that one of the Ticket's Assignments was placed on the node.
func pastOn(eng *core.Engine, nodeID, ticketID string) error {
	t, _, err := eng.Ticket(ticketID)
	if err != nil {
		return err
	}
	if !placedOn(t, nodeID) {
		return errNotOnNode(ticketID)
	}
	return nil
}

// fact applies one of the node's facts to the Engine.
func (f *Fleet) fact(nodeID string, call *athenav1.NodeCall) error {
	if !nodelink.IsFact(call.GetMethod()) {
		return &core.UserError{Msg: fmt.Sprintf("%q is not a fact", call.GetMethod())}
	}
	a, err := args(call)
	if err != nil {
		return err
	}
	eng := f.engine()
	own := func(ticketID, assignmentID string) error { return assignmentOn(eng, nodeID, ticketID, assignmentID) }
	switch a := a.(type) {
	case *nodelink.ArgsAssignmentStarted:
		if err := own(a.TicketID, a.AssignmentID); err != nil {
			return err
		}
		return eng.AssignmentStarted(a.TicketID, a.AssignmentID, a.SessionID, a.Model, a.Runtime)
	case *nodelink.ArgsInputsDelivered:
		if err := own(a.TicketID, a.AssignmentID); err != nil {
			return err
		}
		return eng.InputsDelivered(a.TicketID, a.AssignmentID, a.InputIDs)
	case *nodelink.ArgsDeliveryFailed:
		if err := own(a.TicketID, a.AssignmentID); err != nil {
			return err
		}
		return eng.DeliveryFailed(a.TicketID, a.AssignmentID, a.ErrMsg)
	case *nodelink.ArgsInputsRead:
		if err := pastOn(eng, nodeID, a.TicketID); err != nil {
			return err
		}
		return eng.InputsRead(a.TicketID, a.InputIDs)
	case *nodelink.ArgsTurnEnded:
		if err := own(a.TicketID, a.AssignmentID); err != nil {
			return err
		}
		return eng.TurnEnded(a.TicketID, a.AssignmentID)
	case *nodelink.ArgsAssignmentInterrupted:
		if err := own(a.TicketID, a.AssignmentID); err != nil {
			return err
		}
		return eng.AssignmentInterrupted(a.TicketID, a.AssignmentID, a.Reason)
	case *nodelink.ArgsAssignmentPaused:
		if err := own(a.TicketID, a.AssignmentID); err != nil {
			return err
		}
		return eng.AssignmentPaused(a.TicketID, a.AssignmentID, a.ReportOwed)
	case *nodelink.ArgsUsageLimited:
		if err := own(a.TicketID, a.AssignmentID); err != nil {
			return err
		}
		return eng.UsageLimited(a.TicketID, a.AssignmentID, a.Limit)
	case *nodelink.ArgsRecordUsage:
		if err := own(a.TicketID, a.AssignmentID); err != nil {
			return err
		}
		return eng.RecordUsage(a.TicketID, a.AssignmentID, a.Usage)
	case *nodelink.ArgsAllowedByRule:
		if err := pastOn(eng, nodeID, a.TicketID); err != nil {
			return err
		}
		return eng.AllowedByRule(a.TicketID, a.Tool, a.Rule, a.Repo)
	case *nodelink.ArgsExpireApproval:
		if err := approvalOnNode(eng, nodeID, a.ID); err != nil {
			return err
		}
		return eng.ExpireApproval(a.ID)
	case *nodelink.ArgsCheckpointSent:
		return f.checkpointSent(eng, nodeID, a.TicketID, a.Repo, a.SHA, nil)
	case *nodelink.ArgsSettleProposal:
		if err := f.settleOnNode(eng, nodeID, a.ID); err != nil {
			return err
		}
		var applyErr error
		if a.Error != "" {
			applyErr = errors.New(a.Error)
		}
		if err := eng.SettleProposal(a.ID, applyErr); err != nil {
			return err
		}
		f.settled(a.ID)
		return nil
	}
	return &core.UserError{Msg: fmt.Sprintf("unknown fact %q", call.GetMethod())}
}

// request answers one of the node's requests: the JSON of its result, or an error.
func (f *Fleet) request(ctx context.Context, nodeID string, call *athenav1.NodeCall) ([]byte, error) {
	if nodelink.IsFact(call.GetMethod()) {
		return nil, &core.UserError{Msg: fmt.Sprintf("%q is a fact, sent without a seq", call.GetMethod())}
	}
	a, err := args(call)
	if err != nil {
		return nil, err
	}
	v, err := f.answer(ctx, nodeID, a)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

func (f *Fleet) answer(ctx context.Context, nodeID string, a any) (any, error) {
	eng := f.engine()
	guard := func(ticketID string) error { return currentOn(eng, nodeID, ticketID) }
	switch a := a.(type) {
	case *nodelink.ArgsFileReport:
		if err := assignmentOn(eng, nodeID, a.TicketID, a.Report.AssignmentID); err != nil {
			return nil, err
		}
		return nil, eng.FileReport(a.TicketID, a.Report)
	case *nodelink.ArgsFileReview:
		if err := assignmentOn(eng, nodeID, a.TicketID, a.AssignmentID); err != nil {
			return nil, err
		}
		return nil, eng.FileReview(a.TicketID, a.AssignmentID, a.Verdict, a.Findings)
	case *nodelink.ArgsTicket:
		t, st, err := eng.Ticket(a.ID)
		if err != nil {
			return nil, err
		}
		if !placedOn(t, nodeID) {
			return nil, errNotOnNode(a.ID)
		}
		return nodelink.ResultTicket{Ticket: t, Stage: st}, nil
	case *nodelink.ArgsRequestApproval:
		if err := assignmentOn(eng, nodeID, a.Approval.TicketID, a.Approval.AssignmentID); err != nil {
			return nil, err
		}
		return eng.RequestApproval(a.Approval)
	case *nodelink.ArgsWaitApproval:
		if err := approvalOnNode(eng, nodeID, a.ID); err != nil {
			return nil, err
		}
		return waitApproval(ctx, eng, a.ID, time.Duration(a.WaitMs)*time.Millisecond)
	case *nodelink.ArgsTrigger:
		if err := triggerOnNode(eng, nodeID, a.ID); err != nil {
			return nil, err
		}
		return eng.Trigger(a.ID)
	case *nodelink.ArgsReplyOnce:
		if err := guard(a.Request.TicketID); err != nil {
			return nil, err
		}
		return eng.ReplyOnce(a.Request)
	case *nodelink.ArgsFollowForContext:
		if err := guard(a.TicketID); err != nil {
			return nil, err
		}
		return nil, eng.FollowForContext(a.TicketID, a.Thread)
	case *nodelink.ArgsMarkUnrelated:
		if err := guard(a.TicketID); err != nil {
			return nil, err
		}
		return nil, eng.MarkUnrelated(a.TicketID, a.Thread, a.Reason)
	case *nodelink.ArgsHandback:
		if err := guard(a.TicketID); err != nil {
			return nil, err
		}
		return eng.Handback(a.TicketID, a.TriggerID, a.Reason)
	case *nodelink.ArgsBindThread:
		if err := guard(a.TicketID); err != nil {
			return nil, err
		}
		return nil, eng.BindThread(a.TicketID, a.Thread, a.Own)
	case *nodelink.ArgsPropose:
		if err := guard(a.Proposal.TicketID); err != nil {
			return nil, err
		}
		return eng.Propose(a.Proposal)
	case *nodelink.ArgsPendingProposals:
		return pendingProposalsOn(eng, nodeID, func(ticketID string) bool { return f.applyingOn(nodeID, ticketID) })
	case *nodelink.ArgsStartProposal:
		if err := f.proposalOnNode(eng, nodeID, a.ID); err != nil {
			return nil, err
		}
		first, err := eng.StartProposal(a.ID)
		if err == nil {
			f.startedOn(a.ID, nodeID)
		}
		return first, err

	case *nodelink.ArgsToken:
		if f.cfg.Creds == nil {
			return nil, &core.UserError{Msg: "the server has no GitHub credentials"}
		}
		if err := repoOnNode(eng, nodeID, a.Repo); err != nil && !f.applyingRepo(nodeID, a.Repo) {
			return nil, err
		}
		tctx, cancel := context.WithTimeout(ctx, RequestTimeout)
		defer cancel()
		tok, exp, err := f.cfg.Creds.Token(tctx, a.Repo, a.Write)
		if err != nil {
			if errors.Is(err, github.ErrPermanent) || errors.Is(err, github.ErrGone) {
				// The node fails the launch instead of waiting for a token it will never get.
				return nil, &core.UserError{Msg: err.Error(), Err: err}
			}
			return nil, err
		}
		return nodelink.ResultToken{Token: tok, Expires: exp}, nil
	case *nodelink.ArgsRevoke:
		rv, ok := f.cfg.Creds.(node.TokenRevoker)
		if !ok {
			return nil, nil
		}
		tctx, cancel := context.WithTimeout(ctx, RequestTimeout)
		defer cancel()
		return nil, rv.Revoke(tctx, a.Token)
	case *nodelink.ArgsIdentity:
		var r nodelink.ResultIdentity
		if f.cfg.Creds != nil {
			r.Login, r.Email = f.cfg.Creds.Identity()
		}
		return r, nil

	// Without a Policy the server runs every session standard, as the local node does.
	case *nodelink.ArgsToolPolicy:
		if f.cfg.Policy == nil {
			return policy.ToolStandard, nil
		}
		return f.cfg.Policy.ToolPolicy(a.Class), nil
	case *nodelink.ArgsAllows:
		return f.cfg.Policy != nil && f.cfg.Policy.Allows(a.Tool, a.Rule, a.Repo), nil
	case *nodelink.ArgsShadow:
		return f.cfg.Policy != nil && f.cfg.Policy.Shadow(a.Repo), nil
	case *nodelink.ArgsTrust:
		if f.cfg.Policy == nil {
			return "", nil
		}
		return f.cfg.Policy.Trust(a.Trigger), nil

	case *nodelink.ArgsPrompts:
		w, r := prompts.Worker, prompts.Reviewer
		if f.cfg.Prompts != nil {
			w, r = f.cfg.Prompts()
		}
		return nodelink.ResultPrompts{Worker: w, Reviewer: r}, nil
	}
	return nil, &core.UserError{Msg: "unknown request"}
}

// placedOn reports whether one of the Ticket's Assignments was placed on the node.
func placedOn(t core.Ticket, nodeID string) bool {
	for _, a := range t.Assignments {
		if a.Node == nodeID {
			return true
		}
	}
	return false
}

// waitApproval waits up to wait for the Approval to be answered and returns it, pending if it was not.
func waitApproval(ctx context.Context, eng *core.Engine, id string, wait time.Duration) (core.Approval, error) {
	if wait > 0 {
		wctx, cancel := context.WithTimeout(ctx, wait)
		a, err := eng.WaitApproval(wctx, id)
		cancel()
		if err == nil || (!errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled)) {
			return a, err
		}
		if ctx.Err() != nil {
			return core.Approval{}, ctx.Err()
		}
	}
	// Look without waiting: the pending Approval, or the answered one.
	pending, err := eng.PendingApprovals()
	if err != nil {
		return core.Approval{}, err
	}
	for _, a := range pending {
		if a.ID == id {
			return a, nil
		}
	}
	lctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return eng.WaitApproval(lctx, id)
}

// approvalOnNode checks that the Approval is the node's: its Assignment on the node, or, naming none, its Ticket
// currently on the node.
func approvalOnNode(eng *core.Engine, nodeID, id string) error {
	a, err := eng.Approval(id)
	if err != nil {
		return err
	}
	return assignmentOn(eng, nodeID, a.TicketID, a.AssignmentID)
}

// proposalOnNode checks that the Proposal belongs to a Ticket currently on the node, or to one an apply the fleet
// sent the node is under way for (an owner-approved apply of a Ticket that ended or moved).
func (f *Fleet) proposalOnNode(eng *core.Engine, nodeID, id string) error {
	p, err := eng.Proposal(id)
	if err != nil {
		return err
	}
	if f.applyingOn(nodeID, p.TicketID) {
		return nil
	}
	return currentOn(eng, nodeID, p.TicketID)
}

// settleOnNode checks a SettleProposal, a late fact: the node StartProposal was granted to settles it, and no other.
// Without a record of that (the server restarted since), a node under an apply of the Ticket, or one the Ticket was
// ever placed on, may.
func (f *Fleet) settleOnNode(eng *core.Engine, nodeID, id string) error {
	p, err := eng.Proposal(id)
	if err != nil {
		return err
	}
	f.amu.Lock()
	by, ok := f.started[id]
	f.amu.Unlock()
	if ok {
		if by != nodeID {
			return errNotOnNode(p.TicketID)
		}
		return nil
	}
	if f.applyingOn(nodeID, p.TicketID) {
		return nil
	}
	return pastOn(eng, nodeID, p.TicketID)
}

// applyGrant lets a node act on one held Proposal while the fleet's apply of it is out to the node: start it (and the
// Ticket's pending push a pull request applies first), get a write token for its repository, and settle it.
type applyGrant struct {
	node, ticketID, repo string
}

// grantApply records the grant of p's apply on the node; the returned func clears it.
func (f *Fleet) grantApply(nodeID string, p core.Proposal) func() {
	f.amu.Lock()
	f.applying[p.ID] = applyGrant{node: nodeID, ticketID: p.TicketID, repo: p.Repo}
	f.amu.Unlock()
	return func() {
		f.amu.Lock()
		delete(f.applying, p.ID)
		delete(f.started, p.ID) // the server settles the applied Proposal itself
		f.amu.Unlock()
	}
}

// applyingOn reports whether an apply of one of the Ticket's Proposals is out to the node.
func (f *Fleet) applyingOn(nodeID, ticketID string) bool {
	f.amu.Lock()
	defer f.amu.Unlock()
	for _, g := range f.applying {
		if g.node == nodeID && g.ticketID == ticketID {
			return true
		}
	}
	return false
}

// applyingRepo reports whether an apply of a Proposal of repo is out to the node.
func (f *Fleet) applyingRepo(nodeID, repo string) bool {
	f.amu.Lock()
	defer f.amu.Unlock()
	for _, g := range f.applying {
		if g.node == nodeID && g.repo != "" && strings.EqualFold(g.repo, repo) {
			return true
		}
	}
	return false
}

// startedOn records that StartProposal of id was granted to the node.
func (f *Fleet) startedOn(id, nodeID string) {
	f.amu.Lock()
	f.started[id] = nodeID
	f.amu.Unlock()
}

// settled forgets who started id.
func (f *Fleet) settled(id string) {
	f.amu.Lock()
	delete(f.started, id)
	f.amu.Unlock()
}

// pendingProposalsOn lists the pending Proposals of the Tickets currently on the node, or applying on it.
func pendingProposalsOn(eng *core.Engine, nodeID string, applying func(ticketID string) bool) ([]core.Proposal, error) {
	all, err := eng.PendingProposals()
	if err != nil {
		return nil, err
	}
	out := []core.Proposal{}
	on := map[string]bool{}
	for _, p := range all {
		ok, seen := on[p.TicketID]
		if !seen {
			ok = currentOn(eng, nodeID, p.TicketID) == nil || applying(p.TicketID)
			on[p.TicketID] = ok
		}
		if ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// triggerOnNode checks that the Trigger started, or reached as an Input, an open Ticket currently on the node.
func triggerOnNode(eng *core.Engine, nodeID, id string) error {
	tickets, err := eng.Tickets(false)
	if err != nil {
		return err
	}
	for _, t := range tickets {
		if a := t.LatestAssignment(); a == nil || a.Node != nodeID || a.State == core.AssignmentEnded {
			continue
		}
		if t.Origin.TriggerID == id || slices.Contains(t.Origin.TriggerIDs, id) {
			return nil
		}
		for _, in := range t.Inputs {
			if in.TriggerID == id {
				return nil
			}
		}
	}
	return &core.UserError{Msg: "trigger " + id + " is not on this node"}
}

// repoOnNode checks that the node runs work in the repository: an open Ticket of that Repo whose latest Assignment is
// placed on the node and not ended. A node gets installation tokens only for its own Assignments.
func repoOnNode(eng *core.Engine, nodeID, repo string) error {
	tickets, err := eng.Tickets(false)
	if err != nil {
		return err
	}
	for _, t := range tickets {
		a := t.LatestAssignment()
		if a != nil && a.Node == nodeID && a.State != core.AssignmentEnded && t.Repo != "" && strings.EqualFold(t.Repo, repo) {
			return nil
		}
	}
	return &core.UserError{Msg: "this node has no work in " + repo}
}
