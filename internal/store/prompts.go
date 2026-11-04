package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/danielpina1/athena/prompts"
)

// ErrConflict is returned when a save was edited from a revision that is no longer the current one (the same error as
// ErrStaleRevision, which the dispatcher reports as the conflict result code).
var ErrConflict = ErrStaleRevision

// PromptOverride is the owner's current text for one prompt part and the default it was saved against.
type PromptOverride struct {
	Part, Text  string
	Revision    uint32
	DefaultHash string
	// DefaultText is the default the owner saw when saving.
	DefaultText string
}

// PromptRevision is one stored revision of a prompt part; a nil Text is a reset to the default.
type PromptRevision struct {
	Revision    uint32
	Part        string
	Text        *string
	DefaultHash string
	DefaultText string
	Origin      string
	At          time.Time
}

// PromptOverrides returns the parts whose latest revision is an override (not a reset), by part name, and the latest
// revision of any part (0 while nothing was saved).
func (s *Store) PromptOverrides() (map[string]PromptOverride, uint32, error) {
	rows, err := s.db.Query(`SELECT r.revision, r.part, r.text, r.default_hash, r.default_text FROM prompt_revisions r
		WHERE r.revision = (SELECT MAX(revision) FROM prompt_revisions WHERE part = r.part)`)
	if err != nil {
		return nil, 0, fmt.Errorf("store: prompt overrides: %w", err)
	}
	defer rows.Close()
	out := map[string]PromptOverride{}
	for rows.Next() {
		var o PromptOverride
		var text sql.NullString
		if err := rows.Scan(&o.Revision, &o.Part, &text, &o.DefaultHash, &o.DefaultText); err != nil {
			return nil, 0, fmt.Errorf("store: prompt overrides: %w", err)
		}
		if text.Valid {
			o.Text = text.String
			out[o.Part] = o
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: prompt overrides: %w", err)
	}
	var latest uint32
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(revision), 0) FROM prompt_revisions`).Scan(&latest); err != nil {
		return nil, 0, fmt.Errorf("store: prompt revision: %w", err)
	}
	return out, latest, nil
}

// SavePrompt appends a revision of part: text is the override, nil a reset to the default. defaultText is the part's
// current compiled-in default, recorded with its hash. It fails with ErrConflict unless base is the latest revision
// of any part (0 while nothing was saved); the check and the insert are one transaction. It returns the new revision.
func (s *Store) SavePrompt(part string, text *string, base uint32, defaultText string, origin string) (uint32, error) {
	var rev uint32
	err := s.Tx(func(tx *Tx) error {
		var cur uint32
		if err := tx.sql.QueryRow(`SELECT COALESCE(MAX(revision), 0) FROM prompt_revisions`).Scan(&cur); err != nil {
			return fmt.Errorf("store: read prompt revision: %w", err)
		}
		if cur != base {
			return fmt.Errorf("%w: prompts are at revision %d, not %d", ErrConflict, cur, base)
		}
		rev = cur + 1
		var t any
		if text != nil {
			t = *text
		}
		if _, err := tx.sql.Exec(`INSERT INTO prompt_revisions (revision, part, text, default_hash, default_text, origin, at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, rev, part, t, prompts.Hash(defaultText), defaultText, origin, tx.now().UnixMilli()); err != nil {
			return fmt.Errorf("store: save prompt %q: %w", part, err)
		}
		tx.changes.Prompts = true
		return nil
	})
	if err != nil {
		return 0, err
	}
	return rev, nil
}

// PromptHistory returns the revisions of a part, newest first; limit <= 0 returns all.
func (s *Store) PromptHistory(part string, limit int) ([]PromptRevision, error) {
	q := `SELECT revision, part, text, default_hash, default_text, origin, at FROM prompt_revisions WHERE part = ? ORDER BY revision DESC`
	args := []any{part}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: prompt history %q: %w", part, err)
	}
	defer rows.Close()
	var out []PromptRevision
	for rows.Next() {
		var r PromptRevision
		var text sql.NullString
		var at int64
		if err := rows.Scan(&r.Revision, &r.Part, &text, &r.DefaultHash, &r.DefaultText, &r.Origin, &at); err != nil {
			return nil, fmt.Errorf("store: prompt history %q: %w", part, err)
		}
		if text.Valid {
			t := text.String
			r.Text = &t
		}
		r.At = time.UnixMilli(at)
		out = append(out, r)
	}
	return out, rows.Err()
}
