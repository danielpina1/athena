package store

import (
	"fmt"

	"github.com/danielpina1/athena/internal/core"
)

// seqKeyPrefix namespaces the kv keys that hold NextSeq counters.
const seqKeyPrefix = "seq/"

// The store's transaction handle is the Engine's.
var _ core.Tx = (*Tx)(nil)

// Repos returns the Store as the Engine's Repositories: each Tx runs in one SQLite transaction.
func (s *Store) Repos() core.Repositories { return repos{s} }

type repos struct{ s *Store }

func (r repos) Tx(fn func(core.Tx) error) error {
	return r.s.Tx(func(tx *Tx) error { return fn(tx) })
}

// NextSeq returns the next value of the named counter: 1 on first use, then one more each call. It moves only if
// the transaction commits.
func (tx *Tx) NextSeq(name string) (int, error) {
	key := seqKeyPrefix + name
	n := 0
	if _, err := tx.kvGetJSON(key, &n); err != nil {
		return 0, err
	}
	n++
	if err := tx.kvSetJSON(key, n); err != nil {
		return 0, err
	}
	return n, nil
}

// AllTickets returns every Ticket, ended ones included, least recently updated first.
func (tx *Tx) AllTickets() ([]*core.Ticket, error) {
	rows, err := tx.sql.Query(`SELECT doc FROM tickets ORDER BY updated_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: all tickets: %w", err)
	}
	defer rows.Close()
	var out []*core.Ticket
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("store: all tickets: %w", err)
		}
		t, err := decodeTicket(doc)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: all tickets: %w", err)
	}
	return out, nil
}
