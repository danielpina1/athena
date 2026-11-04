package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

var t0 = time.Date(2026, 3, 4, 5, 6, 7, 123456789, time.UTC)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "athena.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func sampleTicket(id string) *core.Ticket {
	started := t0.Add(time.Minute)
	read := t0.Add(2 * time.Minute)
	return &core.Ticket{
		ID:         id,
		Title:      "Fix the thing",
		DecisionID: "d1",
		Mandate: core.Mandate{
			Strength: "human", PolicyVersion: "v1",
			Actor: core.ActorRef{Source: "slack", ID: "U1"}, Scope: "repo",
		},
		Channels:  []core.ChannelRef{{Source: "slack", Thread: "t1", Address: map[string]string{"channel": "C1"}}},
		Attention: &core.Attention{ID: "att1", Question: "which branch?", TriggerID: "tr1"},
		Assignments: []core.Assignment{{
			ID: "as1", State: core.AssignmentRunning, CreatedAt: t0, StartedAt: &started,
			Report:    &core.Report{State: core.ReportProgress, Summary: "going", At: started},
			SessionID: "sess",
		}},
		Inputs: []core.Input{
			{ID: "in1", Kind: core.InputTrigger, At: t0, TriggerID: "tr1", Human: true, ReadAt: &read, DeliveredTo: "as1", DeliveredAt: &read},
			{ID: "in2", Kind: core.InputText, At: t0.Add(time.Hour), Text: "hello"},
		},
		Undelivered: []string{"in2"},
		CreatedAt:   t0,
		UpdatedAt:   t0.Add(time.Hour),
		Origin:      core.Origin{TriggerID: "tr1", TriggerIDs: []string{"tr1", "tr2"}},
		Execution:   &core.Execution{Name: "n", Directory: "/w", Assignment: "as1", Runtime: "claude"},
		Delivery:    &core.DeliveryFailure{Error: "boom", At: t0},
	}
}

func put(t *testing.T, s *Store, tk *core.Ticket) {
	t.Helper()
	if err := s.Tx(func(tx *Tx) error { return tx.PutTicket(tk) }); err != nil {
		t.Fatalf("PutTicket: %v", err)
	}
}

func get(t *testing.T, s *Store, id string) *core.Ticket {
	t.Helper()
	var got *core.Ticket
	if err := s.Tx(func(tx *Tx) (err error) { got, err = tx.GetTicket(id); return }); err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	return got
}

func TestRoundTripTicket(t *testing.T) {
	s, _ := openTemp(t)
	want := sampleTicket("t1")
	put(t, s, want)
	got := get(t, s, "t1")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if get(t, s, "absent") != nil {
		t.Fatal("absent ticket should be nil")
	}
}

func TestPutTicketOverwrites(t *testing.T) {
	s, _ := openTemp(t)
	tk := sampleTicket("t1")
	put(t, s, tk)
	tk.Title = "changed"
	put(t, s, tk)
	if got := get(t, s, "t1"); got.Title != "changed" {
		t.Fatalf("title = %q", got.Title)
	}
}

func TestReopenPersists(t *testing.T) {
	s, path := openTemp(t)
	put(t, s, sampleTicket("t1"))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if get(t, s2, "t1") == nil {
		t.Fatal("ticket lost after reopen")
	}
	var schema int
	var ok bool
	if err := s2.Tx(func(tx *Tx) (err error) { ok, err = tx.kvGetJSON("schema", &schema); return }); err != nil {
		t.Fatal(err)
	}
	if !ok || schema != len(migrations) {
		t.Fatalf("schema = %d (found %v), want %d", schema, ok, len(migrations))
	}
}

func TestTxRollback(t *testing.T) {
	s, _ := openTemp(t)
	boom := errors.New("boom")
	err := s.Tx(func(tx *Tx) error {
		if err := tx.PutTicket(sampleTicket("t1")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if get(t, s, "t1") != nil {
		t.Fatal("ticket survived rollback")
	}
}

func TestTxPanicRollsBack(t *testing.T) {
	s, _ := openTemp(t)
	func() {
		defer func() {
			if r := recover(); r != "kaboom" {
				t.Fatalf("recovered %v, want kaboom", r)
			}
		}()
		_ = s.Tx(func(tx *Tx) error {
			_ = tx.PutTicket(sampleTicket("t1"))
			panic("kaboom")
		})
	}()
	if get(t, s, "t1") != nil {
		t.Fatal("ticket survived panic")
	}
	// The store must still be usable (the connection was released).
	put(t, s, sampleTicket("t2"))
}

func TestOpenTicketsExcludesEnded(t *testing.T) {
	s, _ := openTemp(t)
	open1, open2, ended := sampleTicket("a"), sampleTicket("b"), sampleTicket("c")
	open2.UpdatedAt = open1.UpdatedAt.Add(time.Hour)
	ended.Ended = &core.Ended{At: t0, By: core.EndByHuman, Outcome: core.OutcomeClosed}
	for _, tk := range []*core.Ticket{open1, open2, ended} {
		put(t, s, tk)
	}
	var got []*core.Ticket
	if err := s.Tx(func(tx *Tx) (err error) { got, err = tx.OpenTickets(); return }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("open tickets = %v", got)
	}
}

func TestTriggerDecisionKV(t *testing.T) {
	s, _ := openTemp(t)
	err := s.Tx(func(tx *Tx) error {
		if err := tx.PutTrigger(&core.Trigger{ID: "tr1", Source: "slack", At: t0, Text: "hi"}); err != nil {
			return err
		}
		if err := tx.PutDecision(&core.Decision{ID: "d1", Kind: "accept", At: t0}); err != nil {
			return err
		}
		return tx.kvSetJSON("cursor", map[string]int{"n": 3})
	})
	if err != nil {
		t.Fatal(err)
	}
	var cur map[string]int
	var ok, missing bool
	err = s.Tx(func(tx *Tx) (err error) {
		if ok, err = tx.kvGetJSON("cursor", &cur); err != nil {
			return
		}
		var x int
		missing, err = tx.kvGetJSON("nope", &x)
		return
	})
	if err != nil || !ok || cur["n"] != 3 || missing {
		t.Fatalf("kv: err=%v ok=%v cur=%v missing=%v", err, ok, cur, missing)
	}
}

func TestFileModes(t *testing.T) {
	s, path := openTemp(t)
	put(t, s, sampleTicket("t1"))
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if m := fi.Mode().Perm(); m != 0o600 {
		t.Fatalf("db mode = %o, want 600", m)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if m := di.Mode().Perm(); m != 0o700 {
		t.Fatalf("dir mode = %o, want 700", m)
	}
	// WAL/SHM sidecars, if present, must not be more permissive.
	for _, suf := range []string{"-wal", "-shm"} {
		if fi, err := os.Stat(path + suf); err == nil && fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s mode = %o", suf, fi.Mode().Perm())
		}
	}
}

func TestPragmas(t *testing.T) {
	s, _ := openTemp(t)
	var mode string
	var sync, fk int
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`PRAGMA synchronous`).Scan(&sync); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" || sync != 2 || fk != 1 {
		t.Fatalf("journal_mode=%s synchronous=%d foreign_keys=%d, want wal/2/1", mode, sync, fk)
	}
}

func TestOpenRelativePath(t *testing.T) {
	for _, rel := range []string{
		filepath.Join("state", "a.db"),
		filepath.Join("my state", "a b#c?d.db"),
	} {
		t.Run(rel, func(t *testing.T) {
			t.Chdir(t.TempDir())
			s, err := Open(rel)
			if err != nil {
				t.Fatalf("Open(%q): %v", rel, err)
			}
			defer s.Close()
			want := sampleTicket("t1")
			put(t, s, want)
			if got := get(t, s, "t1"); !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip mismatch: %+v", got)
			}
			if _, err := os.Stat(rel); err != nil {
				t.Fatalf("db not at relative path: %v", err)
			}
		})
	}
}

func sampleApproval(id, ticket string, created time.Time) *core.Approval {
	answered := created.Add(time.Minute)
	return &core.Approval{
		ID: id, TicketID: ticket, AssignmentID: "as1", Tool: "Bash", Summary: "rm -rf build", Rule: "destructive",
		Input: json.RawMessage(`{"command":"rm -rf build"}`), State: core.ApprovalPending, CreatedAt: created,
		AnsweredAt: &answered, Answer: "ok",
	}
}

func TestApprovalRoundTrip(t *testing.T) {
	s, path := openTemp(t)
	a := sampleApproval("ap1", "t1", t0)
	var missing *core.Approval
	if err := s.Tx(func(tx *Tx) (err error) {
		if missing, err = tx.GetApproval("ap1"); err != nil {
			return err
		}
		return tx.PutApproval(a)
	}); err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Fatalf("GetApproval before put = %+v, want nil", missing)
	}
	read := func(s *Store, id string) (got *core.Approval) {
		t.Helper()
		if err := s.Tx(func(tx *Tx) (err error) { got, err = tx.GetApproval(id); return }); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := read(s, "ap1"); !reflect.DeepEqual(got, a) {
		t.Fatalf("round trip:\n got  %+v\n want %+v", got, a)
	}

	// Pending ones list oldest first; answered ones drop out; an overwrite keeps one row.
	b := sampleApproval("ap0", "t2", t0.Add(time.Hour))
	c := sampleApproval("ap2", "t1", t0.Add(-time.Hour))
	if err := s.Tx(func(tx *Tx) error {
		for _, x := range []*core.Approval{b, c} {
			if err := tx.PutApproval(x); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ids := func() (out []string) {
		if err := s.Tx(func(tx *Tx) error {
			list, err := tx.PendingApprovals()
			for _, x := range list {
				out = append(out, x.ID)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got, want := ids(), []string{"ap2", "ap1", "ap0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pending = %v, want %v", got, want)
	}
	a.State = core.ApprovalApproved
	if err := s.Tx(func(tx *Tx) error { return tx.PutApproval(a) }); err != nil {
		t.Fatal(err)
	}
	if got, want := ids(), []string{"ap2", "ap0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pending after answer = %v, want %v", got, want)
	}
	if got := read(s, "ap1"); got.State != core.ApprovalApproved {
		t.Fatalf("state = %q after overwrite", got.State)
	}

	// Survives a reopen.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := read(s2, "ap0"); !reflect.DeepEqual(got, b) {
		t.Fatalf("after reopen: %+v, want %+v", got, b)
	}
}

func TestMigration1To2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "athena.db")
	// Build a database exactly as release 1 left it: only migration 1 applied.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO kv (key, value) VALUES ('schema', '1')`); err != nil {
		t.Fatal(err)
	}
	tk, err := json.Marshal(sampleTicket("t1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tickets (id, ended, updated_at, doc) VALUES ('t1', 0, '2026-01-01T00:00:00Z', ?)`, string(tk)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='approvals'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("schema 1 already has approvals (n=%d, err=%v)", n, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on schema 1: %v", err)
	}
	defer s.Close()
	var schema int
	if err := s.Tx(func(tx *Tx) error {
		_, err := tx.kvGetJSON("schema", &schema)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if schema != len(migrations) {
		t.Fatalf("schema = %d after upgrade, want %d", schema, len(migrations))
	}
	if get(t, s, "t1") == nil {
		t.Fatal("ticket lost in the upgrade")
	}
	if err := s.Tx(func(tx *Tx) error { return tx.PutApproval(sampleApproval("ap1", "t1", t0)) }); err != nil {
		t.Fatalf("PutApproval after upgrade: %v", err)
	}
}
