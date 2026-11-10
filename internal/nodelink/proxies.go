package nodelink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/sources/github"
	"github.com/danielpina1/athena/prompts"
)

// The node's calls to the server, by NodeCall.method. A fact (IsFact) goes through the outbox: it carries its
// fact_seq and the server applies it once, in seq order, then answers it with an empty NodeResult. A request carries
// fact_seq 0 and its NodeResult is the answer. Each one's args are the JSON of its Args struct, field names as in Go.
const (
	// Facts.
	MethodAssignmentStarted     = "assignmentStarted"
	MethodInputsDelivered       = "inputsDelivered"
	MethodDeliveryFailed        = "deliveryFailed"
	MethodInputsRead            = "inputsRead"
	MethodTurnEnded             = "turnEnded"
	MethodAssignmentInterrupted = "assignmentInterrupted"
	MethodAssignmentPaused      = "assignmentPaused"
	MethodUsageLimited          = "usageLimited"
	MethodRecordUsage           = "recordUsage"
	MethodAllowedByRule         = "allowedByRule"
	MethodExpireApproval        = "expireApproval"
	MethodSettleProposal        = "settleProposal"
	// MethodCheckpointSent follows the BlobChunks of a checkpoint bundle: the server has the Ticket's checkpoint at SHA.
	MethodCheckpointSent = "checkpointSent"

	// Requests of EngineCallbacks.
	MethodFileReport       = "fileReport"
	MethodFileReview       = "fileReview"
	MethodTicket           = "ticket"
	MethodRequestApproval  = "requestApproval"
	MethodWaitApproval     = "waitApproval"
	MethodTrigger          = "trigger"
	MethodReplyOnce        = "replyOnce"
	MethodFollowForContext = "followForContext"
	MethodMarkUnrelated    = "markUnrelated"
	MethodHandback         = "handback"
	MethodBindThread       = "bindThread"
	MethodPropose          = "propose"
	MethodPendingProposals = "pendingProposals"
	MethodStartProposal    = "startProposal"
	// Requests of Credentials (and TokenRevoker).
	MethodToken    = "token"
	MethodRevoke   = "revoke"
	MethodIdentity = "identity"
	// Requests of Policy.
	MethodToolPolicy = "toolPolicy"
	MethodAllows     = "allows"
	MethodShadow     = "shadow"
	MethodTrust      = "trust"
	// MethodPrompts asks for the system prompts new sessions start with.
	MethodPrompts = "prompts"

	// MethodLines is no call but a notification on STREAM (id 0, fact_seq 0, not answered): transcript lines of a
	// subscription (ServeSubscribe), as ArgsLines.
	MethodLines = "lines"
)

// methods maps each node→server method to a new zero value of its args, and whether it is a fact.
var methods = map[string]struct {
	fact bool
	args func() any
}{
	MethodAssignmentStarted:     {true, func() any { return new(ArgsAssignmentStarted) }},
	MethodInputsDelivered:       {true, func() any { return new(ArgsInputsDelivered) }},
	MethodDeliveryFailed:        {true, func() any { return new(ArgsDeliveryFailed) }},
	MethodInputsRead:            {true, func() any { return new(ArgsInputsRead) }},
	MethodTurnEnded:             {true, func() any { return new(ArgsTurnEnded) }},
	MethodAssignmentInterrupted: {true, func() any { return new(ArgsAssignmentInterrupted) }},
	MethodAssignmentPaused:      {true, func() any { return new(ArgsAssignmentPaused) }},
	MethodUsageLimited:          {true, func() any { return new(ArgsUsageLimited) }},
	MethodRecordUsage:           {true, func() any { return new(ArgsRecordUsage) }},
	MethodAllowedByRule:         {true, func() any { return new(ArgsAllowedByRule) }},
	MethodExpireApproval:        {true, func() any { return new(ArgsExpireApproval) }},
	MethodSettleProposal:        {true, func() any { return new(ArgsSettleProposal) }},
	MethodCheckpointSent:        {true, func() any { return new(ArgsCheckpointSent) }},
	MethodFileReport:            {false, func() any { return new(ArgsFileReport) }},
	MethodFileReview:            {false, func() any { return new(ArgsFileReview) }},
	MethodTicket:                {false, func() any { return new(ArgsTicket) }},
	MethodRequestApproval:       {false, func() any { return new(ArgsRequestApproval) }},
	MethodWaitApproval:          {false, func() any { return new(ArgsWaitApproval) }},
	MethodTrigger:               {false, func() any { return new(ArgsTrigger) }},
	MethodReplyOnce:             {false, func() any { return new(ArgsReplyOnce) }},
	MethodFollowForContext:      {false, func() any { return new(ArgsFollowForContext) }},
	MethodMarkUnrelated:         {false, func() any { return new(ArgsMarkUnrelated) }},
	MethodHandback:              {false, func() any { return new(ArgsHandback) }},
	MethodBindThread:            {false, func() any { return new(ArgsBindThread) }},
	MethodPropose:               {false, func() any { return new(ArgsPropose) }},
	MethodPendingProposals:      {false, func() any { return new(ArgsPendingProposals) }},
	MethodStartProposal:         {false, func() any { return new(ArgsStartProposal) }},
	MethodToken:                 {false, func() any { return new(ArgsToken) }},
	MethodRevoke:                {false, func() any { return new(ArgsRevoke) }},
	MethodIdentity:              {false, func() any { return new(ArgsIdentity) }},
	MethodToolPolicy:            {false, func() any { return new(ArgsToolPolicy) }},
	MethodAllows:                {false, func() any { return new(ArgsAllows) }},
	MethodShadow:                {false, func() any { return new(ArgsShadow) }},
	MethodTrust:                 {false, func() any { return new(ArgsTrust) }},
	MethodPrompts:               {false, func() any { return new(ArgsPrompts) }},
	MethodLines:                 {false, func() any { return new(ArgsLines) }},
}

// IsFact reports whether method is one of the node's facts.
func IsFact(method string) bool { return methods[method].fact }

// NewArgs returns a pointer to a zero Args struct for a node→server method, to decode its args into; ok is false for
// a method this package does not define.
func NewArgs(method string) (args any, ok bool) {
	m, ok := methods[method]
	if !ok {
		return nil, false
	}
	return m.args(), true
}

// Methods lists the node→server methods.
func Methods() []string {
	out := make([]string, 0, len(methods))
	for m := range methods {
		out = append(out, m)
	}
	return out
}

// Args of the facts.
type (
	ArgsAssignmentStarted struct{ TicketID, AssignmentID, SessionID, Model, Runtime string }
	ArgsInputsDelivered   struct {
		TicketID, AssignmentID string
		InputIDs               []string
	}
	ArgsDeliveryFailed struct{ TicketID, AssignmentID, ErrMsg string }
	ArgsInputsRead     struct {
		TicketID string
		InputIDs []string
	}
	ArgsTurnEnded             struct{ TicketID, AssignmentID string }
	ArgsAssignmentInterrupted struct{ TicketID, AssignmentID, Reason string }
	ArgsAssignmentPaused      struct {
		TicketID, AssignmentID string
		ReportOwed             bool
	}
	ArgsUsageLimited struct {
		TicketID, AssignmentID string
		Limit                  core.UsageLimit
	}
	ArgsRecordUsage struct {
		TicketID, AssignmentID string
		Usage                  core.UsageDelta
	}
	ArgsAllowedByRule  struct{ TicketID, Tool, Rule, Repo string }
	ArgsExpireApproval struct{ ID string }
	// ArgsSettleProposal carries applyErr as its message; "" is nil.
	ArgsSettleProposal struct{ ID, Error string }
	// ArgsCheckpointSent says the bundle of the Ticket's checkpoint at SHA went before it, on BLOB (checkpoint.go).
	ArgsCheckpointSent struct{ TicketID, Repo, SHA string }
)

// Args and results of the requests.
type (
	ArgsFileReport struct {
		TicketID string
		Report   core.ReportInput
	}
	ArgsFileReview struct{ TicketID, AssignmentID, Verdict, Findings string }
	ArgsTicket     struct{ ID string }
	// ResultTicket answers MethodTicket.
	ResultTicket struct {
		Ticket core.Ticket
		Stage  core.Stage
	}
	// ArgsRequestApproval answers with the stored core.Approval.
	ArgsRequestApproval struct{ Approval core.Approval }
	// ArgsWaitApproval waits up to WaitMs for the Approval to be answered (0: look without waiting) and answers with the
	// core.Approval, pending if it was not.
	ArgsWaitApproval struct {
		ID     string
		WaitMs int64
	}
	// ArgsTrigger answers with the core.Trigger.
	ArgsTrigger struct{ ID string }
	// ArgsReplyOnce answers with the core.ReplyOutcome.
	ArgsReplyOnce        struct{ Request core.ReplyRequest }
	ArgsFollowForContext struct{ TicketID, Thread string }
	ArgsMarkUnrelated    struct{ TicketID, Thread, Reason string }
	// ArgsHandback answers with a JSON bool.
	ArgsHandback   struct{ TicketID, TriggerID, Reason string }
	ArgsBindThread struct {
		TicketID, Thread string
		Own              bool
	}
	// ArgsPropose answers with the stored core.Proposal.
	ArgsPropose struct{ Proposal core.Proposal }
	// ArgsPendingProposals answers with a JSON []core.Proposal.
	ArgsPendingProposals struct{}
	// ArgsStartProposal answers with a JSON bool (first).
	ArgsStartProposal struct{ ID string }

	// ArgsToken answers with ResultToken. A refusal the same request will not get past (github.ErrPermanent or ErrGone
	// on the server) is answered with user_error set.
	ArgsToken struct {
		Repo  string
		Write bool
	}
	ResultToken struct {
		Token   string
		Expires time.Time
	}
	ArgsRevoke struct{ Token string }
	// ArgsIdentity answers with ResultIdentity.
	ArgsIdentity   struct{}
	ResultIdentity struct{ Login, Email string }

	// ArgsToolPolicy answers with a JSON string.
	ArgsToolPolicy struct{ Class string }
	// ArgsAllows answers with a JSON bool.
	ArgsAllows struct{ Tool, Rule, Repo string }
	// ArgsShadow answers with a JSON bool.
	ArgsShadow struct{ Repo string }
	// ArgsTrust answers with a JSON string.
	ArgsTrust struct{ Trigger core.Trigger }

	// ArgsPrompts answers with ResultPrompts.
	ArgsPrompts   struct{}
	ResultPrompts struct{ Worker, Reviewer string }

	// ArgsLines are transcript lines of the subscription Sub (the id of its ServeSubscribe call), in order. Closed ends
	// the subscription: the node has no more for it.
	ArgsLines struct {
		Sub    uint64
		Lines  []node.Line
		Closed bool
	}
)

// engineProxy is node.EngineCallbacks over the link.
type engineProxy struct{ c *Client }

var _ node.EngineCallbacks = engineProxy{}

func (p engineProxy) AssignmentStarted(ticketID, assignmentID, sessionID, model, runtime string) error {
	return p.c.fact(MethodAssignmentStarted, ArgsAssignmentStarted{ticketID, assignmentID, sessionID, model, runtime})
}

func (p engineProxy) InputsDelivered(ticketID, assignmentID string, inputIDs []string) error {
	return p.c.fact(MethodInputsDelivered, ArgsInputsDelivered{ticketID, assignmentID, inputIDs})
}

func (p engineProxy) DeliveryFailed(ticketID, assignmentID, errMsg string) error {
	return p.c.fact(MethodDeliveryFailed, ArgsDeliveryFailed{ticketID, assignmentID, errMsg})
}

func (p engineProxy) InputsRead(ticketID string, inputIDs []string) error {
	return p.c.fact(MethodInputsRead, ArgsInputsRead{ticketID, inputIDs})
}

func (p engineProxy) TurnEnded(ticketID, assignmentID string) error {
	return p.c.fact(MethodTurnEnded, ArgsTurnEnded{ticketID, assignmentID})
}

func (p engineProxy) AssignmentInterrupted(ticketID, assignmentID, reason string) error {
	return p.c.fact(MethodAssignmentInterrupted, ArgsAssignmentInterrupted{ticketID, assignmentID, reason})
}

func (p engineProxy) AssignmentPaused(ticketID, assignmentID string, reportOwed bool) error {
	return p.c.fact(MethodAssignmentPaused, ArgsAssignmentPaused{ticketID, assignmentID, reportOwed})
}

func (p engineProxy) UsageLimited(ticketID, assignmentID string, l core.UsageLimit) error {
	return p.c.fact(MethodUsageLimited, ArgsUsageLimited{ticketID, assignmentID, l})
}

func (p engineProxy) RecordUsage(ticketID, assignmentID string, u core.UsageDelta) error {
	return p.c.fact(MethodRecordUsage, ArgsRecordUsage{ticketID, assignmentID, u})
}

func (p engineProxy) AllowedByRule(ticketID, tool, rule, repo string) error {
	return p.c.fact(MethodAllowedByRule, ArgsAllowedByRule{ticketID, tool, rule, repo})
}

func (p engineProxy) ExpireApproval(id string) error {
	return p.c.fact(MethodExpireApproval, ArgsExpireApproval{id})
}

func (p engineProxy) SettleProposal(id string, applyErr error) error {
	a := ArgsSettleProposal{ID: id}
	if applyErr != nil {
		a.Error = applyErr.Error()
	}
	return p.c.fact(MethodSettleProposal, a)
}

func (p engineProxy) FileReport(ticketID string, r core.ReportInput) error {
	return p.c.request(context.Background(), MethodFileReport, ArgsFileReport{ticketID, r}, nil)
}

func (p engineProxy) FileReview(ticketID, assignmentID, verdict, findings string) error {
	return p.c.request(context.Background(), MethodFileReview, ArgsFileReview{ticketID, assignmentID, verdict, findings}, nil)
}

// Ticket asks the server; offline, it answers from the last snapshot the server sent with a Start, Deliver or Stop.
func (p engineProxy) Ticket(id string) (core.Ticket, core.Stage, error) {
	var r ResultTicket
	err := p.c.request(context.Background(), MethodTicket, ArgsTicket{id}, &r)
	if err == nil {
		p.c.cacheTicket(r.Ticket)
		return r.Ticket, r.Stage, nil
	}
	if errors.Is(err, ErrOffline) {
		if t, ok := p.c.cachedTicket(id); ok {
			return t, core.TicketStage(&t), nil
		}
	}
	return core.Ticket{}, core.Stage{}, err
}

func (p engineProxy) RequestApproval(a core.Approval) (core.Approval, error) {
	var out core.Approval
	err := p.c.request(context.Background(), MethodRequestApproval, ArgsRequestApproval{a}, &out)
	return out, err
}

// WaitApproval waits on the server as long as ctx allows (a ctx already done looks without waiting). A link that
// drops during the wait is waited for, and the wait asked again, until ctx ends; offline at the start is ErrOffline.
func (p engineProxy) WaitApproval(ctx context.Context, id string) (core.Approval, error) {
	for {
		wait := int64(0)
		if ctx.Err() == nil {
			wait = maxApprovalWait.Milliseconds()
			if dl, ok := ctx.Deadline(); ok {
				wait = min(wait, max(time.Until(dl).Milliseconds(), 0))
			}
		}
		rctx := context.WithoutCancel(ctx)
		if wait > 0 {
			rctx = ctx
		}
		var out core.Approval
		err := p.c.requestTimeout(rctx, MethodWaitApproval, ArgsWaitApproval{id, wait},
			&out, time.Duration(wait)*time.Millisecond+p.c.opts.RequestTimeout)
		if !errors.Is(err, errLinkLost) {
			return out, err
		}
		if wait == 0 || !p.c.waitOnline(ctx) {
			if ctx.Err() != nil {
				return core.Approval{}, ctx.Err()
			}
			return core.Approval{}, ErrOffline
		}
	}
}

// maxApprovalWait bounds one wait the server holds for an Approval with no deadline.
const maxApprovalWait = time.Hour

func (p engineProxy) Trigger(id string) (core.Trigger, error) {
	var out core.Trigger
	err := p.c.request(context.Background(), MethodTrigger, ArgsTrigger{id}, &out)
	return out, err
}

func (p engineProxy) ReplyOnce(r core.ReplyRequest) (core.ReplyOutcome, error) {
	var out core.ReplyOutcome
	err := p.c.request(context.Background(), MethodReplyOnce, ArgsReplyOnce{r}, &out)
	return out, err
}

func (p engineProxy) FollowForContext(ticketID, thread string) error {
	return p.c.request(context.Background(), MethodFollowForContext, ArgsFollowForContext{ticketID, thread}, nil)
}

func (p engineProxy) MarkUnrelated(ticketID, thread, reason string) error {
	return p.c.request(context.Background(), MethodMarkUnrelated, ArgsMarkUnrelated{ticketID, thread, reason}, nil)
}

func (p engineProxy) Handback(ticketID, triggerID, reason string) (bool, error) {
	var out bool
	err := p.c.request(context.Background(), MethodHandback, ArgsHandback{ticketID, triggerID, reason}, &out)
	return out, err
}

func (p engineProxy) BindThread(ticketID, thread string, own bool) error {
	return p.c.request(context.Background(), MethodBindThread, ArgsBindThread{ticketID, thread, own}, nil)
}

func (p engineProxy) Propose(pr core.Proposal) (core.Proposal, error) {
	var out core.Proposal
	err := p.c.request(context.Background(), MethodPropose, ArgsPropose{pr}, &out)
	return out, err
}

func (p engineProxy) PendingProposals() ([]core.Proposal, error) {
	var out []core.Proposal
	err := p.c.request(context.Background(), MethodPendingProposals, ArgsPendingProposals{}, &out)
	return out, err
}

func (p engineProxy) StartProposal(id string) (bool, error) {
	var out bool
	err := p.c.request(context.Background(), MethodStartProposal, ArgsStartProposal{id}, &out)
	return out, err
}

// credsProxy is node.Credentials and node.TokenRevoker over the link. The token crosses the link (it is the
// Assignment's own, short-lived) and is never logged.
type credsProxy struct{ c *Client }

var (
	_ node.Credentials  = credsProxy{}
	_ node.TokenRevoker = credsProxy{}
)

func (p credsProxy) Token(ctx context.Context, repo string, write bool) (string, time.Time, error) {
	var r ResultToken
	err := p.c.request(ctx, MethodToken, ArgsToken{repo, write}, &r)
	if ue := (*core.UserError)(nil); errors.As(err, &ue) {
		// The server's refusal is permanent: the launch fails instead of waiting for it.
		err = fmt.Errorf("%w: %s", github.ErrPermanent, ue.Msg)
	}
	return r.Token, r.Expires, err
}

// isPermanent reports whether a Token error is one a launch does not wait for.
func isPermanent(err error) bool { return errors.Is(err, github.ErrPermanent) }

func (p credsProxy) Revoke(ctx context.Context, token string) error {
	return p.c.request(ctx, MethodRevoke, ArgsRevoke{token}, nil)
}

// Identity asks the server for the bot's commit identity; unreachable, it is the last one it gave ("" before any).
func (p credsProxy) Identity() (login, email string) {
	var r ResultIdentity
	if err := p.c.request(context.Background(), MethodIdentity, ArgsIdentity{}, &r); err != nil {
		p.c.mu.Lock()
		defer p.c.mu.Unlock()
		return p.c.identity.Login, p.c.identity.Email
	}
	p.c.mu.Lock()
	p.c.identity = r
	p.c.mu.Unlock()
	return r.Login, r.Email
}

// policyProxy is node.Policy over the link. It fails closed: when the server cannot answer, work is restricted,
// nothing is allowed by a rule, every repository is in shadow mode and every sender a stranger.
type policyProxy struct{ c *Client }

var _ node.Policy = policyProxy{}

func (p policyProxy) ToolPolicy(class string) string {
	var out string
	if err := p.c.request(context.Background(), MethodToolPolicy, ArgsToolPolicy{class}, &out); err != nil || out == "" {
		return core.ToolRestricted
	}
	return out
}

func (p policyProxy) Allows(tool, rule, repo string) bool {
	var out bool
	if err := p.c.request(context.Background(), MethodAllows, ArgsAllows{tool, rule, repo}, &out); err != nil {
		return false
	}
	return out
}

func (p policyProxy) Shadow(repo string) bool {
	out := true
	if err := p.c.request(context.Background(), MethodShadow, ArgsShadow{repo}, &out); err != nil {
		return true
	}
	return out
}

func (p policyProxy) Trust(t core.Trigger) string {
	var out string
	if err := p.c.request(context.Background(), MethodTrust, ArgsTrust{t}, &out); err != nil || out == "" {
		return core.TrustStranger
	}
	return out
}

// WorkerPrompt is node.Config.WorkerPrompt: the server's current worker prompt, else the last one it gave. Before
// the server first gave one it is the built-in prompt, but `athena node run` serves only after Ready, which closes
// once the server did, so a serving node never starts a session on it.
func (c *Client) WorkerPrompt() string {
	w, _ := c.prompts()
	return w
}

// ReviewerPrompt is node.Config.ReviewerPrompt, as WorkerPrompt.
func (c *Client) ReviewerPrompt() string {
	_, r := c.prompts()
	return r
}

func (c *Client) prompts() (worker, reviewer string) {
	if r, err := c.fetchPrompts(context.Background()); err == nil {
		return r.Worker, r.Reviewer
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.hasPrompt {
		return prompts.Worker, prompts.Reviewer
	}
	return c.prompt.Worker, c.prompt.Reviewer
}

// errNoPrompts is a prompts answer without a worker prompt.
var errNoPrompts = errors.New("the server's answer has no worker prompt")

// fetchPrompts asks the server for the prompts and caches them.
func (c *Client) fetchPrompts(ctx context.Context) (ResultPrompts, error) {
	var r ResultPrompts
	if err := c.request(ctx, MethodPrompts, ArgsPrompts{}, &r); err != nil {
		return r, err
	}
	if r.Worker == "" {
		return r, errNoPrompts
	}
	c.mu.Lock()
	c.prompt, c.hasPrompt = r, true
	c.mu.Unlock()
	return r, nil
}

// decodeValue unmarshals a NodeResult's value into out (nil: the value is ignored).
func decodeValue(b []byte, out any) error {
	if out == nil || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}
