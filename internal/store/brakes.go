package store

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/danielpina1/athena/internal/core"
)

// PutBrake inserts or replaces the Brake of a Trigger.
func (tx *Tx) PutBrake(b core.Brake) error {
	doc, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("store: encode brake %q: %w", b.TriggerID, err)
	}
	_, err = tx.sql.Exec(`INSERT INTO brakes (trigger_id, repo, doc) VALUES (?, ?, ?)
		ON CONFLICT(trigger_id) DO UPDATE SET repo = excluded.repo, doc = excluded.doc`, b.TriggerID, b.Repo, string(doc))
	if err != nil {
		return fmt.Errorf("store: put brake %q: %w", b.TriggerID, err)
	}
	return nil
}

// DeleteBrake removes the Brake of a Trigger; none is not an error.
func (tx *Tx) DeleteBrake(triggerID string) error {
	if _, err := tx.sql.Exec(`DELETE FROM brakes WHERE trigger_id = ?`, triggerID); err != nil {
		return fmt.Errorf("store: delete brake %q: %w", triggerID, err)
	}
	return nil
}

// Brakes returns every Brake, oldest first.
func (tx *Tx) Brakes() ([]core.Brake, error) {
	rows, err := tx.sql.Query(`SELECT doc FROM brakes`)
	if err != nil {
		return nil, fmt.Errorf("store: brakes: %w", err)
	}
	defer rows.Close()
	var out []core.Brake
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("store: brakes: %w", err)
		}
		var b core.Brake
		if err := json.Unmarshal([]byte(doc), &b); err != nil {
			return nil, fmt.Errorf("store: decode brake: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: brakes: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].TriggerID < out[j].TriggerID
	})
	return out, nil
}
