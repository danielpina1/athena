package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

func agentMsg(id, from, to, replyTo string, at time.Time) *core.AgentMessage {
	return &core.AgentMessage{ID: id, From: from, To: to, Text: "hello " + id, ReplyTo: replyTo, Hops: 1, At: at}
}

func TestAgentMessageRoundTrip(t *testing.T) {
	s, _ := openTemp(t)
	want := agentMsg("m1", "T-1", "T-2", "", t0)
	inTx(t, s, func(tx core.Tx) error {
		if err := tx.PutAgentMessage(want); err != nil {
			return err
		}
		return tx.PutAgentMessage(agentMsg("m2", "T-3", "T-1", "", t0.Add(time.Minute)))
	})
	inTx(t, s, func(tx core.Tx) error {
		got, err := tx.GetAgentMessage("m1")
		if err != nil {
			return err
		}
		if got == nil || *got != *want {
			t.Errorf("got %+v, want %+v", got, want)
		}
		if miss, err := tx.GetAgentMessage("nope"); err != nil || miss != nil {
			t.Errorf("missing = %v, %v; want nil, nil", miss, err)
		}
		of, err := tx.AgentMessagesOf("T-1")
		if err != nil {
			return err
		}
		if len(of) != 2 || of[0].ID != "m1" || of[1].ID != "m2" {
			t.Errorf("messages of T-1 = %+v, want m1 then m2", of)
		}
		if of, _ := tx.AgentMessagesOf("T-9"); len(of) != 0 {
			t.Errorf("messages of T-9 = %+v, want none", of)
		}
		return nil
	})
}

func TestCountAgentMessagesFromWindow(t *testing.T) {
	s, _ := openTemp(t)
	now := t0.Add(time.Hour)
	inTx(t, s, func(tx core.Tx) error {
		for i, m := range []int{-11, -5, -1} {
			if err := tx.PutAgentMessage(agentMsg(string(rune('a'+i)), "T-1", "T-2", "", now.Add(time.Duration(m)*time.Minute))); err != nil {
				return err
			}
		}
		// Another sender's message does not count.
		return tx.PutAgentMessage(agentMsg("z", "T-2", "T-1", "", now.Add(-time.Minute)))
	})
	inTx(t, s, func(tx core.Tx) error {
		n, err := tx.CountAgentMessagesFrom("T-1", now.Add(-10*time.Minute))
		if err != nil {
			return err
		}
		if n != 2 {
			t.Errorf("count = %d, want 2", n)
		}
		return nil
	})
}

func TestAgentAnswer(t *testing.T) {
	s, _ := openTemp(t)
	inTx(t, s, func(tx core.Tx) error {
		if err := tx.PutAgentMessage(agentMsg("q", "T-1", "T-2", "", t0)); err != nil {
			return err
		}
		return tx.PutAgentMessage(agentMsg("a", "T-2", "T-1", "q", t0.Add(time.Minute)))
	})
	inTx(t, s, func(tx core.Tx) error {
		got, err := tx.AgentAnswer("q")
		if err != nil {
			return err
		}
		if got == nil || got.ID != "a" {
			t.Errorf("answer = %+v, want a", got)
		}
		if none, err := tx.AgentAnswer("a"); err != nil || none != nil {
			t.Errorf("answer to a = %v, %v; want nil, nil", none, err)
		}
		return nil
	})
}

func TestMigration14OnExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "athena.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:13] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO kv (key, value) VALUES ('schema', '13')`); err != nil {
		t.Fatal(err)
	}
	tk, err := json.Marshal(sampleTicket("t1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tickets (id, ended, updated_at, doc) VALUES ('t1', 0, '2026-01-01T00:00:00Z', ?)`, string(tk)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on schema 13: %v", err)
	}
	defer s.Close()
	if get(t, s, "t1") == nil {
		t.Fatal("ticket lost in the upgrade")
	}
	inTx(t, s, func(tx core.Tx) error { return tx.PutAgentMessage(agentMsg("m", "t1", "t2", "", t0)) })
}

// A message changes what both its Tickets' timelines show: the commit names the sender and the receiver.
func TestAgentMessageReportsBothTickets(t *testing.T) {
	s, _ := openTemp(t)
	var got Changes
	s.OnCommit(func(c Changes) { got = c })
	inTx(t, s, func(tx core.Tx) error { return tx.PutAgentMessage(agentMsg("m1", "T-1", "T-2", "", t0)) })
	if len(got.Tickets) != 2 || got.Tickets[0] != "T-1" || got.Tickets[1] != "T-2" {
		t.Fatalf("changed tickets = %v, want T-1 and T-2", got.Tickets)
	}
}
