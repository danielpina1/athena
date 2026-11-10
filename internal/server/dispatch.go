package server

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/effects"
	"github.com/danielpina1/athena/internal/fleet"
	"github.com/danielpina1/athena/internal/metrics"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/pause"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/router"
	"github.com/danielpina1/athena/internal/sources/schedule"
	"github.com/danielpina1/athena/internal/store"
	"github.com/danielpina1/athena/internal/web"
)

// The owner's actions, whichever way they come: a control socket request (handleRequest, framed by control.go) or a
// dashboard Command (Do). Both reach the same methods, so the CLI and the dashboard act alike.

const (
	// resultsKept is how long a Command's result answers its resend; pruneEvery is how often older ones go.
	resultsKept  = 24 * time.Hour
	pruneEvery   = time.Hour
	maxCommandID = 128
)

// Dispatcher carries out the owner's actions; it implements web.Commander.
type Dispatcher struct {
	stateDir string
	eng      *core.Engine
	workRoot string
	model    string
	log      *slog.Logger
	reg      *metrics.Registry
	st       *store.Store
	fx       *effects.Runner
	// gh is nil without GitHub in the configuration; host is nil without a router.
	gh     *gitHub
	host   *router.Host
	policy *policyCache
	// autopilot is the schedule Source, nil without a router.
	autopilot *schedule.Source
	// pause is the kill switch; nil (tests) pauses nothing and answers unavailable.
	pause *pause.Switch
	// node is the server's own node (its status, its tokens); nil in tests.
	node *node.Node
	// fleet routes a Ticket's calls (Interrupt) to the node that has it; nil (tests) falls back to node.
	fleet *fleet.Fleet
	// auth ends the open connections of a revoked passkey; until SetAuth (no web server) revoking works on the store.
	auth atomic.Pointer[web.Auth]
	// web is the dashboard's endpoint, nil when it is off; set before the control socket serves.
	web *webEndpoint
	// local finds the local git repositories the dashboard lists; nil when the dashboard or the listing is off.
	local *localRepos
	// facts holds what the dashboard's places need to know about the used folders, found off the hub's loop; nil
	// when the dashboard is off (Repos then guesses: plain folders, unresolved).
	facts *placeFacts
	// onPolicyChange, when set (tests), runs before the Engine hears of a Policy change; an error refuses it.
	onPolicyChange func(from, to policy.Settings) error
	// inflight serializes the copies of one Command (by client and command id) from lookup to stored result.
	inflight keyLocks
	// beforeStore, when set (tests), runs between acting on a Command and storing its result.
	beforeStore func()
}

// keyLocks is a mutex per key, held only while someone uses it. The zero value is ready.
type keyLocks struct {
	mu sync.Mutex
	m  map[string]*keyLock
}

type keyLock struct {
	mu    sync.Mutex
	users int
}

// lock locks key and returns its unlock.
func (k *keyLocks) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyLock{}
	}
	l := k.m[key]
	if l == nil {
		l = &keyLock{}
		k.m[key] = l
	}
	l.users++
	k.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		if l.users--; l.users == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

var _ web.Commander = (*Dispatcher)(nil)

// SetAuth gives the Dispatcher the web server's login, so a revoked passkey's connections close.
func (d *Dispatcher) SetAuth(a *web.Auth) { d.auth.Store(a) }

// --- control socket requests ---

func (d *Dispatcher) handleRequest(req Request) (any, error) {
	switch req.Op {
	case OpPing:
		return nil, nil
	case OpSubmit:
		t, err := d.submit(req.Text, req.Dir, req.Model, req.Repo, req.Runtime, pairingOf(req.Review), "")
		if err != nil {
			return nil, err
		}
		return map[string]string{"id": t.ID}, nil
	case OpTickets:
		list, err := d.eng.Tickets(req.All)
		if err != nil {
			return nil, err
		}
		slices.SortStableFunc(list, func(a, b core.Ticket) int { return a.CreatedAt.Compare(b.CreatedAt) })
		out := make([]TicketSummary, 0, len(list))
		for i := range list {
			out = append(out, summary(&list[i]))
		}
		return out, nil
	case OpTicket:
		return d.detail(req.ID)
	case OpReply:
		return nil, d.eng.AddPersonInput(req.ID, req.Text)
	case OpStop:
		return nil, d.eng.StopTicket(req.ID, req.Reason)
	case OpRetry:
		return nil, d.eng.RetryTicket(req.ID)
	case OpApprovals:
		list, err := d.eng.PendingApprovals()
		if err != nil {
			return nil, err
		}
		out := make([]ApprovalView, 0, len(list))
		for _, a := range list {
			out = append(out, approvalView(a))
		}
		return out, nil
	case OpAnswer:
		return nil, d.eng.AnswerApproval(req.ID, req.Approve, req.Note)
	case OpMetrics:
		window, err := parseWindow(req.Window)
		if err != nil {
			return nil, err
		}
		return d.reg.Snapshot(window), nil
	case OpHealth:
		return d.health(), nil
	case OpTriggers:
		list, err := d.st.Triggers(canonicalThread(req.Thread), listLimit)
		if err != nil {
			return nil, err
		}
		out := make([]TriggerView, 0, len(list))
		for i := range list {
			out = append(out, triggerView(&list[i]))
		}
		return out, nil
	case OpThreads:
		list, err := d.st.Threads()
		if err != nil {
			return nil, err
		}
		out := make([]ThreadView, 0, len(list))
		for i := range list {
			out = append(out, threadView(&list[i]))
		}
		return out, nil
	case OpDecisions:
		list, err := d.eng.Decisions(canonicalThread(req.Thread), listLimit)
		if err != nil {
			return nil, err
		}
		out := make([]DecisionView, 0, len(list))
		for i := range list {
			out = append(out, decisionView(&list[i]))
		}
		return out, nil
	case OpPause, OpResume:
		return nil, d.setPaused(req.Op == OpPause, "cli")
	case OpProposals:
		return d.proposals()
	case OpApply:
		_, err := d.eng.ApplyProposal(req.ID)
		return nil, err
	case OpDiscard:
		return nil, d.eng.DiscardProposal(req.ID)
	case OpReevaluate:
		return nil, d.eng.Reevaluate(req.ID)
	case OpDismiss:
		return nil, d.eng.Dismiss(req.ID)
	case OpUnsubscribe:
		return nil, d.unsubscribe(req.Thread)
	case OpPolicyShow:
		return d.policyShow()
	case OpPolicySet:
		return d.policySet(req.Policy)
	case OpWebRevoke:
		n, err := d.revokePasskey(req.ID)
		if err != nil {
			return nil, err
		}
		return WebRevokeResult{Sessions: n}, nil
	case OpWebStatus:
		if d.web == nil {
			return WebStatusResult{}, nil
		}
		return d.web.status(), nil
	default:
		return nil, userErrorf("unknown op %q", req.Op)
	}
}

func (d *Dispatcher) detail(id string) (TicketDetail, error) {
	t, _, err := d.eng.Ticket(id)
	if err != nil {
		return TicketDetail{}, err
	}
	out := TicketDetail{TicketSummary: summary(&t), Ended: t.Ended, Assignments: t.Assignments, Undelivered: len(t.Undelivered),
		Stats: t.Stats, Repo: t.Repo, Pairing: t.Pairing}
	if out.Assignments == nil {
		out.Assignments = []core.Assignment{}
	}
	if t.Attention != nil {
		out.Question = t.Attention.Question
	}
	if x := t.Execution; x != nil {
		out.SourceDir, out.Runtime, out.Model = x.Directory, x.Runtime, x.Model
	}
	if out.Model == "" {
		out.Model = d.model
	}
	if wd, err := node.WorkingDir(d.workRoot, t); err == nil {
		out.Dir = wd
	}
	if t.PendingApproval != "" {
		list, err := d.eng.PendingApprovals()
		if err != nil {
			return TicketDetail{}, err
		}
		for _, a := range list {
			if a.ID == t.PendingApproval {
				v := approvalView(a)
				out.Approval = &v
				break
			}
		}
	}
	return out, nil
}

// setPaused sets or clears the kill switch.
func (d *Dispatcher) setPaused(on bool, by string) error {
	if d.pause == nil {
		return errors.New("the kill switch is not available")
	}
	var err error
	if on {
		_, err = d.pause.Pause(by)
	} else {
		_, err = d.pause.Resume(by)
	}
	return err
}

// --- dashboard commands ---

// Do carries out a dashboard's Command once: a (client, command id) with a stored result gets that result without
// acting again; otherwise the result is stored after acting. Results that say nothing was done (reauth_required,
// unauthenticated, unavailable, internal) are not stored, so the same Command can be sent again once that changes;
// reads (ListPasskeys, GetPrompt) are not stored either. Copies of one Command run one after the other, so a copy that arrives
// while the first still acts waits and gets its result.
func (d *Dispatcher) Do(_ context.Context, o web.Origin, c *athenav1.Command) *athenav1.CommandResult {
	id := c.GetId()
	switch {
	case id == "" || len(id) > maxCommandID:
		return &athenav1.CommandResult{Id: id, Code: web.CodeInvalid, Message: "a command id is 1 to 128 bytes"}
	case o.ClientID == "":
		return &athenav1.CommandResult{Id: id, Code: web.CodeInvalid, Message: "a command needs a client id"}
	case c.GetAction() == nil:
		return &athenav1.CommandResult{Id: id, Code: web.CodeInvalid, Message: "a command needs an action"}
	}
	switch c.GetAction().(type) {
	case *athenav1.Command_ListPasskeys, *athenav1.Command_GetPrompt:
		res := d.act(o, c)
		res.Id = id
		return res
	}
	// A copy of the Command may be running (the old connection's, after a reconnect): it finishes first, and this one
	// then finds its result.
	unlock := d.inflight.lock(o.ClientID + "\x00" + id)
	defer unlock()
	if raw, ok, err := d.st.CommandResult(o.ClientID, id); err != nil {
		d.log.Error("command result", "client", o.ClientID, "command", id, "error", err)
		return &athenav1.CommandResult{Id: id, Code: web.CodeInternal, Message: "internal error"}
	} else if ok {
		return d.decodeResult(id, raw)
	}
	res := d.act(o, c)
	res.Id = id
	switch res.GetCode() {
	case web.CodeReauthRequired, web.CodeUnauthenticated, web.CodeUnavailable, web.CodeInternal:
		return res
	}
	if d.beforeStore != nil {
		d.beforeStore()
	}
	raw, err := proto.Marshal(res)
	if err != nil {
		d.log.Error("encode command result", "command", id, "error", err)
		return res
	}
	stored, err := d.st.PutCommandResult(o.ClientID, id, raw, time.Now())
	if err != nil {
		d.log.Error("store command result", "client", o.ClientID, "command", id, "error", err)
		return res
	}
	return d.decodeResult(id, stored) // the first result stored, if a copy of the Command ran meanwhile
}

func (d *Dispatcher) decodeResult(id string, raw []byte) *athenav1.CommandResult {
	res := &athenav1.CommandResult{}
	if err := proto.Unmarshal(raw, res); err != nil {
		d.log.Error("decode command result", "command", id, "error", err)
		return &athenav1.CommandResult{Id: id, Code: web.CodeInternal, Message: "internal error"}
	}
	res.Id = id
	return res
}

// requestID names a Command across clients: client ids hold no ':' (web.Dashboard checks), so the pair is unique.
func requestID(o web.Origin, c *athenav1.Command) string { return o.ClientID + ":" + c.GetId() }

var reauthRequired = &athenav1.CommandResult{Code: web.CodeReauthRequired,
	Message: "this needs a passkey confirmation from the last 5 minutes"}

func (d *Dispatcher) act(o web.Origin, c *athenav1.Command) *athenav1.CommandResult {
	ok := &athenav1.CommandResult{Ok: true}
	switch a := c.GetAction().(type) {
	case *athenav1.Command_SubmitTask:
		s := a.SubmitTask
		t, err := d.submit(s.GetText(), s.GetDir(), s.GetModel(), s.GetRepo(), s.GetRuntime(), pairingOf(s.Review), requestID(o, c))
		if err != nil {
			return d.failure(err)
		}
		return &athenav1.CommandResult{Ok: true, Result: &athenav1.CommandResult_TicketId{TicketId: t.ID}}
	case *athenav1.Command_Reply:
		if err := d.eng.AddPersonInputWithID(a.Reply.GetTicketId(), requestID(o, c), a.Reply.GetText()); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_Stop:
		if err := d.eng.StopTicket(a.Stop.GetTicketId(), a.Stop.GetReason()); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_Retry:
		if err := d.eng.RetryTicket(a.Retry.GetTicketId()); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_Interrupt:
		if err := d.interrupt(a.Interrupt.GetTicketId()); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_AnswerApproval:
		aa := a.AnswerApproval
		if err := d.eng.AnswerApproval(aa.GetApprovalId(), aa.GetApprove(), aa.GetNote()); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_Reevaluate:
		if err := d.eng.Reevaluate(a.Reevaluate.GetTriggerId()); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_Dismiss:
		if err := d.eng.Dismiss(a.Dismiss.GetDecisionId()); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_Unsubscribe:
		if err := d.unsubscribe(a.Unsubscribe.GetThread()); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_Pause:
		if err := d.setPaused(true, "web"); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_Resume:
		if !o.Sensitive {
			return proto.Clone(reauthRequired).(*athenav1.CommandResult)
		}
		if err := d.setPaused(false, "web"); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_ApplyProposal:
		if _, err := d.eng.ApplyProposal(a.ApplyProposal.GetId()); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_DiscardProposal:
		if err := d.eng.DiscardProposal(a.DiscardProposal.GetId()); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_AllowAction:
		if !o.Sensitive {
			return proto.Clone(reauthRequired).(*athenav1.CommandResult)
		}
		if err := d.allowAction(a.AllowAction.GetClass()); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_DismissOffer:
		if err := d.dismissOffer(a.DismissOffer.GetClass()); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_SetPolicy:
		if !o.Sensitive {
			return proto.Clone(reauthRequired).(*athenav1.CommandResult)
		}
		res, err := d.setPolicy([]byte(a.SetPolicy.GetSettingsJson()), int(a.SetPolicy.GetBaseRevision()), policy.OriginWeb)
		if err != nil {
			return d.failure(err)
		}
		return &athenav1.CommandResult{Ok: true, Result: &athenav1.CommandResult_PolicyRevision{PolicyRevision: uint32(res.Revision)}}
	case *athenav1.Command_ListPasskeys:
		list, err := d.passkeys()
		if err != nil {
			return d.failure(err)
		}
		return &athenav1.CommandResult{Ok: true, Result: &athenav1.CommandResult_Passkeys{Passkeys: list}}
	case *athenav1.Command_RevokePasskey:
		if !o.Sensitive {
			return proto.Clone(reauthRequired).(*athenav1.CommandResult)
		}
		if _, err := d.revokePasskey(a.RevokePasskey.GetId()); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_GetPrompt:
		detail, err := d.getPrompt(a.GetPrompt.GetPart())
		if err != nil {
			return d.failure(err)
		}
		return &athenav1.CommandResult{Ok: true, Result: &athenav1.CommandResult_Prompt{Prompt: detail}}
	case *athenav1.Command_SetPrompt:
		if !o.Sensitive {
			return proto.Clone(reauthRequired).(*athenav1.CommandResult)
		}
		text := a.SetPrompt.GetText()
		if err := d.setPrompt(a.SetPrompt.GetPart(), a.SetPrompt.GetBaseRevision(), &text); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_ResetPrompt:
		if !o.Sensitive {
			return proto.Clone(reauthRequired).(*athenav1.CommandResult)
		}
		if err := d.setPrompt(a.ResetPrompt.GetPart(), a.ResetPrompt.GetBaseRevision(), nil); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_PushSubscribe:
		if !o.Sensitive {
			return proto.Clone(reauthRequired).(*athenav1.CommandResult)
		}
		if err := d.pushSubscribe(o, a.PushSubscribe); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_PushUnsubscribe:
		if !o.Sensitive {
			return proto.Clone(reauthRequired).(*athenav1.CommandResult)
		}
		if err := d.st.DeletePushSubscription(a.PushUnsubscribe.GetEndpoint()); err != nil {
			return d.failure(err)
		}
	case *athenav1.Command_SetNotificationPrefs:
		if !o.Sensitive {
			return proto.Clone(reauthRequired).(*athenav1.CommandResult)
		}
		if err := web.SaveMutedKinds(d.st, a.SetNotificationPrefs.GetMuted()); err != nil {
			return d.failure(err)
		}
	default:
		// WatchTicket, UnwatchTicket and Visibility belong to the connection, which answers them itself.
		return &athenav1.CommandResult{Code: web.CodeInvalid, Message: "not a command the server carries out"}
	}
	return ok
}

// pushSubscribe stores a browser's push subscription, tied to the calling session, after checking it is one a push service can serve: an https
// endpoint and the browser's P-256 key and 16-byte auth secret.
func (d *Dispatcher) pushSubscribe(o web.Origin, p *athenav1.PushSubscribe) error {
	if !web.ValidPushEndpoint(p.GetEndpoint()) {
		return userErrorf("the push endpoint must be an https URL")
	}
	if !web.ValidSubscriptionKeys(p.GetP256Dh(), p.GetAuth()) {
		return userErrorf("the push subscription keys are not valid")
	}
	hash, err := hex.DecodeString(o.SessionHash)
	if err != nil || len(hash) == 0 {
		return userErrorf("push needs a signed-in session")
	}
	return d.st.PutPushSubscription(store.PushSubscription{SessionHash: hash, Endpoint: p.GetEndpoint(), P256dh: p.GetP256Dh(), Auth: p.GetAuth(), CreatedAt: time.Now()})
}

// failure turns an error into a refused result: a UserError's message with its code, anything else internal (its
// detail goes only to the log).
func (d *Dispatcher) failure(err error) *athenav1.CommandResult {
	var ue *core.UserError
	switch {
	case errors.As(err, &ue):
		code := web.CodeInvalid
		switch {
		case strings.HasSuffix(ue.Msg, " not found"):
			code = web.CodeNotFound
		case ue.Msg == staleSettings:
			code = web.CodeConflict
		}
		return &athenav1.CommandResult{Code: code, Message: ue.Msg}
	case errors.Is(err, store.ErrConflict):
		return &athenav1.CommandResult{Code: web.CodeConflict, Message: stalePrompts}
	case errors.Is(err, store.ErrStaleRevision):
		return &athenav1.CommandResult{Code: web.CodeConflict, Message: staleSettings}
	case errors.Is(err, node.ErrNoSession):
		return &athenav1.CommandResult{Code: web.CodeUnavailable, Message: "the ticket has no session running"}
	}
	d.log.Error("command failed", "error", err)
	return &athenav1.CommandResult{Code: web.CodeInternal, Message: "internal error"}
}

// --- the actions ---

// submit records an owner task. A relative directory would resolve against the server's working directory, not the
// client's; a repository must be one GitHub serves. A requestID seen before returns the Ticket it created.
// pairingOf is the TaskOptions.Pairing of a submit's review switch: on, off, or unset for the Policy's default.
func pairingOf(review *bool) string {
	switch {
	case review == nil:
		return ""
	case *review:
		return core.PairingReview
	}
	return core.PairingOff
}

func (d *Dispatcher) submit(text, dir, model, repo, runtime, pairing, requestID string) (core.Ticket, error) {
	if dir != "" {
		if !filepath.IsAbs(dir) {
			return core.Ticket{}, userErrorf("directory %s must be an absolute path", dir)
		}
		dir = filepath.Clean(dir)
	}
	if repo != "" {
		if d.gh == nil || !d.gh.running() {
			return core.Ticket{}, userErrorf("github is not running: a task on a repository needs it (see athena status)")
		}
		name, ok := d.gh.cfg.repo(repo)
		if !ok {
			return core.Ticket{}, userErrorf("repository %s is not configured (configured: %s)", repo, strings.Join(d.gh.cfg.Repos, ", "))
		}
		repo = name
	}
	t, err := d.eng.SubmitOwnerTask(text, core.TaskOptions{Dir: dir, Model: model, Repo: repo, Runtime: runtime, Pairing: pairing,
		RequestID: requestID})
	if errors.Is(err, core.ErrDuplicate) {
		return t, nil
	}
	return t, err
}

func (d *Dispatcher) unsubscribe(thread string) error {
	thread = canonicalThread(thread)
	if _, err := d.eng.Thread(thread); err != nil {
		return err
	}
	return d.eng.Unsubscribe(thread)
}

// interrupt abandons the running turn of a Ticket's session; a Ticket without a running session is node.ErrNoSession.
func (d *Dispatcher) interrupt(ticketID string) error {
	if _, _, err := d.eng.Ticket(ticketID); err != nil {
		return err
	}
	if d.fleet != nil {
		return d.fleet.Interrupt(ticketID)
	}
	if d.node == nil {
		return node.ErrNoSession
	}
	return d.node.Interrupt(ticketID)
}

func (d *Dispatcher) passkeys() (*athenav1.PasskeyList, error) {
	list, err := d.st.Passkeys()
	if err != nil {
		return nil, err
	}
	out := &athenav1.PasskeyList{}
	for _, p := range list {
		v := &athenav1.Passkey{Id: p.ID, Name: p.Name, CreatedAt: timestamppb.New(p.CreatedAt)}
		if !p.LastUsedAt.IsZero() {
			v.LastUsedAt = timestamppb.New(p.LastUsedAt)
		}
		out.Passkeys = append(out.Passkeys, v)
	}
	return out, nil
}

// WebStatusResult answers web.status. Serving is false when the server runs without the endpoint (web.enabled
// false); the rest is then empty.
type WebStatusResult struct {
	Serving bool   `json:"serving"`
	URL     string `json:"url,omitempty"`
	TLS     string `json:"tls,omitempty"`
	// TLSOK and TLSMessage are the certificate's health; CertNotAfter is the expiry of the one served (none in mode
	// off, or before ACME got one).
	TLSOK        bool       `json:"tlsOk,omitempty"`
	TLSMessage   string     `json:"tlsMessage,omitempty"`
	CertNotAfter *time.Time `json:"certNotAfter,omitempty"`
	// Clients is the number of open dashboard connections.
	Clients int `json:"clients"`
}

// WebRevokeResult answers web.revoke: how many sessions the revoked passkey had.
type WebRevokeResult struct {
	Sessions int `json:"sessions"`
}

// revokePasskey deletes a passkey and its sessions; with the web server's login set, their connections close.
func (d *Dispatcher) revokePasskey(id string) (int, error) {
	if id == "" {
		return 0, userErrorf("revoke needs a passkey id")
	}
	if _, found, err := d.st.Passkey(id); err != nil {
		return 0, err
	} else if !found {
		return 0, userErrorf("passkey %s not found", id)
	}
	if a := d.auth.Load(); a != nil {
		return a.RevokePasskey(id)
	}
	sessions, err := d.st.DeletePasskey(id)
	return len(sessions), err
}

// pruneResults drops the Command results older than resultsKept, the expired web sessions and the spent or expired enrollment tokens, now and every pruneEvery, until ctx is done.
func (d *Dispatcher) pruneResults(ctx context.Context) {
	prune := func() {
		now := time.Now()
		if _, err := d.st.PruneCommandResults(now.Add(-resultsKept)); err != nil {
			d.log.Warn("prune command results", "error", err)
		}
		// A pruned session takes its push subscriptions with it (foreign key cascade).
		if _, err := d.st.PruneWebSessions(now); err != nil {
			d.log.Warn("prune web sessions", "error", err)
		}
		if _, err := d.st.PruneEnrollTokens(now); err != nil {
			d.log.Warn("prune enroll tokens", "error", err)
		}
	}
	prune()
	t := time.NewTicker(pruneEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		}
	}
}
