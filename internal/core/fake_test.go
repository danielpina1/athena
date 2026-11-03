package core

import (
	"encoding/json"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memRepos is an in-memory Repositories. Aggregates are stored JSON-encoded, as the SQLite store does, so a command
// that mutates a loaded Ticket without putting it changes nothing, and a failed Tx leaves no trace.
type memRepos struct {
	mu        sync.Mutex
	tickets   map[string][]byte
	triggers  map[string][]byte
	decisions map[string][]byte
	approvals map[string][]byte
	threads   map[string][]byte
	kv        map[string]string
	effects   map[string]fakeEffect
	seqs      map[string]int
	// failWith, when set, makes every Tx roll back with this error after its function ran.
	failWith error
	// inTx is set while a Tx function runs.
	inTx atomic.Bool
}

func newMemRepos() *memRepos {
	return &memRepos{tickets: map[string][]byte{}, triggers: map[string][]byte{}, decisions: map[string][]byte{}, approvals: map[string][]byte{}, threads: map[string][]byte{}, kv: map[string]string{}, effects: map[string]fakeEffect{}, seqs: map[string]int{}}
}

// fakeEffect is a queued effect, as Enqueue leaves it.
type fakeEffect struct {
	Kind, Key string
	Payload   []byte
	Seq       int
}

func (r *memRepos) Tx(fn func(Tx) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	tx := &memTx{
		tickets:   cloneMap(r.tickets),
		triggers:  cloneMap(r.triggers),
		decisions: cloneMap(r.decisions),
		approvals: cloneMap(r.approvals),
		threads:   cloneMap(r.threads),
		kv:        map[string]string{},
		effects:   map[string]fakeEffect{},
		seqs:      map[string]int{},
	}
	for k, v := range r.kv {
		tx.kv[k] = v
	}
	for k, v := range r.effects {
		tx.effects[k] = v
	}
	for k, v := range r.seqs {
		tx.seqs[k] = v
	}
	r.inTx.Store(true)
	err := fn(tx)
	r.inTx.Store(false)
	if err != nil {
		return err
	}
	if r.failWith != nil {
		return r.failWith
	}
	r.tickets, r.triggers, r.decisions, r.approvals, r.seqs = tx.tickets, tx.triggers, tx.decisions, tx.approvals, tx.seqs
	r.threads, r.kv, r.effects = tx.threads, tx.kv, tx.effects
	return nil
}

func cloneMap(m map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ticket returns the stored Ticket with the given ID, or nil.
func (r *memRepos) ticket(id string) *Ticket {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, ok := r.tickets[id]
	if !ok {
		return nil
	}
	var t Ticket
	mustUnmarshal(raw, &t)
	return &t
}

func (r *memRepos) trigger(id string) *Trigger {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, ok := r.triggers[id]
	if !ok {
		return nil
	}
	var t Trigger
	mustUnmarshal(raw, &t)
	return &t
}

// thread returns the stored Thread with the given key, or nil.
func (r *memRepos) thread(key string) *Thread {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, ok := r.threads[key]
	if !ok {
		return nil
	}
	var t Thread
	mustUnmarshal(raw, &t)
	return &t
}

// approval returns the stored Approval with the given ID, or nil.
func (r *memRepos) approval(id string) *Approval {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, ok := r.approvals[id]
	if !ok {
		return nil
	}
	var a Approval
	mustUnmarshal(raw, &a)
	return &a
}

func (r *memRepos) decisionList() []Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Decision
	for _, raw := range r.decisions {
		var d Decision
		mustUnmarshal(raw, &d)
		out = append(out, d)
	}
	return out
}

func (r *memRepos) counts() (tickets, triggers, decisions int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tickets), len(r.triggers), len(r.decisions)
}

type memTx struct {
	tickets, triggers, decisions, approvals, threads map[string][]byte
	kv                                               map[string]string
	effects                                          map[string]fakeEffect
	seqs                                             map[string]int
}

func (tx *memTx) GetTrigger(id string) (*Trigger, error) {
	raw, ok := tx.triggers[id]
	if !ok {
		return nil, nil
	}
	var t Trigger
	mustUnmarshal(raw, &t)
	return &t, nil
}

func (tx *memTx) GetThread(key string) (*Thread, error) {
	raw, ok := tx.threads[key]
	if !ok {
		return nil, nil
	}
	var t Thread
	mustUnmarshal(raw, &t)
	return &t, nil
}

func (tx *memTx) PutThread(t *Thread) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tx.threads[t.Key] = raw
	return nil
}

func (tx *memTx) ActiveThreads() ([]*Thread, error) {
	var out []*Thread
	for _, raw := range tx.threads {
		var t Thread
		mustUnmarshal(raw, &t)
		if t.Active() {
			out = append(out, &t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (tx *memTx) GetDecision(id string) (*Decision, error) {
	raw, ok := tx.decisions[id]
	if !ok {
		return nil, nil
	}
	var d Decision
	mustUnmarshal(raw, &d)
	return &d, nil
}

func (tx *memTx) Decisions(thread string, limit int) ([]*Decision, error) {
	var out []*Decision
	for _, raw := range tx.decisions {
		var d Decision
		mustUnmarshal(raw, &d)
		if d.Thread == thread {
			out = append(out, &d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].ID > out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Enqueue mirrors the store: a waiting (kind, key) takes the new payload and keeps its seq.
func (tx *memTx) Enqueue(kind, key string, payload []byte) error {
	id := kind + "\x00" + key
	e, ok := tx.effects[id]
	if !ok {
		seq, _ := tx.NextSeq("effect")
		e = fakeEffect{Kind: kind, Key: key, Seq: seq}
	}
	e.Payload = append([]byte(nil), payload...)
	tx.effects[id] = e
	return nil
}

func (tx *memTx) KVGet(key string) (string, bool, error) {
	v, ok := tx.kv[key]
	return v, ok, nil
}

func (tx *memTx) KVSet(key, value string) error {
	tx.kv[key] = value
	return nil
}

// effectList returns the queued effects in seq order.
func (r *memRepos) effectList() []fakeEffect {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []fakeEffect
	for _, e := range r.effects {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func (tx *memTx) GetApproval(id string) (*Approval, error) {
	raw, ok := tx.approvals[id]
	if !ok {
		return nil, nil
	}
	var a Approval
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (tx *memTx) PutApproval(a *Approval) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	tx.approvals[a.ID] = raw
	return nil
}

// PendingApprovals lists the pending Approvals, oldest first, as the SQLite store does.
func (tx *memTx) PendingApprovals() ([]*Approval, error) {
	var out []*Approval
	for id := range tx.approvals {
		a, err := tx.GetApproval(id)
		if err != nil {
			return nil, err
		}
		if a.State == ApprovalPending {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (tx *memTx) GetTicket(id string) (*Ticket, error) {
	raw, ok := tx.tickets[id]
	if !ok {
		return nil, nil
	}
	var t Ticket
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (tx *memTx) PutTicket(t *Ticket) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tx.tickets[t.ID] = raw
	return nil
}

func (tx *memTx) list(includeEnded bool) ([]*Ticket, error) {
	var out []*Ticket
	for id := range tx.tickets {
		t, err := tx.GetTicket(id)
		if err != nil {
			return nil, err
		}
		if includeEnded || t.Ended == nil {
			out = append(out, t)
		}
	}
	// Least recently updated first, as the SQLite store orders them.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.Before(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (tx *memTx) OpenTickets() ([]*Ticket, error) { return tx.list(false) }
func (tx *memTx) AllTickets() ([]*Ticket, error)  { return tx.list(true) }

func (tx *memTx) PutTrigger(t *Trigger) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tx.triggers[t.ID] = raw
	return nil
}

func (tx *memTx) PutDecision(d *Decision) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	tx.decisions[d.ID] = raw
	return nil
}

func (tx *memTx) NextSeq(name string) (int, error) {
	tx.seqs[name]++
	return tx.seqs[name], nil
}

func mustUnmarshal(raw []byte, v any) {
	if err := json.Unmarshal(raw, v); err != nil {
		panic(err)
	}
}

// execCall is one call the Engine made on the Executor.
type execCall struct {
	Method       string // start, deliver or stop
	TicketID     string
	AssignmentID string
	Reason       string
	Ticket       Ticket
	Assignment   Assignment
}

// fakeExec records Executor calls and queues launches and deliveries for harness.settle, as the reference test harness does.
type fakeExec struct {
	t     testing.TB
	repos *memRepos
	// strict fails the test when the Engine calls the Executor while a Tx runs (sequential tests only).
	strict bool

	mu         sync.Mutex
	calls      []execCall
	launches   []execCall
	deliveries []string
}

func (f *fakeExec) record(c execCall) {
	if f.strict && f.repos.inTx.Load() {
		f.t.Errorf("executor %s called inside a transaction", c.Method)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	switch c.Method {
	case "start":
		f.launches = append(f.launches, c)
	case "deliver":
		for _, id := range f.deliveries {
			if id == c.TicketID {
				return
			}
		}
		f.deliveries = append(f.deliveries, c.TicketID)
	}
}

func (f *fakeExec) Start(t Ticket, a Assignment) {
	f.record(execCall{Method: "start", TicketID: t.ID, AssignmentID: a.ID, Ticket: t, Assignment: a})
}

func (f *fakeExec) Deliver(t Ticket, a Assignment) {
	f.record(execCall{Method: "deliver", TicketID: t.ID, AssignmentID: a.ID, Ticket: t, Assignment: a})
}

func (f *fakeExec) Stop(t Ticket, a Assignment, reason string) {
	f.record(execCall{Method: "stop", TicketID: t.ID, AssignmentID: a.ID, Reason: reason, Ticket: t, Assignment: a})
}

// methods returns the recorded calls as "method:assignment[:reason]" strings, in order.
func (f *fakeExec) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		s := c.Method + ":" + c.AssignmentID
		if c.Reason != "" {
			s += ":" + c.Reason
		}
		out = append(out, s)
	}
	return out
}

func (f *fakeExec) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

func (f *fakeExec) last(method string) execCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Method == method {
			return f.calls[i]
		}
	}
	f.t.Fatalf("no %s call", method)
	return execCall{}
}

// eventLog records emitted Events.
type eventLog struct {
	t      testing.TB
	repos  *memRepos
	strict bool
	mu     sync.Mutex
	events []Event
}

func (l *eventLog) emit(e Event) {
	if l.strict && l.repos.inTx.Load() {
		l.t.Errorf("event %s emitted inside a transaction", e.Type)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) types() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.events {
		out = append(out, e.Type)
	}
	return out
}

// harness wires an Engine to the fakes with a clock that moves only when a test ticks it.
type harness struct {
	t      *testing.T
	repos  *memRepos
	exec   *fakeExec
	events *eventLog
	eng    *Engine
	clock  atomic.Int64 // seconds after base
	// delivered lists the Inputs settle handed to running Assignments, as [ticket id, input id].
	delivered [][2]string
}

var base = time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, nil)
}

// newHarnessWith is newHarness with Engine Options beyond the clock and the event log, which configure sets.
func newHarnessWith(t *testing.T, configure func(*Options)) *harness {
	t.Helper()
	repos := newMemRepos()
	h := &harness{t: t, repos: repos}
	h.exec = &fakeExec{t: t, repos: repos, strict: true}
	h.events = &eventLog{t: t, repos: repos, strict: true}
	opts := Options{Now: h.now, Emit: h.events.emit}
	if configure != nil {
		configure(&opts)
	}
	h.eng = NewEngineWith(repos, h.exec, opts)
	return h
}

func (h *harness) now() time.Time   { return base.Add(time.Duration(h.clock.Load()) * time.Second) }
func (h *harness) tick(seconds int) { h.clock.Add(int64(seconds)) }

// submit submits an owner task and fails the test on error.
func (h *harness) submit(text string) Ticket {
	h.t.Helper()
	t, err := h.eng.SubmitOwnerTask(text, TaskOptions{Dir: "/work/x", Model: "opus"})
	if err != nil {
		h.t.Fatalf("SubmitOwnerTask: %v", err)
	}
	return t
}

// accepted submits a task and settles it: its Assignment runs and has its first Input (the reference's `accepted`).
func (h *harness) accepted() string {
	h.t.Helper()
	t := h.submit("Fix the login page")
	h.settle(true)
	return t.ID
}

// settle simulates the Executor: it starts queued Assignments (when launch is set) and delivers each waiting Input
// to the running Assignment, one by one, until nothing is queued.
func (h *harness) settle(launch bool) {
	h.t.Helper()
	for {
		h.exec.mu.Lock()
		var next *execCall
		if launch && len(h.exec.launches) > 0 {
			c := h.exec.launches[0]
			h.exec.launches = h.exec.launches[1:]
			next = &c
		}
		var ticketID string
		if next == nil && len(h.exec.deliveries) > 0 {
			ticketID = h.exec.deliveries[0]
			h.exec.deliveries = h.exec.deliveries[1:]
		}
		h.exec.mu.Unlock()
		if next != nil {
			h.must(h.eng.AssignmentStarted(next.TicketID, next.AssignmentID, "session-"+next.AssignmentID, ""))
			continue
		}
		if ticketID == "" {
			return
		}
		t := h.repos.ticket(ticketID)
		a := runningOf(t)
		if t.Ended != nil || a == nil {
			continue
		}
		for _, id := range t.Undelivered {
			h.delivered = append(h.delivered, [2]string{ticketID, id})
			h.must(h.eng.InputsDelivered(ticketID, a.ID, []string{id}))
		}
	}
}

// read simulates the worker's Session taking every delivered, unread trigger Input into a step.
func (h *harness) read(ticketID string) {
	h.t.Helper()
	var ids []string
	for _, in := range h.repos.ticket(ticketID).Inputs {
		if in.Kind == InputTrigger && in.DeliveredTo != "" && in.ReadAt == nil {
			ids = append(ids, in.ID)
		}
	}
	h.must(h.eng.InputsRead(ticketID, ids))
}

func (h *harness) ticket(id string) *Ticket {
	h.t.Helper()
	t := h.repos.ticket(id)
	if t == nil {
		h.t.Fatalf("ticket %s not stored", id)
	}
	return t
}

func (h *harness) must(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatalf("unexpected error: %v", err)
	}
}

func runningOf(t *Ticket) *Assignment {
	for i := range t.Assignments {
		if t.Assignments[i].State == AssignmentRunning {
			return &t.Assignments[i]
		}
	}
	return nil
}
