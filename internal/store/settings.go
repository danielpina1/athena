package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrStaleRevision is returned by SaveSettings when the base revision is not the block's current one.
var ErrStaleRevision = errors.New("store: settings revision is stale")

// SettingsRevision is one stored revision of a settings block.
type SettingsRevision struct {
	Revision int
	Doc      string
}

// CurrentSettings returns the newest revision of a settings block; the bool is false (and the revision 0) if the
// block was never saved.
func (s *Store) CurrentSettings(block string) (SettingsRevision, bool, error) {
	var r SettingsRevision
	err := s.db.QueryRow(`SELECT revision, doc FROM settings WHERE block = ? ORDER BY revision DESC LIMIT 1`, block).
		Scan(&r.Revision, &r.Doc)
	if errors.Is(err, sql.ErrNoRows) {
		return SettingsRevision{}, false, nil
	}
	if err != nil {
		return SettingsRevision{}, false, fmt.Errorf("store: read settings %q: %w", block, err)
	}
	return r, true, nil
}

// SaveSettings appends doc as the next revision of a settings block and returns it. It fails with ErrStaleRevision
// unless baseRevision is the current revision (0 for a block never saved); the check and the insert are one
// transaction.
func (s *Store) SaveSettings(block string, baseRevision int, doc string) (int, error) {
	var rev int
	err := s.Tx(func(tx *Tx) error {
		var cur int
		if err := tx.sql.QueryRow(`SELECT COALESCE(MAX(revision), 0) FROM settings WHERE block = ?`, block).Scan(&cur); err != nil {
			return fmt.Errorf("store: read settings %q: %w", block, err)
		}
		if cur != baseRevision {
			return fmt.Errorf("%w: block %q is at revision %d, not %d", ErrStaleRevision, block, cur, baseRevision)
		}
		rev = cur + 1
		if _, err := tx.sql.Exec(`INSERT INTO settings (block, revision, doc) VALUES (?, ?, ?)`, block, rev, doc); err != nil {
			return fmt.Errorf("store: save settings %q: %w", block, err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return rev, nil
}

// SettingsHistory returns the revisions of a settings block, newest first; limit <= 0 returns all.
func (s *Store) SettingsHistory(block string, limit int) ([]SettingsRevision, error) {
	q := `SELECT revision, doc FROM settings WHERE block = ? ORDER BY revision DESC`
	args := []any{block}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: settings history %q: %w", block, err)
	}
	defer rows.Close()
	var out []SettingsRevision
	for rows.Next() {
		var r SettingsRevision
		if err := rows.Scan(&r.Revision, &r.Doc); err != nil {
			return nil, fmt.Errorf("store: settings history %q: %w", block, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
