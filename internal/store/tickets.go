package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

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
	// request_id is NULL, not "", when unset: the unique index must not see Tickets that have none as equal.
	var requestID any
	if t.RequestID != "" {
		requestID = t.RequestID
	}
	_, err = tx.sql.Exec(`INSERT INTO tickets (id, ended, updated_at, doc, request_id) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET ended = excluded.ended, updated_at = excluded.updated_at, doc = excluded.doc,
			request_id = excluded.request_id`,
		t.ID, ended, t.UpdatedAt.UTC().Format(timeLayout), string(doc), requestID)
	if err != nil {
		return fmt.Errorf("store: put ticket %q: %w", t.ID, err)
	}
	tx.changes.Tickets = addID(tx.changes.Tickets, t.ID)
	return nil
}

// TicketByRequestID returns the Ticket created under the given request id, or nil, nil if there is none.
func (tx *Tx) TicketByRequestID(requestID string) (*core.Ticket, error) {
	var doc string
	err := tx.sql.QueryRow(`SELECT doc FROM tickets WHERE request_id = ?`, requestID).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: ticket by request id: %w", err)
	}
	return decodeTicket(doc)
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

// GetApproval returns the Approval with the given ID, or nil, nil if there is none.
func (tx *Tx) GetApproval(id string) (*core.Approval, error) {
	var doc string
	err := tx.sql.QueryRow(`SELECT doc FROM approvals WHERE id = ?`, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get approval %q: %w", id, err)
	}
	return decodeApproval(doc)
}

// PutApproval inserts or replaces an Approval.
func (tx *Tx) PutApproval(a *core.Approval) error {
	doc, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("store: encode approval %q: %w", a.ID, err)
	}
	_, err = tx.sql.Exec(`INSERT INTO approvals (id, state, ticket_id, doc) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET state = excluded.state, ticket_id = excluded.ticket_id, doc = excluded.doc`,
		a.ID, a.State, a.TicketID, string(doc))
	if err != nil {
		return fmt.Errorf("store: put approval %q: %w", a.ID, err)
	}
	tx.changes.Approvals = addID(tx.changes.Approvals, a.ID)
	return nil
}

// PendingApprovals returns every Approval still pending, oldest first.
func (tx *Tx) PendingApprovals() ([]*core.Approval, error) {
	rows, err := tx.sql.Query(`SELECT doc FROM approvals WHERE state = ?`, core.ApprovalPending)
	if err != nil {
		return nil, fmt.Errorf("store: pending approvals: %w", err)
	}
	defer rows.Close()
	var out []*core.Approval
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("store: pending approvals: %w", err)
		}
		a, err := decodeApproval(doc)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: pending approvals: %w", err)
	}
	// The table keeps no creation column, so order by the document's own time.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// AnsweredApprovals returns the Approvals no longer pending (answered or expired), the most recently answered first;
// limit <= 0 returns all. It reads in a transaction of its own. The table keeps no answer column, so the rows are
// sorted here by the document's own times (one without an answer time by its creation).
func (s *Store) AnsweredApprovals(limit int) (out []core.Approval, err error) {
	err = s.Tx(func(tx *Tx) error {
		rows, err := tx.sql.Query(`SELECT doc FROM approvals WHERE state != ?`, core.ApprovalPending)
		if err != nil {
			return fmt.Errorf("store: answered approvals: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var doc string
			if err := rows.Scan(&doc); err != nil {
				return fmt.Errorf("store: answered approvals: %w", err)
			}
			a, err := decodeApproval(doc)
			if err != nil {
				return err
			}
			out = append(out, *a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	answered := func(a *core.Approval) time.Time {
		if a.AnsweredAt != nil {
			return *a.AnsweredAt
		}
		return a.CreatedAt
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ti, tj := answered(&out[i]), answered(&out[j]); !ti.Equal(tj) {
			return ti.After(tj)
		}
		return out[i].ID > out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
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

// getDoc loads the JSON document with the given id from one of the (id, doc) tables into v; table is a constant. It
// reports false if there is no such row.
func (tx *Tx) getDoc(table, id string, v any) (bool, error) {
	var doc string
	err := tx.sql.QueryRow(`SELECT doc FROM `+table+` WHERE id = ?`, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: get %s %q: %w", table, id, err)
	}
	if err := json.Unmarshal([]byte(doc), v); err != nil {
		return false, fmt.Errorf("store: decode %s %q: %w", table, id, err)
	}
	return true, nil
}

func decodeApproval(doc string) (*core.Approval, error) {
	var a core.Approval
	if err := json.Unmarshal([]byte(doc), &a); err != nil {
		return nil, fmt.Errorf("store: decode approval: %w", err)
	}
	return &a, nil
}
