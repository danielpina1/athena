package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/danielpina1/athena/internal/core"
)

// GetProposal returns the Proposal with the given ID, or nil, nil if there is none.
func (tx *Tx) GetProposal(id string) (*core.Proposal, error) {
	var doc string
	err := tx.sql.QueryRow(`SELECT doc FROM proposals WHERE id = ?`, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get proposal %q: %w", id, err)
	}
	return decodeProposal(doc)
}

// PutProposal inserts or replaces a Proposal.
func (tx *Tx) PutProposal(p *core.Proposal) error {
	doc, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("store: encode proposal %q: %w", p.ID, err)
	}
	_, err = tx.sql.Exec(`INSERT INTO proposals (id, ticket_id, state, doc) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET ticket_id = excluded.ticket_id, state = excluded.state, doc = excluded.doc`,
		p.ID, p.TicketID, p.State, string(doc))
	if err != nil {
		return fmt.Errorf("store: put proposal %q: %w", p.ID, err)
	}
	tx.changes.Proposals = addID(tx.changes.Proposals, p.ID)
	return nil
}

// PendingProposals returns every Proposal still pending, oldest first.
func (tx *Tx) PendingProposals() ([]*core.Proposal, error) {
	return tx.proposals(`SELECT doc FROM proposals WHERE state = ?`, core.ProposalPending)
}

// TicketProposals returns a Ticket's Proposals in every state, oldest first.
func (tx *Tx) TicketProposals(ticketID string) ([]*core.Proposal, error) {
	return tx.proposals(`SELECT doc FROM proposals WHERE ticket_id = ?`, ticketID)
}

func (tx *Tx) proposals(query string, args ...any) ([]*core.Proposal, error) {
	rows, err := tx.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: proposals: %w", err)
	}
	defer rows.Close()
	var out []*core.Proposal
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("store: proposals: %w", err)
		}
		p, err := decodeProposal(doc)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: proposals: %w", err)
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

func decodeProposal(doc string) (*core.Proposal, error) {
	var p core.Proposal
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return nil, fmt.Errorf("store: decode proposal: %w", err)
	}
	return &p, nil
}
