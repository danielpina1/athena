package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/danielpina1/athena/internal/core"
)

// timeLayout is RFC3339 with a fixed nine-digit fraction, so UTC timestamps sort lexicographically.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// GetTicket returns the Ticket with the given ID, or nil, nil if there is none.
func (tx *Tx) GetTicket(id string) (*core.Ticket, error) {
	var doc string
	err := tx.sql.QueryRow(`SELECT doc FROM tickets WHERE id = ?`, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get ticket %q: %w", id, err)
	}
	return decodeTicket(doc)
}

// PutTicket inserts or replaces a Ticket.
func (tx *Tx) PutTicket(t *core.Ticket) error {
	doc, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("store: encode ticket %q: %w", t.ID, err)
	}
	ended := 0
	if t.Ended != nil {
		ended = 1
	}
	_, err = tx.sql.Exec(`INSERT INTO tickets (id, ended, updated_at, doc) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET ended = excluded.ended, updated_at = excluded.updated_at, doc = excluded.doc`,
		t.ID, ended, t.UpdatedAt.UTC().Format(timeLayout), string(doc))
	if err != nil {
		return fmt.Errorf("store: put ticket %q: %w", t.ID, err)
	}
	return nil
}

// OpenTickets returns every Ticket that has not ended, least recently updated first.
func (tx *Tx) OpenTickets() ([]*core.Ticket, error) {
	rows, err := tx.sql.Query(`SELECT doc FROM tickets WHERE ended = 0 ORDER BY updated_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: open tickets: %w", err)
	}
	defer rows.Close()
	var out []*core.Ticket
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("store: open tickets: %w", err)
		}
		t, err := decodeTicket(doc)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: open tickets: %w", err)
	}
	return out, nil
}

// PutTrigger inserts or replaces a Trigger.
func (tx *Tx) PutTrigger(t *core.Trigger) error {
	return tx.putDoc("triggers", t.ID, t)
}

// PutDecision inserts or replaces a Decision.
func (tx *Tx) PutDecision(d *core.Decision) error {
	return tx.putDoc("decisions", d.ID, d)
}

// putDoc upserts a JSON document into one of the (id, doc) tables; table is a constant, never user input.
func (tx *Tx) putDoc(table, id string, v any) error {
	doc, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: encode %s %q: %w", table, id, err)
	}
	_, err = tx.sql.Exec(`INSERT INTO `+table+` (id, doc) VALUES (?, ?)
		ON CONFLICT(id) DO UPDATE SET doc = excluded.doc`, id, string(doc))
	if err != nil {
		return fmt.Errorf("store: put %s %q: %w", table, id, err)
	}
	return nil
}

func decodeTicket(doc string) (*core.Ticket, error) {
	var t core.Ticket
	if err := json.Unmarshal([]byte(doc), &t); err != nil {
		return nil, fmt.Errorf("store: decode ticket: %w", err)
	}
	return &t, nil
}
