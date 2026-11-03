package store

import (
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
	if err := s2.Tx(func(tx *Tx) (err error) { ok, err = tx.KVGet("schema", &schema); return }); err != nil {
		t.Fatal(err)
	}
	if !ok || schema != 1 {
		t.Fatalf("schema = %d (found %v), want 1", schema, ok)
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
		return tx.KVSet("cursor", map[string]int{"n": 3})
	})
	if err != nil {
		t.Fatal(err)
	}
	var cur map[string]int
	var ok, missing bool
	err = s.Tx(func(tx *Tx) (err error) {
		if ok, err = tx.KVGet("cursor", &cur); err != nil {
			return
		}
		var x int
		missing, err = tx.KVGet("nope", &x)
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
