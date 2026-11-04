package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

func sampleThread(key string) *core.Thread {
	delivered := t0.Add(time.Minute)
	return &core.Thread{
		Key: key, Source: "github", Label: "owner/repo#7",
		Subscriptions: []core.Subscription{
			{TicketID: "T-1", Bound: true, Own: true},
			{TicketID: "T-2", Context: true},
		},
		Eval: &core.Evaluation{
			ID: "ev1", Thread: key, Primary: "github:issuecomment:1", TriggerIDs: []string{"github:issuecomment:1", "github:issuecomment:2"},
			Strength: "explicit", PolicyVersion: "p3", StartedAt: t0, DeliveredAt: &delivered,
			Basis: core.MandateBasis{Via: core.ViaAddressed, Source: "github", Scope: "comments", Row: "member", Sender: "alice", Rules: []string{"r1"}, Observation: true},
		},
		Expired:   1,
		Decisions: []string{"d1"},
		Closed:    &core.Close{At: t0, Outcome: core.OutcomeMerged},
		Meta:      map[string]string{"url": "https://example.test/7"},
	}
}

func inTx(t *testing.T, s *Store, fn func(core.Tx) error) {
	t.Helper()
	if err := s.Repos().Tx(fn); err != nil {
		t.Fatal(err)
	}
}

func TestThreadRoundTrip(t *testing.T) {
	s, path := openTemp(t)
	want := sampleThread("github:owner/repo#7")
	inTx(t, s, func(tx core.Tx) error { return tx.PutThread(want) })

	var got, missing *core.Thread
	inTx(t, s, func(tx core.Tx) (err error) {
		if got, err = tx.GetThread(want.Key); err != nil {
			return err
		}
		missing, err = tx.GetThread("github:nope")
		return err
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("thread = %+v, want %+v", got, want)
	}
	if missing != nil {
		t.Fatalf("GetThread(missing) = %+v, want nil", missing)
	}

	// Replacing keeps one row, and survives a reopen.
	want.Label = "renamed"
	inTx(t, s, func(tx core.Tx) error { return tx.PutThread(want) })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	inTx(t, s2, func(tx core.Tx) (err error) { got, err = tx.GetThread(want.Key); return err })
	if got == nil || got.Label != "renamed" {
		t.Fatalf("after reopen: %+v", got)
	}
}

func TestActiveThreadsListsQueuedOrEval(t *testing.T) {
	s, _ := openTemp(t)
	queued := &core.Thread{Key: "github:a#2", Source: "github", Queued: []string{"tr1"}}
	evaluating := &core.Thread{Key: "github:a#3", Source: "github", Eval: &core.Evaluation{ID: "ev", Thread: "github:a#3", Primary: "tr2"}}
	idle := &core.Thread{Key: "github:a#1", Source: "github", Subscriptions: []core.Subscription{{TicketID: "T-1"}}}
	becomesIdle := &core.Thread{Key: "github:a#4", Source: "github", Queued: []string{"tr3"}}
	inTx(t, s, func(tx core.Tx) error {
		for _, th := range []*core.Thread{evaluating, queued, idle, becomesIdle} {
			if err := tx.PutThread(th); err != nil {
				return err
			}
		}
		return nil
	})
	becomesIdle.Queued = nil
	inTx(t, s, func(tx core.Tx) error { return tx.PutThread(becomesIdle) })

	var active []*core.Thread
	inTx(t, s, func(tx core.Tx) (err error) { active, err = tx.ActiveThreads(); return err })
	var keys []string
	for _, th := range active {
		keys = append(keys, th.Key)
	}
	if want := []string{"github:a#2", "github:a#3"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("ActiveThreads = %v, want %v", keys, want)
	}
}

func TestTriggerDisposition(t *testing.T) {
	s, _ := openTemp(t)
	requeued := t0.Add(time.Hour)
	want := &core.Trigger{
		ID: "github:issuecomment:9", Source: "github", Kind: core.KindMessage, Actor: core.ActorRef{Source: "github", ID: "alice"},
		Human: true, Addressed: core.AddressedMention, Scope: "comments", At: t0, Text: "@athena please fix",
		Threads: []string{"github:o/r#7", "github:o/r#8"}, ReplyTo: "github:issuecomment:8",
		Meta:     map[string]string{"url": "u"},
		Channels: []core.ChannelRef{{Source: "github", Thread: "github:o/r#7", Address: map[string]string{"issue": "7"}}},
		Disposition: core.Disposition{
			Filtered: "scope", Reached: []string{"T-1"}, Attached: []string{"T-2"}, Requeued: &requeued,
			HandedBack: map[string]string{"T-3": "unrelated"}, Acknowledged: []string{"T-1"},
		},
	}
	inTx(t, s, func(tx core.Tx) error { return tx.PutTrigger(want) })
	var got, missing *core.Trigger
	inTx(t, s, func(tx core.Tx) (err error) {
		if got, err = tx.GetTrigger(want.ID); err != nil {
			return err
		}
		missing, err = tx.GetTrigger("nope")
		return err
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trigger = %+v, want %+v", got, want)
	}
	if missing != nil {
		t.Fatalf("GetTrigger(missing) = %+v", missing)
	}

	// A new Trigger has no disposition in its document.
	raw, err := json.Marshal(core.Trigger{ID: "x", Source: "s", At: t0})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"id":"x","source":"s","actor":{"source":"","id":""},"at":"2026-03-04T05:06:07.123456789Z"}`; string(raw) != want {
		t.Fatalf("plain Trigger encodes as %s, want %s", raw, want)
	}
}

func TestDecisionsByThread(t *testing.T) {
	s, _ := openTemp(t)
	dismissed := t0.Add(time.Hour)
	mk := func(id, thread string, at time.Time) *core.Decision {
		return &core.Decision{ID: id, Kind: "ignore", Thread: thread, At: at, TriggerIDs: []string{"tr"}}
	}
	first := mk("d1", "github:o/r#1", t0)
	first.Reasons = []core.Reason{{Code: "said", Values: map[string]string{"text": "no"}, TriggerID: "tr"}}
	first.WouldAccept, first.IgnoredFromCard, first.Dismissed, first.Ack = true, true, &dismissed, "on it"
	inTx(t, s, func(tx core.Tx) error {
		for _, d := range []*core.Decision{first, mk("d2", "github:o/r#1", t0.Add(time.Minute)), mk("d3", "github:o/r#2", t0.Add(2*time.Minute)), mk("d4", "github:o/r#1", t0.Add(2*time.Minute))} {
			if err := tx.PutDecision(d); err != nil {
				return err
			}
		}
		return nil
	})
	ids := func(ds []*core.Decision) (out []string) {
		for _, d := range ds {
			out = append(out, d.ID)
		}
		return out
	}
	var all, one []*core.Decision
	var got, missing *core.Decision
	inTx(t, s, func(tx core.Tx) (err error) {
		if all, err = tx.Decisions("github:o/r#1", 0); err != nil {
			return err
		}
		if one, err = tx.Decisions("github:o/r#1", 1); err != nil {
			return err
		}
		if got, err = tx.GetDecision("d1"); err != nil {
			return err
		}
		missing, err = tx.GetDecision("nope")
		return err
	})
	if want := []string{"d4", "d2", "d1"}; !reflect.DeepEqual(ids(all), want) {
		t.Fatalf("Decisions = %v, want %v (most recent first)", ids(all), want)
	}
	if want := []string{"d4"}; !reflect.DeepEqual(ids(one), want) {
		t.Fatalf("Decisions(limit 1) = %v, want %v", ids(one), want)
	}
	if !reflect.DeepEqual(got, first) || missing != nil {
		t.Fatalf("GetDecision = %+v / %+v", got, missing)
	}
}

func TestKVPlainStrings(t *testing.T) {
	s, _ := openTemp(t)
	inTx(t, s, func(tx core.Tx) error { return tx.KVSet("router.session", "abc") })
	inTx(t, s, func(tx core.Tx) error { return tx.KVSet("router.session", "def") })
	var v, none string
	var ok, noneOK bool
	inTx(t, s, func(tx core.Tx) (err error) {
		if v, ok, err = tx.KVGet("router.session"); err != nil {
			return err
		}
		none, noneOK, err = tx.KVGet("nope")
		return err
	})
	if v != "def" || !ok || none != "" || noneOK {
		t.Fatalf("KVGet = %q %v / %q %v", v, ok, none, noneOK)
	}
}

func TestMigration3FromV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "athena.db")
	// A database exactly as plan 1 left it: migrations 1 and 2 applied, with a Ticket, Trigger and Decision whose
	// documents carry only plan 1 fields.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:2] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	tk, err := json.Marshal(sampleTicket("t1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO kv (key, value) VALUES ('schema', '2')`, nil},
		{`INSERT INTO tickets (id, ended, updated_at, doc) VALUES ('t1', 0, '2026-01-01T00:00:00Z', ?)`, []any{string(tk)}},
		{`INSERT INTO triggers (id, doc) VALUES ('tr1', '{"id":"tr1","source":"owner","actor":{"source":"owner","id":"me"},"human":true,"at":"2026-01-01T00:00:00Z","text":"hi"}')`, nil},
		{`INSERT INTO decisions (id, doc) VALUES ('d1', '{"id":"d1","kind":"accept","ticketId":"t1","triggerIds":["tr1"],"at":"2026-01-01T00:00:00Z"}')`, nil},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on schema 2: %v", err)
	}
	defer s.Close()
	var schema int
	var tr *core.Trigger
	var d *core.Decision
	var threads []*core.Thread
	inTx(t, s, func(tx core.Tx) (err error) {
		if _, err = tx.(*Tx).kvGetJSON("schema", &schema); err != nil {
			return err
		}
		if tr, err = tx.GetTrigger("tr1"); err != nil {
			return err
		}
		if d, err = tx.GetDecision("d1"); err != nil {
			return err
		}
		threads, err = tx.ActiveThreads()
		return err
	})
	if schema != len(migrations) {
		t.Fatalf("schema = %d after upgrade, want %d", schema, len(migrations))
	}
	if get(t, s, "t1") == nil {
		t.Fatal("ticket lost in the upgrade")
	}
	if tr == nil || tr.Text != "hi" || !tr.Human || tr.Kind != "" || len(tr.Threads) != 0 {
		t.Fatalf("plan 1 trigger decoded as %+v", tr)
	}
	if d == nil || d.Kind != "accept" || d.Thread != "" || len(d.Reasons) != 0 {
		t.Fatalf("plan 1 decision decoded as %+v", d)
	}
	if len(threads) != 0 {
		t.Fatalf("ActiveThreads = %v on an upgraded database", threads)
	}
	// The new tables work.
	inTx(t, s, func(tx core.Tx) error {
		if err := tx.PutThread(sampleThread("github:o/r#1")); err != nil {
			return err
		}
		return tx.Enqueue("reply", "k", []byte("x"))
	})
}

func TestStoreKVOutsideTx(t *testing.T) {
	s, _ := openTemp(t)
	if _, ok, err := s.KVGet("github.cursor.o/r.issues"); err != nil || ok {
		t.Fatalf("KVGet of a missing key = %v, %v", ok, err)
	}
	if err := s.KVSet("github.cursor.o/r.issues", "2026-11-03T11:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := s.KVSet("github.cursor.o/r.issues", "2026-11-03T11:30:00Z"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.KVGet("github.cursor.o/r.issues")
	if err != nil || !ok || v != "2026-11-03T11:30:00Z" {
		t.Fatalf("KVGet = %q, %v, %v", v, ok, err)
	}
	// The same table the Tx helpers use.
	var inTxValue string
	inTx(t, s, func(tx core.Tx) (err error) { inTxValue, _, err = tx.KVGet("github.cursor.o/r.issues"); return err })
	if inTxValue != v {
		t.Fatalf("Tx KVGet = %q", inTxValue)
	}
}

func TestStoreKVList(t *testing.T) {
	s, _ := openTemp(t)
	for k, v := range map[string]string{"github.state.github:o/r#1": "gone", "github.state.github:o/r#2": "open",
		"github.statement": "no", "github.cursor.o/r.issues": "x", "github.state.100%_é": "y"} {
		if err := s.KVSet(k, v); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.KVList("github.state.")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"github.state.github:o/r#1": "gone", "github.state.github:o/r#2": "open", "github.state.100%_é": "y"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("KVList = %v, want %v", got, want)
	}
	if got, err := s.KVList("nothing."); err != nil || len(got) != 0 {
		t.Fatalf("KVList of an unused prefix = %v, %v", got, err)
	}
	if got, err := s.KVList("github.state.100%_"); err != nil || len(got) != 1 {
		t.Fatalf("a prefix with wildcards matches literally: %v, %v", got, err)
	}
}

// The owner's views: every Thread by key, and the Triggers of one Thread (or of all) newest first.
func TestThreadAndTriggerLists(t *testing.T) {
	s, _ := openTemp(t)
	inTx(t, s, func(tx core.Tx) error {
		for _, th := range []*core.Thread{{Key: "github:o/r#2", Source: "github"}, {Key: "github:o/r#1", Source: "github"}} {
			if err := tx.PutThread(th); err != nil {
				return err
			}
		}
		for _, tr := range []*core.Trigger{
			{ID: "a", Source: "github", At: t0, Threads: []string{"github:o/r#1"}},
			{ID: "b", Source: "github", At: t0.Add(2 * time.Minute), Threads: []string{"github:o/r#2", "github:o/r#1"}},
			{ID: "c", Source: "github", At: t0.Add(time.Minute), Threads: []string{"github:o/r#2"}},
		} {
			if err := tx.PutTrigger(tr); err != nil {
				return err
			}
		}
		return nil
	})
	threads, err := s.Threads()
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 2 || threads[0].Key != "github:o/r#1" || threads[1].Key != "github:o/r#2" {
		t.Fatalf("Threads = %+v", threads)
	}
	ids := func(list []core.Trigger) []string {
		var out []string
		for _, tr := range list {
			out = append(out, tr.ID)
		}
		return out
	}
	for _, c := range []struct {
		thread string
		limit  int
		want   []string
	}{
		{"", 0, []string{"b", "c", "a"}},
		{"github:o/r#1", 0, []string{"b", "a"}},
		{"github:o/r#2", 1, []string{"b"}},
		{"github:o/r#9", 0, nil},
	} {
		got, err := s.Triggers(c.thread, c.limit)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ids(got), c.want) {
			t.Errorf("Triggers(%q, %d) = %v, want %v", c.thread, c.limit, ids(got), c.want)
		}
	}
}
