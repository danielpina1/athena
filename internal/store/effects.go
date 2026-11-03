package store

import (
	"fmt"
	"time"
)

// effectSeqName is the NextSeq counter that orders effects.
const effectSeqName = "effect"

// Enqueue records external work for the effects runner in the transaction. Work is identified by (kind, key):
// enqueuing a key still waiting replaces its payload, makes it due now, clears its failure record and keeps its
// seq, so a newer message takes the place of the older one without jumping the queue.
func (tx *Tx) Enqueue(kind, key string, payload []byte) error {
	if payload == nil {
		payload = []byte{} // a nil slice would bind as NULL
	}
	due := time.Now().UTC().Format(timeLayout)
	res, err := tx.sql.Exec(`UPDATE effects SET payload = ?, due_at = ?, attempts = 0, last_error = ''
		WHERE kind = ? AND key = ?`, payload, due, kind, key)
	if err != nil {
		return fmt.Errorf("store: enqueue %s/%s: %w", kind, key, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: enqueue %s/%s: %w", kind, key, err)
	} else if n > 0 {
		return nil
	}
	seq, err := tx.NextSeq(effectSeqName)
	if err != nil {
		return err
	}
	if _, err := tx.sql.Exec(`INSERT INTO effects (kind, key, payload, seq, due_at) VALUES (?, ?, ?, ?, ?)`,
		kind, key, payload, seq, due); err != nil {
		return fmt.Errorf("store: enqueue %s/%s: %w", kind, key, err)
	}
	return nil
}
