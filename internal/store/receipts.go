package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/danielpina1/athena/internal/core"
)

// Reply receipts: the Engine adds them with the reply effect, and the Outbox settles them.

// GetReceipt returns the reply receipt with the given ID, or nil, nil if there is none.
func (tx *Tx) GetReceipt(id string) (*core.Receipt, error) {
	var doc string
	err := tx.sql.QueryRow(`SELECT doc FROM receipts WHERE id = ?`, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get receipt %q: %w", id, err)
	}
	return decodeReceipt(doc)
}

// PutReceipt inserts or replaces a reply receipt.
func (tx *Tx) PutReceipt(r *core.Receipt) error {
	doc, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("store: encode receipt %q: %w", r.ID, err)
	}
	_, err = tx.sql.Exec(`INSERT INTO receipts (id, ticket_id, conversation, state, attempts, doc) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET ticket_id = excluded.ticket_id, conversation = excluded.conversation,
		state = excluded.state, attempts = excluded.attempts, doc = excluded.doc`,
		r.ID, r.TicketID, r.Conversation, r.State, r.Attempts, string(doc))
	if err != nil {
		return fmt.Errorf("store: put receipt %q: %w", r.ID, err)
	}
	return nil
}

// ReceiptsInState returns the reply receipts in a state (waiting, sent or dropped), in queue order (Seq).
func (tx *Tx) ReceiptsInState(state string) ([]*core.Receipt, error) {
	return tx.receipts(`SELECT doc FROM receipts WHERE state = ?`, state)
}

// TicketReceipts returns a Ticket's reply receipts, in queue order (Seq).
func (tx *Tx) TicketReceipts(ticketID string) ([]*core.Receipt, error) {
	return tx.receipts(`SELECT doc FROM receipts WHERE ticket_id = ?`, ticketID)
}

// ReceiptBehind reports whether an earlier receipt (lower Seq) of the same Ticket to the same conversation is still
// waiting and has failed fewer than failures times; a message of no Ticket waits for nothing.
func (tx *Tx) ReceiptBehind(r *core.Receipt, failures int) (bool, error) {
	if r.TicketID == "" {
		return false, nil
	}
	earlier, err := tx.receipts(`SELECT doc FROM receipts WHERE ticket_id = ? AND conversation = ? AND state = ?
		AND attempts < ? AND id != ?`, r.TicketID, r.Conversation, core.ReceiptWaiting, failures, r.ID)
	if err != nil {
		return false, err
	}
	for _, x := range earlier {
		if x.Seq < r.Seq {
			return true, nil
		}
	}
	return false, nil
}

func (tx *Tx) receipts(query string, args ...any) ([]*core.Receipt, error) {
	rows, err := tx.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: receipts: %w", err)
	}
	defer rows.Close()
	var out []*core.Receipt
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("store: receipts: %w", err)
		}
		r, err := decodeReceipt(doc)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: receipts: %w", err)
	}
	slices.SortFunc(out, func(a, b *core.Receipt) int { return a.Seq - b.Seq })
	return out, nil
}

func decodeReceipt(doc string) (*core.Receipt, error) {
	var r core.Receipt
	if err := json.Unmarshal([]byte(doc), &r); err != nil {
		return nil, fmt.Errorf("store: decode receipt: %w", err)
	}
	return &r, nil
}
