package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Node is a remote machine that runs Assignments for the server. A revoked node keeps its row.
type Node struct {
	ID, Name, CertSerial string
	CreatedAt, LastSeen  time.Time
	Draining             bool
	RevokedAt            *time.Time
	// AppliedFactSeq is the high-water mark of the node's facts applied here, for exactly-once replay.
	AppliedFactSeq uint64
	Caps           []byte
}

const nodeColumns = `id, name, cert_serial, created_at, last_seen, draining, revoked_at, applied_fact_seq, caps`

// PutNode inserts the node, or updates its row if the ID exists. Names are unique (a clashing name is an error).
func (s *Store) PutNode(n Node) error {
	var revoked string
	if n.RevokedAt != nil {
		revoked = formatTime(*n.RevokedAt)
	}
	_, err := s.db.Exec(`INSERT INTO nodes (`+nodeColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, cert_serial = excluded.cert_serial, created_at = excluded.created_at,
			last_seen = excluded.last_seen, draining = excluded.draining, revoked_at = excluded.revoked_at,
			applied_fact_seq = excluded.applied_fact_seq, caps = excluded.caps`,
		n.ID, n.Name, n.CertSerial, formatTime(n.CreatedAt), formatTime(n.LastSeen), n.Draining, revoked, int64(n.AppliedFactSeq), n.Caps)
	if err != nil {
		return fmt.Errorf("store: put node: %w", err)
	}
	return nil
}

type rowScanner interface{ Scan(...any) error }

func scanNode(r rowScanner) (*Node, error) {
	var n Node
	var created, seen, revoked string
	var seq int64
	if err := r.Scan(&n.ID, &n.Name, &n.CertSerial, &created, &seen, &n.Draining, &revoked, &seq, &n.Caps); err != nil {
		return nil, err
	}
	n.AppliedFactSeq = uint64(seq)
	var err error
	if n.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if n.LastSeen, err = parseTime(seen); err != nil {
		return nil, err
	}
	if revoked != "" {
		t, err := parseTime(revoked)
		if err != nil {
			return nil, err
		}
		n.RevokedAt = &t
	}
	return &n, nil
}

// GetNode returns the node, or nil if there is none.
func (s *Store) GetNode(id string) (*Node, error) {
	n, err := scanNode(s.db.QueryRow(`SELECT `+nodeColumns+` FROM nodes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get node: %w", err)
	}
	return n, nil
}

// Nodes returns every node, revoked ones too, by name.
func (s *Store) Nodes() ([]Node, error) {
	rows, err := s.db.Query(`SELECT ` + nodeColumns + ` FROM nodes ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: nodes: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("store: nodes: %w", err)
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

// SetNodeDraining marks the node as taking no new Assignments (or not).
func (s *Store) SetNodeDraining(id string, on bool) error {
	_, err := s.db.Exec(`UPDATE nodes SET draining = ? WHERE id = ?`, on, id)
	if err != nil {
		return fmt.Errorf("store: set node draining: %w", err)
	}
	return nil
}

// RevokeNode records that the node's certificate no longer counts, as of at. The row stays.
func (s *Store) RevokeNode(id string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE nodes SET revoked_at = ? WHERE id = ?`, formatTime(at), id)
	if err != nil {
		return fmt.Errorf("store: revoke node: %w", err)
	}
	return nil
}

// TouchNode records that the node was last seen at.
func (s *Store) TouchNode(id string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE nodes SET last_seen = ? WHERE id = ?`, formatTime(at), id)
	if err != nil {
		return fmt.Errorf("store: touch node: %w", err)
	}
	return nil
}

// AdvanceFactSeq moves the node's applied-fact mark to seq: true if it moved, false if seq is not above the mark
// (or the node is unknown), in one statement.
func (s *Store) AdvanceFactSeq(id string, seq uint64) (bool, error) {
	res, err := s.db.Exec(`UPDATE nodes SET applied_fact_seq = ? WHERE id = ? AND applied_fact_seq < ?`, int64(seq), id, int64(seq))
	n, err := rowsAffected(res, err, "advance fact seq")
	return n > 0, err
}
