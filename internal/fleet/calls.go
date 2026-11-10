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

// errNotOnNode refuses a node's call about a Ticket none of whose Assignments was placed on it.
func errNotOnNode(ticketID string) error {
	return &core.UserError{Msg: "ticket " + ticketID + " is not on this node"}
}

// onNode checks that the Ticket has an Assignment placed on the node: a node acts only on its own Tickets.
func onNode(eng *core.Engine, nodeID, ticketID string) error {
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
	guard := func(ticketID string) error { return onNode(eng, nodeID, ticketID) }
	switch a := a.(type) {
	case *nodelink.ArgsAssignmentStarted:
		if err := guard(a.TicketID); err != nil {
			return err
		}
		return eng.AssignmentStarted(a.TicketID, a.AssignmentID, a.SessionID, a.Model, a.Runtime)
	case *nodelink.ArgsInputsDelivered:
		if err := guard(a.TicketID); err != nil {
			return err
		}
		return eng.InputsDelivered(a.TicketID, a.AssignmentID, a.InputIDs)
	case *nodelink.ArgsDeliveryFailed:
		if err := guard(a.TicketID); err != nil {
			return err
		}
		return eng.DeliveryFailed(a.TicketID, a.AssignmentID, a.ErrMsg)
	case *nodelink.ArgsInputsRead:
		if err := guard(a.TicketID); err != nil {
			return err
		}
		return eng.InputsRead(a.TicketID, a.InputIDs)
	case *nodelink.ArgsTurnEnded:
		if err := guard(a.TicketID); err != nil {
			return err
		}
		return eng.TurnEnded(a.TicketID, a.AssignmentID)
	case *nodelink.ArgsAssignmentInterrupted:
		if err := guard(a.TicketID); err != nil {
			return err
		}
		return eng.AssignmentInterrupted(a.TicketID, a.AssignmentID, a.Reason)
	case *nodelink.ArgsAssignmentPaused:
		if err := guard(a.TicketID); err != nil {
			return err
		}
		return eng.AssignmentPaused(a.TicketID, a.AssignmentID, a.ReportOwed)
	case *nodelink.ArgsUsageLimited:
		if err := guard(a.TicketID); err != nil {
			return err
		}
		return eng.UsageLimited(a.TicketID, a.AssignmentID, a.Limit)
	case *nodelink.ArgsRecordUsage:
		if err := guard(a.TicketID); err != nil {
			return err
		}
		return eng.RecordUsage(a.TicketID, a.AssignmentID, a.Usage)
	case *nodelink.ArgsAllowedByRule:
		if err := guard(a.TicketID); err != nil {
			return err
		}
		return eng.AllowedByRule(a.TicketID, a.Tool, a.Rule, a.Repo)
	case *nodelink.ArgsExpireApproval:
		if err := approvalOnNode(eng, nodeID, a.ID); err != nil {
			return err
		}
		return eng.ExpireApproval(a.ID)
	case *nodelink.ArgsSettleProposal:
		if err := proposalOnNode(eng, nodeID, a.ID); err != nil {
			return err
		}
		var applyErr error
		if a.Error != "" {
			applyErr = errors.New(a.Error)
		}
		return eng.SettleProposal(a.ID, applyErr)
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
	guard := func(ticketID string) error { return onNode(eng, nodeID, ticketID) }
	switch a := a.(type) {
	case *nodelink.ArgsFileReport:
		if err := guard(a.TicketID); err != nil {
			return nil, err
		}
		return nil, eng.FileReport(a.TicketID, a.Report)
	case *nodelink.ArgsFileReview:
		if err := guard(a.TicketID); err != nil {
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
		if err := guard(a.Approval.TicketID); err != nil {
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
		return pendingProposalsOn(eng, nodeID)
	case *nodelink.ArgsStartProposal:
		if err := proposalOnNode(eng, nodeID, a.ID); err != nil {
			return nil, err
		}
		return eng.StartProposal(a.ID)

	case *nodelink.ArgsToken:
		if f.cfg.Creds == nil {
			return nil, &core.UserError{Msg: "the server has no GitHub credentials"}
		}
		if err := repoOnNode(eng, nodeID, a.Repo); err != nil {
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

// approvalOnNode checks that the Approval belongs to a Ticket placed on the node.
func approvalOnNode(eng *core.Engine, nodeID, id string) error {
	a, err := eng.Approval(id)
	if err != nil {
		return err
	}
	return onNode(eng, nodeID, a.TicketID)
}

// proposalOnNode checks that the Proposal belongs to a Ticket placed on the node.
func proposalOnNode(eng *core.Engine, nodeID, id string) error {
	p, err := eng.Proposal(id)
	if err != nil {
		return err
	}
	return onNode(eng, nodeID, p.TicketID)
}

// pendingProposalsOn lists the pending Proposals of the Tickets placed on the node.
func pendingProposalsOn(eng *core.Engine, nodeID string) ([]core.Proposal, error) {
	all, err := eng.PendingProposals()
	if err != nil {
		return nil, err
	}
	out := []core.Proposal{}
	on := map[string]bool{}
	for _, p := range all {
		ok, seen := on[p.TicketID]
		if !seen {
			ok = onNode(eng, nodeID, p.TicketID) == nil
			on[p.TicketID] = ok
		}
		if ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// triggerOnNode checks that the Trigger started, or reached as an Input, an open Ticket placed on the node.
func triggerOnNode(eng *core.Engine, nodeID, id string) error {
	tickets, err := eng.Tickets(false)
	if err != nil {
		return err
	}
	for _, t := range tickets {
		if !placedOn(t, nodeID) {
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
