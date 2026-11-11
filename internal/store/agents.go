package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

// Agent messages: one agent's Ticket writes to another's (plan 7).

// PutAgentMessage inserts or replaces an agent message. Both its Tickets change: each one's timeline shows it.
func (tx *Tx) PutAgentMessage(m *core.AgentMessage) error {
	doc, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("store: encode agent message %q: %w", m.ID, err)
	}
	_, err = tx.sql.Exec(`INSERT INTO agent_messages (id, from_ticket, to_ticket, reply_to, at, doc) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET from_ticket = excluded.from_ticket, to_ticket = excluded.to_ticket,
		reply_to = excluded.reply_to, at = excluded.at, doc = excluded.doc`,
		m.ID, m.From, m.To, m.ReplyTo, m.At.UTC().Format(timeLayout), string(doc))
	if err != nil {
		return fmt.Errorf("store: put agent message %q: %w", m.ID, err)
	}
	tx.changes.Tickets = addID(addID(tx.changes.Tickets, m.From), m.To)
	return nil
}

// GetAgentMessage returns the agent message with the given ID, or nil, nil if there is none.
func (tx *Tx) GetAgentMessage(id string) (*core.AgentMessage, error) {
	var doc string
	err := tx.sql.QueryRow(`SELECT doc FROM agent_messages WHERE id = ?`, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get agent message %q: %w", id, err)
	}
	return decodeAgentMessage(doc)
}

// AgentMessagesOf returns the agent messages sent from or to a Ticket, oldest first.
func (tx *Tx) AgentMessagesOf(ticketID string) ([]*core.AgentMessage, error) {
	rows, err := tx.sql.Query(`SELECT doc FROM agent_messages WHERE from_ticket = ? OR to_ticket = ? ORDER BY at, id`, ticketID, ticketID)
	if err != nil {
		return nil, fmt.Errorf("store: agent messages of %q: %w", ticketID, err)
	}
	defer rows.Close()
	var out []*core.AgentMessage
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		m, err := decodeAgentMessage(doc)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountAgentMessagesFrom counts the agent messages a Ticket sent at or after since.
func (tx *Tx) CountAgentMessagesFrom(ticketID string, since time.Time) (int, error) {
	var n int
	err := tx.sql.QueryRow(`SELECT count(*) FROM agent_messages WHERE from_ticket = ? AND at >= ?`,
		ticketID, since.UTC().Format(timeLayout)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count agent messages from %q: %w", ticketID, err)
	}
	return n, nil
}

// AgentAnswer returns the agent message that answers messageID, or nil, nil if there is none.
func (tx *Tx) AgentAnswer(messageID string) (*core.AgentMessage, error) {
	if messageID == "" {
		return nil, nil
	}
	var doc string
	err := tx.sql.QueryRow(`SELECT doc FROM agent_messages WHERE reply_to = ? ORDER BY at, id LIMIT 1`, messageID).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: agent answer to %q: %w", messageID, err)
	}
	return decodeAgentMessage(doc)
}

func decodeAgentMessage(doc string) (*core.AgentMessage, error) {
	var m core.AgentMessage
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		return nil, fmt.Errorf("store: decode agent message: %w", err)
	}
	return &m, nil
}
