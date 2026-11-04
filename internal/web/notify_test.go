package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/hub"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/store"
)

var t0 = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

// notifyEnv is a hub over a connSource, a store and a Notifier following the hub.
type notifyEnv struct {
	t       *testing.T
	src     *connSource
	hub     *hub.Hub
	st      *store.Store
	visible bool
	ready   chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
	handled chan *athenav1.TicketSummary
}

// seedSession stores a passkey and a web session of it, and returns the session's hash: a push subscription needs one.
func seedSession(t *testing.T, st *store.Store) []byte {
	t.Helper()
	if err := st.PutPasskey(store.Passkey{ID: "seed", Name: "seed", Credential: []byte("c"), CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	hash := []byte("seed-session-hash")
	if err := st.PutWebSession(store.WebSession{Hash: hash, CreatedAt: t0, SeenAt: t0, ReauthAt: t0, PasskeyID: "seed"}); err != nil {
		t.Fatal(err)
	}
	return hash
}

func newNotifyEnv(t *testing.T, seed ...core.Ticket) *notifyEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.PutPushSubscription(store.PushSubscription{SessionHash: seedSession(t, st), Endpoint: "https://push.test/one", P256dh: "k", Auth: "a", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	e := &notifyEnv{t: t, st: st, handled: make(chan *athenav1.TicketSummary, 64), src: &connSource{tickets: map[string]core.Ticket{}}}
	for _, tk := range seed {
		e.src.put(tk)
	}
	e.hub = hub.New(e.src, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	e.hub.Start(ctx)
	e.start(ctx, cancel)
	return e
}

// start runs a Notifier and waits for its reconciliation; it can run again to model a restart.
func (e *notifyEnv) start(ctx context.Context, cancel context.CancelFunc) {
	if e.cancel != nil {
		e.cancel()
		<-e.done
	}
	nctx, ncancel := context.WithCancel(ctx)
	e.cancel = func() { ncancel(); cancel() }
	e.ready = make(chan struct{})
	e.done = make(chan struct{})
	n := NewNotifier(NotifierConfig{Feed: e.hub, Store: e.st, Visible: func() bool { return e.visible }})
	n.observed = func(ts *athenav1.TicketSummary) { e.handled <- ts }
	go func() { n.Run(nctx, e.ready); close(e.done) }()
	e.t.Cleanup(func() { e.cancel(); <-e.done })
	select {
	case <-e.ready:
	case <-time.After(5 * time.Second):
		e.t.Fatal("notifier did not reconcile")
	}
}

func (e *notifyEnv) restart() {
	ctx, cancel := context.WithCancel(context.Background())
	e.start(ctx, cancel)
}

// set commits a Ticket to the source and tells the hub.
func (e *notifyEnv) set(tk core.Ticket) {
	e.src.put(tk)
	e.hub.OnCommit(store.Changes{Tickets: []string{tk.ID}})
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ts := <-e.handled:
			if ts.GetId() == tk.ID && ts.GetUpdatedAt().AsTime().Equal(tk.UpdatedAt) {
				return
			}
		case <-timeout:
			e.t.Fatalf("notifier did not handle %s", tk.ID)
		}
	}
}

func (e *notifyEnv) pushes() []pushJob {
	e.t.Helper()
	fx, err := e.st.KindEffects(PushKind)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []pushJob
	for _, f := range fx {
		var j pushJob
		if err := json.Unmarshal(f.Payload, &j); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, j)
	}
	return out
}

func asking(id, title string, at time.Time) core.Ticket {
	return core.Ticket{ID: id, Title: title, Attention: &core.Attention{ID: "a", Question: "which?"}, UpdatedAt: at}
}

func message(t *testing.T, j pushJob) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(j.Message, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestOneNotificationPerStageEntry(t *testing.T) {
	e := newNotifyEnv(t)
	e.set(core.Ticket{ID: "T-1", Title: "Fix login", UpdatedAt: t0}) // working: nothing
	e.set(asking("T-1", "Fix login", t0.Add(time.Minute)))
	e.set(asking("T-1", "Fix login", t0.Add(2*time.Minute))) // still waiting, updated: not a new entry
	e.set(asking("T-1", "Fix login", t0.Add(3*time.Minute)))
	got := e.pushes()
	if len(got) != 1 {
		t.Fatalf("%d notifications for one entry", len(got))
	}
	m := message(t, got[0])
	if m["title"] != "Athena" || m["body"] != "Fix login needs your answer" || m["url"] != "/t/T-1" || m["tag"] != "T-1" {
		t.Errorf("message %v", m)
	}
	if got[0].Urgency != "normal" {
		t.Errorf("urgency %q", got[0].Urgency)
	}
	fx, _ := e.st.KindEffects(PushKind)
	var key string
	for _, f := range fx {
		key = f.Key
	}
	if want := "T-1:person:answer:" + t0.Add(time.Minute).Format(time.RFC3339Nano) + "|"; !strings.HasPrefix(key, want) {
		t.Errorf("key %q, want %q", key, want)
	}

	// Leaving the stage and entering it again is a new entry.
	e.set(core.Ticket{ID: "T-1", Title: "Fix login", UpdatedAt: t0.Add(4 * time.Minute)})
	e.set(asking("T-1", "Fix login", t0.Add(5*time.Minute)))
	if n := len(e.pushes()); n != 2 {
		t.Errorf("%d notifications after a second entry, want 2", n)
	}
}

func TestApprovalIsUrgent(t *testing.T) {
	e := newNotifyEnv(t)
	e.set(core.Ticket{ID: "T-2", Title: "Deploy", PendingApproval: "ap-1", UpdatedAt: t0})
	got := e.pushes()
	if len(got) != 1 || got[0].Urgency != "high" || message(t, got[0])["body"] != "Deploy needs approval" {
		t.Fatalf("%+v", got)
	}
}

func TestMutedKindSkipped(t *testing.T) {
	e := newNotifyEnv(t)
	if err := SaveMutedKinds(e.st, []string{"approval", "review", "approval"}); err != nil {
		t.Fatal(err)
	}
	if m, _ := MutedKinds(e.st); len(m) != 2 {
		t.Fatalf("muted %v", m)
	}
	e.set(core.Ticket{ID: "T-3", Title: "Deploy", PendingApproval: "ap-1", UpdatedAt: t0})
	e.set(asking("T-4", "Other", t0))
	got := e.pushes()
	if len(got) != 1 || message(t, got[0])["tag"] != "T-4" {
		t.Fatalf("%+v", got)
	}
	// Muted entries are remembered: after a restart they are not pushed.
	e.restart()
	if n := len(e.pushes()); n != 1 {
		t.Errorf("%d notifications after restart", n)
	}
	if err := SaveMutedKinds(e.st, []string{"bogus"}); err == nil {
		t.Error("an unknown kind was saved")
	}
}

func TestVisibleClientSuppresses(t *testing.T) {
	e := newNotifyEnv(t)
	e.visible = true
	e.set(asking("T-5", "Seen", t0))
	if n := len(e.pushes()); n != 0 {
		t.Fatalf("%d notifications while visible", n)
	}
	e.visible = false
	e.set(asking("T-6", "Unseen", t0))
	got := e.pushes()
	if len(got) != 1 || message(t, got[0])["tag"] != "T-6" {
		t.Fatalf("%+v", got)
	}
}

func TestPresenceWindow(t *testing.T) {
	now := t0
	p := NewPresence(func() time.Time { return now })
	a, b := new(int), new(int)
	if p.Visible() {
		t.Fatal("visible with no client")
	}
	p.Report(a, true)
	now = now.Add(29 * time.Second)
	if !p.Visible() {
		t.Error("not visible within 30 s")
	}
	now = now.Add(2 * time.Second)
	if p.Visible() {
		t.Error("visible after 30 s")
	}
	p.Report(a, true)
	p.Report(b, true)
	p.Report(a, false)
	if !p.Visible() {
		t.Error("b is still visible")
	}
	p.Forget(b)
	if p.Visible() {
		t.Error("visible after every client left")
	}
}

func TestReconciliationOnStart(t *testing.T) {
	e := newNotifyEnv(t,
		asking("T-7", "Waiting", t0),
		core.Ticket{ID: "T-8", Title: "Busy", UpdatedAt: t0},
		core.Ticket{ID: "T-9", Title: "Done", Ended: &core.Ended{At: t0}, UpdatedAt: t0})
	got := e.pushes()
	if len(got) != 1 || message(t, got[0])["tag"] != "T-7" {
		t.Fatalf("%+v", got)
	}
	// A restart does not notify it again, even after other updates to the Ticket.
	e.set(asking("T-7", "Waiting", t0.Add(time.Hour)))
	e.restart()
	if n := len(e.pushes()); n != 1 {
		t.Errorf("%d notifications after restart", n)
	}
}

func TestEndedTicketPrunesKeys(t *testing.T) {
	e := newNotifyEnv(t)
	e.set(asking("T-10", "Gone", t0))
	count := func() int {
		var n int
		_ = e.st.Tx(func(tx *store.Tx) error {
			m, err := tx.KVList(sentPrefix + "T-10:")
			n = len(m)
			return err
		})
		return n
	}
	if count() != 1 {
		t.Fatalf("%d keys stored", count())
	}
	e.set(core.Ticket{ID: "T-10", Title: "Gone", Ended: &core.Ended{At: t0}, UpdatedAt: t0.Add(time.Minute)})
	if count() != 0 {
		t.Errorf("%d keys left", count())
	}
}

func TestResyncDoesNotRenotify(t *testing.T) {
	e := newNotifyEnv(t)
	e.set(asking("T-11", "Once", t0))
	before := len(e.pushes())
	// Reconcile again, as after a resync: the same state is not a new entry.
	n := NewNotifier(NotifierConfig{Feed: e.hub, Store: e.st})
	snap, err := e.hub.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	n.reconcile(snap.Tickets)
	n.reconcile(snap.Tickets)
	if len(e.pushes()) != before {
		t.Errorf("resync re-notified: %d -> %d", before, len(e.pushes()))
	}
}

func TestPushPayloadCopy(t *testing.T) {
	cases := []struct {
		tk   core.Ticket
		body string
	}{
		{asking("C-1", "Task", t0), "Task needs your answer"},
		{core.Ticket{ID: "C-2", Title: "Task", PendingApproval: "ap", UpdatedAt: t0}, "Task needs approval"},
	}
	e := newNotifyEnv(t)
	for _, c := range cases {
		e.set(c.tk)
	}
	bodies := map[string]string{}
	for _, j := range e.pushes() {
		m := message(t, j)
		bodies[m["tag"]] = m["body"]
	}
	for _, c := range cases {
		if bodies[c.tk.ID] != c.body {
			t.Errorf("%s: %q, want %q", c.tk.ID, bodies[c.tk.ID], c.body)
		}
	}
	// The remaining kinds, through the Notifier directly.
	want := map[string]string{
		"review": "is ready for review", "delivery": "delivery failed", "blocked": "is blocked", "failed": "failed",
		"answer": "needs your answer", "approval": "needs approval",
	}
	for action, phrase := range want {
		if pushPhrases[action] != phrase {
			t.Errorf("%s: %q, want %q", action, pushPhrases[action], phrase)
		}
	}
	for _, k := range NotificationKinds() {
		if _, ok := pushPhrases[k]; !ok {
			t.Errorf("kind %s has no phrase", k)
		}
	}
	n := NewNotifier(NotifierConfig{Feed: e.hub, Store: e.st})
	for action, phrase := range want {
		id := "P-" + action
		n.notify(&athenav1.TicketSummary{Id: id, Title: "Job", Stage: "person", Action: action, UpdatedAt: timestamppb.New(t0)}, action)
		var found bool
		for _, j := range e.pushes() {
			if m := message(t, j); m["tag"] == id {
				found = true
				if m["body"] != "Job "+phrase {
					t.Errorf("%s: body %q", action, m["body"])
				}
			}
		}
		if !found {
			t.Errorf("%s: no notification", action)
		}
	}
}

func TestOneEffectPerSubscription(t *testing.T) {
	e := newNotifyEnv(t)
	if err := e.st.PutPushSubscription(store.PushSubscription{SessionHash: []byte("seed-session-hash"), Endpoint: "https://push.test/two", P256dh: "k", Auth: "a", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	e.set(asking("T-12", "Both", t0))
	got := e.pushes()
	if len(got) != 2 || got[0].Endpoint == got[1].Endpoint || got[0].Ticket != "T-12" || got[0].Action != "answer" {
		t.Fatalf("%+v", got)
	}
}

func TestNoSubscriptionsNoEffects(t *testing.T) {
	e := newNotifyEnv(t)
	if err := e.st.DeletePushSubscription("https://push.test/one"); err != nil {
		t.Fatal(err)
	}
	e.set(asking("T-13", "Nobody", t0))
	if n := len(e.pushes()); n != 0 {
		t.Fatalf("%d effects without subscriptions", n)
	}
}

func TestRevokedPasskeyGetsNoNotification(t *testing.T) {
	e := newNotifyEnv(t)
	e.set(asking("T-14", "Before", t0))
	if n := len(e.pushes()); n != 1 {
		t.Fatalf("%d effects before the revoke, want 1", n)
	}
	if _, err := e.st.DeletePasskey("seed"); err != nil {
		t.Fatal(err)
	}
	e.set(asking("T-15", "After", t0))
	if n := len(e.pushes()); n != 1 {
		t.Fatalf("%d effects after the revoke, want still 1", n)
	}
}
