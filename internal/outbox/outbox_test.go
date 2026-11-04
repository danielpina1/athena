package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/effects"
	"github.com/danielpina1/athena/internal/sources"
	"github.com/danielpina1/athena/internal/store"
)

// queuedAt is when the fixture's receipts were queued.
var queuedAt = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

const q = "2026-11-03T10:00:00Z"

const (
	issue = "github:o/r#1"
	pull  = "github:o/r#2"
)

// fakeSource is a Source with a Channel: it listens to every Thread but the deaf ones, and fails each message's
// first fails[text] sends.
type fakeSource struct {
	mu     sync.Mutex
	deaf   map[string]bool
	fails  map[string]int
	gone   map[string]bool
	refuse map[string]bool
	sent   []string // texts, in send order
	tries  int
	calls  []sendCall // every Send, in order
	before func()     // runs at the start of Send, without the lock
}

// sendCall is what the Outbox passed to one Send.
type sendCall struct {
	ID       string
	Attempt  int
	QueuedAt string // RFC 3339, UTC
}

func newFakeSource() *fakeSource {
	return &fakeSource{deaf: map[string]bool{}, fails: map[string]int{}, gone: map[string]bool{}, refuse: map[string]bool{}}
}

func (s *fakeSource) Name() string { return "github" }
func (s *fakeSource) Listens(thread string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.deaf[thread]
}
func (s *fakeSource) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (s *fakeSource) Health() error                 { return nil }
func (s *fakeSource) Link(ch core.ChannelRef) string {
	return "https://github.com/" + ch.Thread
}

func (s *fakeSource) Send(_ context.Context, ch core.ChannelRef, req sources.SendRequest) (sources.SendResult, error) {
	text := req.Text
	if s.before != nil {
		s.before()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tries++
	s.calls = append(s.calls, sendCall{req.ReceiptID, req.Attempt, req.QueuedAt.UTC().Format(time.RFC3339)})
	if s.fails[text] > 0 {
		s.fails[text]--
		return "", errors.New("502 bad gateway")
	}
	if s.gone[text] {
		return sources.Gone, nil
	}
	if s.refuse[text] {
		return "", fmt.Errorf("403 Resource not accessible by integration: %w", sources.ErrRefused)
	}
	s.sent = append(s.sent, text)
	return sources.Sent, nil
}

func (s *fakeSource) sentTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

type fixture struct {
	t   *testing.T
	st  *store.Store
	src *fakeSource
	reg *sources.Registry
	h   effects.Handler
	seq int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{t: t, st: st, src: newFakeSource(), reg: sources.NewRegistry()}
	f.reg.Register(f.src)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.reg.Start(ctx)
	f.h = Handler(f.reg, st)
	return f
}

// queue stores a waiting receipt, as the Engine's Reply does, and returns its effect payload.
func (f *fixture) queue(id, ticket, conversation, text string) []byte {
	f.t.Helper()
	f.seq++
	r := &core.Receipt{ID: id, TicketID: ticket, Purpose: core.PurposeAnswer, Text: text,
		Channel: core.ChannelRef{Source: "github", Thread: conversation}, Conversation: conversation,
		State: core.ReceiptWaiting, Seq: f.seq, QueuedAt: queuedAt}
	if err := f.st.Tx(func(tx *store.Tx) error { return tx.PutReceipt(r) }); err != nil {
		f.t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"receiptId": id})
	return payload
}

// run calls the handler as the effects runner would.
func (f *fixture) run(id string) error {
	f.t.Helper()
	payload, _ := json.Marshal(map[string]string{"receiptId": id})
	return f.h(context.Background(), id, payload)
}

func (f *fixture) receipt(id string) *core.Receipt {
	f.t.Helper()
	var r *core.Receipt
	if err := f.st.Tx(func(tx *store.Tx) (err error) { r, err = tx.GetReceipt(id); return err }); err != nil {
		f.t.Fatal(err)
	}
	if r == nil {
		f.t.Fatalf("receipt %s missing", id)
	}
	return r
}

func wantLater(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, effects.ErrLater) {
		t.Errorf("%s: got %v, want effects.Later", what, err)
	}
}

func wantFailure(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil || errors.Is(err, effects.ErrLater) {
		t.Errorf("%s: got %v, want a failure", what, err)
	}
}

func must(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// Reference 003 "Channel failure": a Ticket's messages to one conversation go in queue order; a message waits while
// an earlier one has not gone out and has failed fewer than three times.
func TestOutboxOrderPerConversation(t *testing.T) {
	f := newFixture(t)
	f.queue("ack", "T-0001", issue, "On it.")
	f.queue("result", "T-0001", issue, "Done.")
	f.src.fails["On it."] = 2

	wantFailure(t, "ack fails", f.run("ack"))
	eq(t, "attempt recorded", f.receipt("ack").Attempts, 1)
	wantLater(t, "result waits behind the ack", f.run("result"))
	wantFailure(t, "ack fails again", f.run("ack"))
	eq(t, "attempts", f.receipt("ack").Attempts, 2)
	wantLater(t, "result still waits", f.run("result"))
	eq(t, "result never tried", f.src.tries, 2)
	must(t, "ack goes out", f.run("ack"))
	must(t, "then the result", f.run("result"))
	// The Channel gets the receipt's id, the sends started before this one and the queued time, to keep a retry from
	// duplicating the message.
	eq(t, "id, attempt and queued time reach the Channel", f.src.calls, []sendCall{{"ack", 0, q}, {"ack", 1, q}, {"ack", 2, q}, {"result", 0, q}})
	// Started counts every send begun; Attempts only the failed ones, which is what holds a later message back.
	eq(t, "ack started and failed", [2]int{f.receipt("ack").Started, f.receipt("ack").Attempts}, [2]int{3, 2})
	eq(t, "in order", f.src.sentTexts(), []string{"On it.", "Done."})
	ack := f.receipt("ack")
	eq(t, "ack sent", ack.State, core.ReceiptSent)
	eq(t, "sent at", ack.SentAt != nil, true)
	eq(t, "result sent", f.receipt("result").State, core.ReceiptSent)

	// After three failures, the earlier message no longer holds the later one.
	f.queue("ack2", "T-0001", pull, "On it, again.")
	f.queue("result2", "T-0001", pull, "Done, again.")
	f.src.fails["On it, again."] = 5
	for i := 0; i < 3; i++ {
		wantFailure(t, "ack2 fails", f.run("ack2"))
	}
	must(t, "result2 goes out past a message that failed three times", f.run("result2"))
	eq(t, "result2 first", f.src.sentTexts()[2:], []string{"Done, again."})
	eq(t, "ack2 still waits", f.receipt("ack2").State, core.ReceiptWaiting)
}

// A gone message is dropped with its reason and its receipt kept, and it never goes out again.
func TestOutboxGoneDropsNotRetried(t *testing.T) {
	f := newFixture(t)
	f.queue("m", "T-0001", issue, "Hello.")
	f.src.gone["Hello."] = true
	must(t, "gone settles", f.run("m"))
	r := f.receipt("m")
	eq(t, "dropped", [2]string{r.State, r.DroppedReason}, [2]string{core.ReceiptDropped, core.DropChannelGone})
	eq(t, "dropped at", r.DroppedAt != nil, true)
	must(t, "again: nothing to do", f.run("m"))
	eq(t, "one try", f.src.tries, 1)
	// A dropped message does not hold the later ones.
	f.queue("n", "T-0001", issue, "Later.")
	must(t, "later goes", f.run("n"))
	must(t, "unknown receipt: nothing to do", f.run("nope"))
}

// A failure the same send will never get past (a missing App permission, a validation error, an address that is not
// the conversation's) drops the receipt with its reason and the failure, instead of retrying it forever; Refused lists
// it for health.
func TestOutboxRefusedDropsWithReason(t *testing.T) {
	f := newFixture(t)
	f.queue("m", "T-0001", issue, "Hello.")
	f.src.refuse["Hello."] = true
	must(t, "a refusal settles", f.run("m"))
	r := f.receipt("m")
	eq(t, "dropped", [2]string{r.State, r.DroppedReason}, [2]string{core.ReceiptDropped, core.DropRefused})
	eq(t, "the failure kept", r.LastError, "403 Resource not accessible by integration: "+sources.ErrRefused.Error())
	eq(t, "counted", r.Attempts, 1)
	must(t, "again: nothing to do", f.run("m"))
	eq(t, "one try", f.src.tries, 1)
	got, err := Refused(f.st, time.Time{})
	must(t, "refused", err)
	if len(got) != 1 || got[0].ID != "m" {
		t.Fatalf("refused = %+v, want m", got)
	}
	if got, _ := Refused(f.st, time.Now().Add(time.Hour)); len(got) != 0 {
		t.Fatalf("refused since later = %+v, want none", got)
	}
}

// Messages of other Tickets, and of no Ticket, do not wait for a failing one.
func TestOutboxOtherTicketNotBlocked(t *testing.T) {
	f := newFixture(t)
	f.queue("a1", "T-0001", issue, "On it.")
	f.queue("b1", "T-0002", issue, "Me too.")
	f.queue("n1", "", issue, "Notice.")
	f.src.fails["On it."] = 1
	wantFailure(t, "a1 fails", f.run("a1"))
	must(t, "another ticket's message goes", f.run("b1"))
	must(t, "a message of no ticket goes", f.run("n1"))
	eq(t, "sent", f.src.sentTexts(), []string{"Me too.", "Notice."})
}

// Reference 002: a reply waits while its Source is off and is dropped once its Source no longer listens to the
// conversation.
func TestOutboxSourceOffWaitsNotListeningDrops(t *testing.T) {
	f := newFixture(t)
	f.queue("m", "T-0001", issue, "Hello.")
	f.src.deaf[issue] = true
	must(t, "not listening settles", f.run("m"))
	r := f.receipt("m")
	eq(t, "dropped", [2]string{r.State, r.DroppedReason}, [2]string{core.ReceiptDropped, core.DropChannelGone})
	eq(t, "never sent", f.src.tries, 0)

	// A registered Source that does not run, and a Source not registered, leave the message waiting.
	off := sources.NewRegistry()
	off.Register(newFakeSource())
	h := Handler(off, f.st)
	f.queue("w", "T-0001", pull, "Wait.")
	payload, _ := json.Marshal(map[string]string{"receiptId": "w"})
	wantLater(t, "source not running", h(context.Background(), "w", payload))
	wantLater(t, "source not registered", Handler(sources.NewRegistry(), f.st)(context.Background(), "w", payload))
	eq(t, "still waiting", f.receipt("w").State, core.ReceiptWaiting)
	wantFailure(t, "malformed payload", f.h(context.Background(), "w", []byte("{")))
}

func eq[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

type nopExec struct{}

func (nopExec) Start(core.Ticket, core.Assignment)        {}
func (nopExec) Deliver(core.Ticket, core.Assignment)      {}
func (nopExec) Stop(core.Ticket, core.Assignment, string) {}

// End to end: a Reply the Engine commits goes out through the effects runner, the Outbox and the Source's Channel.
func TestEngineReplyGoesOut(t *testing.T) {
	f := newFixture(t)
	fx := effects.New(f.st, nil)
	fx.Handle(core.EffectReply, f.h, Options)
	eng := core.NewEngineWith(f.st.Repos(), nopExec{}, core.Options{Wake: fx.Wake})
	tk, err := eng.SubmitOwnerTask("Fix the login page", core.TaskOptions{})
	must(t, "submit", err)
	must(t, "channel", f.st.Tx(func(tx *store.Tx) error {
		t, err := tx.GetTicket(tk.ID)
		if err != nil {
			return err
		}
		t.Channels = []core.ChannelRef{{Source: "github", Thread: issue}}
		return tx.PutTicket(t)
	}))
	must(t, "ready", eng.FileReport(tk.ID, core.ReportInput{State: core.ReportReady, Summary: "fixed"}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx.Start(ctx)
	defer fx.Drain(time.Second)
	id, err := eng.Reply(core.ReplyRequest{TicketID: tk.ID, Purpose: core.PurposeResult, Text: "Fixed."})
	must(t, "reply", err)
	deadline := time.Now().Add(5 * time.Second)
	for f.receipt(id).State == core.ReceiptWaiting && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	eq(t, "sent", f.receipt(id).State, core.ReceiptSent)
	eq(t, "through the channel", f.src.sentTexts(), []string{"Fixed."})
}

// R20: a send is recorded as started before it is made. A run that dies after the Channel sent the message but before
// the receipt was settled leaves the receipt waiting; its re-run is a retry (attempt above zero), so the Channel
// checks for the message instead of posting it again.
func TestOutboxRerunAfterLostSettleIsARetry(t *testing.T) {
	f := newFixture(t)
	f.queue("m", "T-0001", issue, "Hello.")
	must(t, "sent", f.run("m"))
	eq(t, "sent", f.receipt("m").State, core.ReceiptSent)
	// The crash: the send happened, the settle did not.
	must(t, "undo the settle", f.st.Tx(func(tx *store.Tx) error {
		r, err := tx.GetReceipt("m")
		if err != nil {
			return err
		}
		r.State, r.SentAt = core.ReceiptWaiting, nil
		return tx.PutReceipt(r)
	}))
	eq(t, "the lost settle is no failure", f.receipt("m").Attempts, 0)
	must(t, "re-run", f.run("m"))
	eq(t, "first send is attempt 0, the re-run is attempt 1", f.src.calls, []sendCall{{"m", 0, q}, {"m", 1, q}})
	eq(t, "settled", f.receipt("m").State, core.ReceiptSent)
}

// The started mark is written before Send: a Send that never returns (the process dies) still leaves it behind.
func TestOutboxRecordsTheStartBeforeSending(t *testing.T) {
	f := newFixture(t)
	f.queue("m", "T-0001", issue, "Hello.")
	started := -1
	f.src.before = func() { started = f.receipt("m").Started }
	must(t, "sent", f.run("m"))
	eq(t, "started was already 1 inside Send", started, 1)
}
