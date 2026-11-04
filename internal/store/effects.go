package store

import (
	"database/sql"
	"errors"
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
	due := tx.now().UTC().Format(timeLayout)
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

// SetClock makes the Store read the current time from now (for the due time of enqueued effects and for the effects
// runner). A nil now restores time.Now. It exists so tests can control backoff.
func (s *Store) SetClock(now func() time.Time) {
	if now == nil {
		s.clock.Store(nil)
		return
	}
	s.clock.Store(&now)
}

// Now returns the Store's current time, time.Now unless SetClock changed it.
func (s *Store) Now() time.Time {
	if c := s.clock.Load(); c != nil {
		return (*c)()
	}
	return time.Now()
}

// Effect is one row of the effects table.
type Effect struct {
	Kind, Key string
	Payload   []byte
	Seq       int
	DueAt     time.Time
	Attempts  int
	LastError string
}

const effectColumns = `kind, key, payload, seq, due_at, attempts, last_error`

type scanner interface{ Scan(dest ...any) error }

func scanEffect(r scanner) (Effect, error) {
	var e Effect
	var due string
	if err := r.Scan(&e.Kind, &e.Key, &e.Payload, &e.Seq, &due, &e.Attempts, &e.LastError); err != nil {
		return Effect{}, err
	}
	if e.Payload == nil {
		e.Payload = []byte{}
	}
	var err error
	if e.DueAt, err = time.Parse(timeLayout, due); err != nil {
		return Effect{}, fmt.Errorf("store: effect %s/%s due_at: %w", e.Kind, e.Key, err)
	}
	return e, nil
}

func queryEffects(tx *Tx, query string, args ...any) ([]Effect, error) {
	rows, err := tx.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: effects: %w", err)
	}
	defer rows.Close()
	var out []Effect
	for rows.Next() {
		e, err := scanEffect(rows)
		if err != nil {
			return nil, fmt.Errorf("store: effects: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: effects: %w", err)
	}
	return out, nil
}

// DueEffects returns up to limit effects of kind whose due time is not after now, oldest seq first.
func (s *Store) DueEffects(kind string, now time.Time, limit int) (out []Effect, err error) {
	err = s.Tx(func(tx *Tx) (err error) {
		out, err = queryEffects(tx, `SELECT `+effectColumns+` FROM effects WHERE kind = ? AND due_at <= ?
			ORDER BY seq LIMIT ?`, kind, now.UTC().Format(timeLayout), limit)
		return err
	})
	return out, err
}

// HeadEffect returns the effect of kind with the lowest seq, due or not; ok is false if the kind has none.
func (s *Store) HeadEffect(kind string) (e Effect, ok bool, err error) {
	err = s.Tx(func(tx *Tx) error {
		var rows []Effect
		rows, err := queryEffects(tx, `SELECT `+effectColumns+` FROM effects WHERE kind = ? ORDER BY seq LIMIT 1`, kind)
		if len(rows) > 0 {
			e, ok = rows[0], true
		}
		return err
	})
	return e, ok, err
}

// NextEffectDue returns the earliest due time of kind's effects that is after now; ok is false if there is none.
func (s *Store) NextEffectDue(kind string, now time.Time) (due time.Time, ok bool, err error) {
	err = s.Tx(func(tx *Tx) error {
		var v sql.NullString
		if err := tx.sql.QueryRow(`SELECT MIN(due_at) FROM effects WHERE kind = ? AND due_at > ?`,
			kind, now.UTC().Format(timeLayout)).Scan(&v); err != nil {
			return fmt.Errorf("store: next effect due: %w", err)
		}
		if !v.Valid {
			return nil
		}
		var perr error
		if due, perr = time.Parse(timeLayout, v.String); perr != nil {
			return fmt.Errorf("store: next effect due: %w", perr)
		}
		ok = true
		return nil
	})
	return due, ok, err
}

// KindEffects returns every effect of kind, due or not, oldest seq first.
func (s *Store) KindEffects(kind string) (out []Effect, err error) {
	err = s.Tx(func(tx *Tx) (err error) {
		out, err = queryEffects(tx, `SELECT `+effectColumns+` FROM effects WHERE kind = ? ORDER BY seq`, kind)
		return err
	})
	return out, err
}

// ListEffects returns every effect, oldest seq first (for health and tests).
func (s *Store) ListEffects() (out []Effect, err error) {
	err = s.Tx(func(tx *Tx) (err error) {
		out, err = queryEffects(tx, `SELECT `+effectColumns+` FROM effects ORDER BY seq`)
		return err
	})
	return out, err
}

// FailingEffects returns the effects that have failed at least minAttempts times, oldest seq first.
func (s *Store) FailingEffects(minAttempts int) (out []Effect, err error) {
	err = s.Tx(func(tx *Tx) (err error) {
		out, err = queryEffects(tx, `SELECT `+effectColumns+` FROM effects WHERE attempts >= ? ORDER BY seq`, minAttempts)
		return err
	})
	return out, err
}

// The three methods below settle an effect that was run with the given payload. Each acts only if the row still holds
// that payload: if Enqueue replaced it meanwhile, the newer message must stay and run, so the settle is a no-op.

// CompleteEffect deletes the effect (kind, key) if it still holds payload and reports whether it did.
func (s *Store) CompleteEffect(kind, key string, payload []byte) (done bool, err error) {
	err = s.Tx(func(tx *Tx) error {
		res, err := tx.sql.Exec(`DELETE FROM effects WHERE kind = ? AND key = ? AND payload = ?`, kind, key, nonNil(payload))
		if err != nil {
			return fmt.Errorf("store: complete effect %s/%s: %w", kind, key, err)
		}
		n, err := res.RowsAffected()
		done = n > 0
		return err
	})
	return done, err
}

// DelayEffect makes the effect (kind, key) due at due without counting a failure.
func (s *Store) DelayEffect(kind, key string, payload []byte, due time.Time) (done bool, err error) {
	err = s.Tx(func(tx *Tx) error {
		res, err := tx.sql.Exec(`UPDATE effects SET due_at = ? WHERE kind = ? AND key = ? AND payload = ?`,
			due.UTC().Format(timeLayout), kind, key, nonNil(payload))
		if err != nil {
			return fmt.Errorf("store: delay effect %s/%s: %w", kind, key, err)
		}
		n, err := res.RowsAffected()
		done = n > 0
		return err
	})
	return done, err
}

// FailEffect records a failed run of the effect (kind, key): attempts goes up by one, last_error is set and the effect
// becomes due at now + delay(attempts) (attempts counting this failure, 1-based). It returns the new attempts and
// due time, and attempts 0 if the effect was replaced or deleted meanwhile.
func (s *Store) FailEffect(kind, key string, payload []byte, lastError string, now time.Time,
	delay func(attempts int) time.Duration) (attempts int, due time.Time, err error) {
	err = s.Tx(func(tx *Tx) error {
		var n int
		err := tx.sql.QueryRow(`SELECT attempts FROM effects WHERE kind = ? AND key = ? AND payload = ?`,
			kind, key, nonNil(payload)).Scan(&n)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: fail effect %s/%s: %w", kind, key, err)
		}
		attempts = n + 1
		due = now.Add(delay(attempts))
		if _, err := tx.sql.Exec(`UPDATE effects SET attempts = ?, last_error = ?, due_at = ? WHERE kind = ? AND key = ?`,
			attempts, lastError, due.UTC().Format(timeLayout), kind, key); err != nil {
			return fmt.Errorf("store: fail effect %s/%s: %w", kind, key, err)
		}
		return nil
	})
	if err != nil {
		return 0, time.Time{}, err
	}
	return attempts, due, nil
}

// nonNil keeps an empty payload bound as an empty blob, not NULL.
func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}
