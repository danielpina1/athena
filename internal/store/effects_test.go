package store

import (
	"errors"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

type effectRow struct {
	Kind, Key, Payload string
	Seq, Attempts      int
	DueAt              time.Time
	LastError          string
}

// effectRows lists the effects table in seq order.
func effectRows(t *testing.T, s *Store) []effectRow {
	t.Helper()
	var out []effectRow
	err := s.Tx(func(tx *Tx) error {
		rows, err := tx.sql.Query(`SELECT kind, key, payload, seq, due_at, attempts, last_error FROM effects ORDER BY seq`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r effectRow
			var due string
			if err := rows.Scan(&r.Kind, &r.Key, &r.Payload, &r.Seq, &due, &r.Attempts, &r.LastError); err != nil {
				return err
			}
			if r.DueAt, err = time.Parse(timeLayout, due); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEnqueueReplacesWaitingKey(t *testing.T) {
	s, _ := openTemp(t)
	inTx(t, s, func(tx core.Tx) error {
		if err := tx.Enqueue("reply", "k", []byte("first")); err != nil {
			return err
		}
		return tx.Enqueue("reply", "other", []byte("o"))
	})
	// Make the waiting row look like it failed and backed off.
	if err := s.Tx(func(tx *Tx) error {
		_, err := tx.sql.Exec(`UPDATE effects SET attempts = 2, last_error = 'boom', due_at = ? WHERE key = 'k'`,
			time.Now().Add(time.Hour).UTC().Format(timeLayout))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := effectRows(t, s)
	inTx(t, s, func(tx core.Tx) error { return tx.Enqueue("reply", "k", []byte("second")) })

	rows := effectRows(t, s)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	k := rows[0]
	if k.Key != "k" || k.Payload != "second" || k.Seq != before[0].Seq {
		t.Fatalf("replaced row = %+v, want key k, payload second, seq %d", k, before[0].Seq)
	}
	if k.DueAt.After(time.Now()) || k.Attempts != 0 || k.LastError != "" {
		t.Fatalf("replaced row = %+v, want due now with a clean failure record", k)
	}
	if rows[1].Key != "other" || rows[1].Seq <= k.Seq {
		t.Fatalf("other row = %+v", rows[1])
	}

	// The same key under another kind is another effect.
	inTx(t, s, func(tx core.Tx) error { return tx.Enqueue("notice", "k", []byte("n")) })
	if got := effectRows(t, s); len(got) != 3 || got[2].Kind != "notice" || got[2].Seq <= rows[1].Seq {
		t.Fatalf("rows = %+v", got)
	}
}

func TestEnqueueRollsBack(t *testing.T) {
	s, _ := openTemp(t)
	boom := errors.New("boom")
	err := s.Repos().Tx(func(tx core.Tx) error {
		if err := tx.Enqueue("reply", "k", []byte("p")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx = %v, want boom", err)
	}
	if rows := effectRows(t, s); len(rows) != 0 {
		t.Fatalf("rows after rollback = %+v, want none", rows)
	}
	// The sequence did not move either.
	inTx(t, s, func(tx core.Tx) error { return tx.Enqueue("reply", "k", []byte("p")) })
	if rows := effectRows(t, s); len(rows) != 1 || rows[0].Seq != 1 {
		t.Fatalf("rows = %+v, want one with seq 1", rows)
	}
}
