package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

func TestMigration4FromV3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "athena.db")
	// A database exactly as plan 2 left it: migrations 1 to 3 applied, with a Ticket that has no request id.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:3] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	tk := `{"id":"t1","title":"old","decisionId":"d1","mandate":{"strength":"human","policyVersion":"v1","actor":{"source":"owner","id":"me"},"scope":"owner"},"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z","origin":{"triggerId":"tr1"},"stats":{}}`
	for _, q := range []string{
		`INSERT INTO kv (key, value) VALUES ('schema', '3')`,
		`INSERT INTO tickets (id, ended, updated_at, doc) VALUES ('t1', 0, '2026-01-01T00:00:00Z', '` + tk + `')`,
		`INSERT INTO tickets (id, ended, updated_at, doc) VALUES ('t2', 0, '2026-01-02T00:00:00Z', '` + tk + `')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on schema 3: %v", err)
	}
	defer s.Close()
	var schema int
	if err := s.Tx(func(tx *Tx) (err error) { _, err = tx.kvGetJSON("schema", &schema); return }); err != nil {
		t.Fatal(err)
	}
	if schema != len(migrations) {
		t.Fatalf("schema = %d after upgrade, want %d", schema, len(migrations))
	}
	if got := get(t, s, "t1"); got == nil || got.Title != "old" || got.RequestID != "" {
		t.Fatalf("ticket after upgrade = %+v", got)
	}
	// Several old Tickets have no request id and must not collide on the unique index.
	a, b := sampleTicket("t3"), sampleTicket("t4")
	a.RequestID, b.RequestID = "r", "r"
	put(t, s, sampleTicket("t5"))
	put(t, s, a)
	if err := s.Tx(func(tx *Tx) error { return tx.PutTicket(b) }); err == nil {
		t.Fatal("two Tickets with one request id were stored")
	}
	var found *core.Ticket
	if err := s.Tx(func(tx *Tx) (err error) { found, err = tx.TicketByRequestID("r"); return }); err != nil || found == nil || found.ID != "t3" {
		t.Fatalf("TicketByRequestID = %+v, %v", found, err)
	}
	// Re-putting the same Ticket keeps working.
	put(t, s, a)
	// The new tables exist.
	if err := s.PutPasskey(Passkey{ID: "p", Name: "n", Credential: []byte("c"), CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
}

// hookLog collects the Changes the commit hook reports.
type hookLog struct{ got atomic.Pointer[[]Changes] }

func (h *hookLog) add(c Changes) {
	for {
		old := h.got.Load()
		var next []Changes
		if old != nil {
			next = slices.Clone(*old)
		}
		next = append(next, c)
		if h.got.CompareAndSwap(old, &next) {
			return
		}
	}
}

func (h *hookLog) all() []Changes {
	if p := h.got.Load(); p != nil {
		return *p
	}
	return nil
}

func TestCommitHookReportsWrittenIDs(t *testing.T) {
	s, _ := openTemp(t)
	var h hookLog
	s.OnCommit(h.add)
	inTx(t, s, func(tx core.Tx) error {
		for _, f := range []func() error{
			func() error { return tx.PutTicket(sampleTicket("t1")) },
			func() error { return tx.PutTicket(sampleTicket("t1")) }, // twice: reported once
			func() error {
				return tx.PutApproval(&core.Approval{ID: "a1", TicketID: "t1", State: core.ApprovalPending})
			},
			func() error { return tx.PutTrigger(&core.Trigger{ID: "tr1"}) },
			func() error { return tx.PutThread(sampleThread("github:o/r#1")) },
			func() error { return tx.PutDecision(&core.Decision{ID: "d1", At: t0}) },
		} {
			if err := f(); err != nil {
				return err
			}
		}
		return nil
	})
	want := Changes{Tickets: []string{"t1"}, Approvals: []string{"a1"}, Triggers: []string{"tr1"}, Threads: []string{"github:o/r#1"}, Decisions: []string{"d1"}}
	if got := h.all(); !reflect.DeepEqual(got, []Changes{want}) {
		t.Fatalf("hook saw %+v, want [%+v]", got, want)
	}

	// A Policy revision reports Policy; another settings block does not.
	if _, err := s.SaveSettings("policy", 0, "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSettings("other", 0, "{}"); err != nil {
		t.Fatal(err)
	}
	got := h.all()
	if len(got) != 2 || !reflect.DeepEqual(got[1], Changes{Policy: true}) {
		t.Fatalf("hook after settings saves saw %+v", got)
	}

	// A Tx that wrote nothing the hook tracks reports nothing.
	inTx(t, s, func(tx core.Tx) error { return tx.KVSet("k", "v") })
	if n := len(h.all()); n != 2 {
		t.Fatalf("hook called %d times, want 2", n)
	}
}

func TestCommitHookSilentOnRollback(t *testing.T) {
	s, _ := openTemp(t)
	var h hookLog
	s.OnCommit(h.add)
	boom := errors.New("boom")
	err := s.Tx(func(tx *Tx) error {
		if err := tx.PutTicket(sampleTicket("t1")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx err = %v", err)
	}
	func() {
		defer func() { _ = recover() }()
		_ = s.Tx(func(tx *Tx) error {
			_ = tx.PutTicket(sampleTicket("t2"))
			panic("x")
		})
	}()
	if got := h.all(); len(got) != 0 {
		t.Fatalf("hook saw %+v after rollbacks", got)
	}
	// The next committed Tx does not carry the rolled-back ids.
	put(t, s, sampleTicket("t3"))
	if got := h.all(); len(got) != 1 || !reflect.DeepEqual(got[0].Tickets, []string{"t3"}) {
		t.Fatalf("hook saw %+v", got)
	}
}

func TestCommandResultsPrune(t *testing.T) {
	s, _ := openTemp(t)
	old, fresh := t0, t0.Add(30*time.Hour)
	if _, err := s.PutCommandResult("c1", "k1", []byte("old"), old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutCommandResult("c1", "k2", []byte("new"), fresh); err != nil {
		t.Fatal(err)
	}
	// A second write under the same key keeps the first result.
	stored, err := s.PutCommandResult("c1", "k2", []byte("other"), fresh)
	if err != nil || string(stored) != "new" {
		t.Fatalf("repeated PutCommandResult = %q, %v; want the first result", stored, err)
	}
	if res, ok, err := s.CommandResult("c1", "k1"); err != nil || !ok || string(res) != "old" {
		t.Fatalf("CommandResult = %q %v %v", res, ok, err)
	}
	n, err := s.PruneCommandResults(t0.Add(24 * time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("PruneCommandResults = %d, %v; want 1", n, err)
	}
	if _, ok, _ := s.CommandResult("c1", "k1"); ok {
		t.Fatal("old result survived the prune")
	}
	if res, ok, _ := s.CommandResult("c1", "k2"); !ok || string(res) != "new" {
		t.Fatalf("fresh result = %q %v", res, ok)
	}
}

func TestPasskeysAndSessions(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.PutPasskey(Passkey{ID: "p1", Name: "phone", Credential: []byte{1, 2}, CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPasskey(Passkey{ID: "p2", Name: "laptop", Credential: []byte{3}, CreatedAt: t0.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchPasskey("p1", t0.Add(time.Hour), []byte{9}); err != nil {
		t.Fatal(err)
	}
	p, ok, err := s.Passkey("p1")
	if err != nil || !ok || !p.LastUsedAt.Equal(t0.Add(time.Hour)) || !reflect.DeepEqual(p.Credential, []byte{9}) || p.Name != "phone" {
		t.Fatalf("Passkey = %+v %v %v", p, ok, err)
	}
	if list, err := s.Passkeys(); err != nil || len(list) != 2 || list[0].ID != "p1" {
		t.Fatalf("Passkeys = %+v %v", list, err)
	}

	for _, ws := range []WebSession{
		{Hash: []byte("h1"), CreatedAt: t0, SeenAt: t0, ReauthAt: t0, PasskeyID: "p1"},
		{Hash: []byte("h2"), CreatedAt: t0, SeenAt: t0, ReauthAt: t0, PasskeyID: "p2"},
	} {
		if err := s.PutWebSession(ws); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.TouchWebSession([]byte("h2"), t0.Add(time.Minute), t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ws, ok, err := s.WebSession([]byte("h2"))
	if err != nil || !ok || !ws.SeenAt.Equal(t0.Add(time.Minute)) || !ws.ReauthAt.Equal(t0.Add(2*time.Minute)) || ws.PasskeyID != "p2" {
		t.Fatalf("WebSession = %+v %v %v", ws, ok, err)
	}
	if list, err := s.WebSessionsOf("p1"); err != nil || len(list) != 1 || string(list[0].Hash) != "h1" || list[0].PasskeyID != "p1" {
		t.Fatalf("WebSessionsOf = %+v %v", list, err)
	}
	// Revoking a passkey ends its sessions and no others.
	if gone, err := s.DeletePasskey("p1"); err != nil || len(gone) != 1 || string(gone[0]) != "h1" {
		t.Fatalf("DeletePasskey = %q %v", gone, err)
	}
	if err := s.PutWebSession(WebSession{Hash: []byte("h9"), CreatedAt: t0, SeenAt: t0, PasskeyID: "p1"}); err == nil {
		t.Fatal("a session was stored for a deleted passkey")
	}
	if _, ok, _ := s.WebSession([]byte("h1")); ok {
		t.Fatal("session of a revoked passkey survived")
	}
	if _, ok, _ := s.WebSession([]byte("h2")); !ok {
		t.Fatal("session of another passkey was removed")
	}
}

func TestPruneWebSessions(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.PutPasskey(Passkey{ID: "p", Name: "p", Credential: []byte{1}, CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	now := t0.Add(200 * 24 * time.Hour)
	for _, ws := range []WebSession{
		{Hash: []byte("live"), CreatedAt: now.Add(-10 * 24 * time.Hour), SeenAt: now.Add(-time.Hour), PasskeyID: "p"},
		{Hash: []byte("idle"), CreatedAt: now.Add(-40 * 24 * time.Hour), SeenAt: now.Add(-31 * 24 * time.Hour), PasskeyID: "p"},
		{Hash: []byte("absolute"), CreatedAt: now.Add(-91 * 24 * time.Hour), SeenAt: now.Add(-time.Hour), PasskeyID: "p"},
	} {
		if err := s.PutWebSession(ws); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneWebSessions(now)
	if err != nil || n != 2 {
		t.Fatalf("PruneWebSessions = %d, %v; want 2", n, err)
	}
	if _, ok, _ := s.WebSession([]byte("live")); !ok {
		t.Fatal("live session pruned")
	}
}

func TestEnrollTokens(t *testing.T) {
	s, _ := openTemp(t)
	h := []byte("tok")
	if err := s.PutEnrollToken(h, t0.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.UseEnrollToken(h, t0.Add(11*time.Minute)); err != nil || ok {
		t.Fatalf("expired token used: %v %v", ok, err)
	}
	if ok, err := s.UseEnrollToken(h, t0.Add(9*time.Minute)); err != nil || !ok {
		t.Fatalf("valid token refused: %v %v", ok, err)
	}
	if ok, err := s.UseEnrollToken(h, t0.Add(9*time.Minute)); err != nil || ok {
		t.Fatalf("token used twice: %v %v", ok, err)
	}
	if ok, _ := s.UseEnrollToken([]byte("nope"), t0); ok {
		t.Fatal("unknown token used")
	}
}

func TestPushSubscriptions(t *testing.T) {
	s, _ := openTemp(t)
	sub := PushSubscription{SessionHash: seedSession(t, s, "p", "h1"), Endpoint: "https://push/1", P256dh: "k", Auth: "a", CreatedAt: t0}
	if err := s.PutPushSubscription(sub); err != nil {
		t.Fatal(err)
	}
	if n, err := s.BumpPushFailures("https://push/1"); err != nil || n != 1 {
		t.Fatalf("BumpPushFailures = %d %v", n, err)
	}
	list, err := s.PushSubscriptions()
	if err != nil || len(list) != 1 || list[0].Failures != 1 || list[0].Auth != "a" {
		t.Fatalf("PushSubscriptions = %+v %v", list, err)
	}
	// Subscribing again resets the failure count.
	if err := s.PutPushSubscription(sub); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.PushSubscriptions(); list[0].Failures != 0 {
		t.Fatalf("failures after resubscribe = %d", list[0].Failures)
	}
	if err := s.DeletePushSubscription("https://push/1"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.PushSubscriptions(); len(list) != 0 {
		t.Fatalf("subscription survived delete: %+v", list)
	}
}

func TestAnsweredApprovalsNewestFirst(t *testing.T) {
	s, _ := openTemp(t)
	at := func(m int) *time.Time { x := t0.Add(time.Duration(m) * time.Minute); return &x }
	err := s.Tx(func(tx *Tx) error {
		for _, a := range []*core.Approval{
			{ID: "a1", TicketID: "T-0001", State: core.ApprovalApproved, CreatedAt: t0, AnsweredAt: at(5)},
			{ID: "a2", TicketID: "T-0001", State: core.ApprovalPending, CreatedAt: t0.Add(time.Hour)},
			{ID: "a3", TicketID: "T-0002", State: core.ApprovalDenied, CreatedAt: t0, AnsweredAt: at(9)},
			{ID: "a4", TicketID: "T-0002", State: core.ApprovalExpired, CreatedAt: t0.Add(7 * time.Minute)},
		} {
			if err := tx.PutApproval(a); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.AnsweredApprovals(0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, a := range got {
		ids = append(ids, a.ID)
	}
	if !slices.Equal(ids, []string{"a3", "a4", "a1"}) {
		t.Fatalf("answered = %v, want a3 a4 a1 (pending left out)", ids)
	}
	if got, _ := s.AnsweredApprovals(2); len(got) != 2 || got[0].ID != "a3" {
		t.Fatalf("limit 2 = %v", got)
	}
}

// seedSession stores a passkey (if new) and a web session of it created at t0, and returns the session's hash.
func seedSession(t *testing.T, s *Store, passkey, hash string) []byte {
	t.Helper()
	if _, ok, _ := s.Passkey(passkey); !ok {
		if err := s.PutPasskey(Passkey{ID: passkey, Name: passkey, Credential: []byte("c"), CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutWebSession(WebSession{Hash: []byte(hash), CreatedAt: t0, SeenAt: t0, ReauthAt: t0, PasskeyID: passkey}); err != nil {
		t.Fatal(err)
	}
	return []byte(hash)
}

func TestMigration5FromV4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "athena.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:4] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO kv (key, value) VALUES ('schema', '4')`,
		`INSERT INTO push_subscriptions (endpoint, p256dh, auth, created_at) VALUES ('https://old', 'k', 'a', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on schema 4: %v", err)
	}
	defer s.Close()
	if list, err := s.PushSubscriptions(); err != nil || len(list) != 0 {
		t.Fatalf("subscriptions after upgrade = %+v, %v (the sessionless rows must go)", list, err)
	}
	h := seedSession(t, s, "p", "h1")
	if err := s.PutPushSubscription(PushSubscription{SessionHash: h, Endpoint: "https://new", P256dh: "k", Auth: "a", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPushSubscription(PushSubscription{SessionHash: []byte("nobody"), Endpoint: "https://x", P256dh: "k", Auth: "a", CreatedAt: t0}); err == nil {
		t.Fatal("a subscription of an unknown session was stored")
	}
}

func TestPushSubscriptionFollowsItsSession(t *testing.T) {
	s, _ := openTemp(t)
	h1, h2 := seedSession(t, s, "p", "h1"), seedSession(t, s, "p", "h2")
	put := func(h []byte, ep string) {
		t.Helper()
		if err := s.PutPushSubscription(PushSubscription{SessionHash: h, Endpoint: ep, P256dh: "k", Auth: "a", CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	endpoints := func() []string {
		list, err := s.PushSubscriptions()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range list {
			out = append(out, p.Endpoint)
		}
		return out
	}
	put(h1, "https://a")
	put(h2, "https://b")
	// Subscribing the same endpoint from another session moves the row.
	put(h2, "https://a")
	if err := s.DeleteWebSession(h1); err != nil {
		t.Fatal(err)
	}
	if got := endpoints(); len(got) != 2 {
		t.Fatalf("endpoints after deleting the old session = %v, want both (a moved to h2)", got)
	}
	if err := s.DeleteWebSession(h2); err != nil {
		t.Fatal(err)
	}
	if got := endpoints(); len(got) != 0 {
		t.Fatalf("endpoints after deleting the session = %v", got)
	}
}

func TestPruneCascadesAndEnrollTokens(t *testing.T) {
	s, _ := openTemp(t)
	h := seedSession(t, s, "p", "h1")
	if err := s.PutPushSubscription(PushSubscription{SessionHash: h, Endpoint: "https://a", P256dh: "k", Auth: "a", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneWebSessions(t0.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("early prune = %d, %v", n, err)
	}
	if n, err := s.PruneWebSessions(t0.Add(WebSessionIdle + time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune = %d, %v", n, err)
	}
	if list, _ := s.PushSubscriptions(); len(list) != 0 {
		t.Fatalf("a pruned session kept its subscription: %+v", list)
	}

	for name, exp := range map[string]time.Time{"live": t0.Add(time.Hour), "spent": t0.Add(time.Hour), "old": t0.Add(-time.Hour)} {
		if err := s.PutEnrollToken([]byte(name), exp); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.UseEnrollToken([]byte("spent"), t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if n, err := s.PruneEnrollTokens(t0); err != nil || n != 2 {
		t.Fatalf("PruneEnrollTokens = %d, %v, want 2 (spent and expired)", n, err)
	}
	if ok, _ := s.UseEnrollToken([]byte("live"), t0); !ok {
		t.Fatal("the live token was pruned")
	}
}
