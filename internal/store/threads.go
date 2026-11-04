package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/core"
)

// PutTrigger inserts or replaces a Trigger.
func (tx *Tx) PutTrigger(t *core.Trigger) error {
	return tx.putDoc("triggers", t.ID, t)
}

// GetTrigger returns the Trigger with the given ID, or nil, nil if there is none.
func (tx *Tx) GetTrigger(id string) (*core.Trigger, error) {
	var t core.Trigger
	ok, err := tx.getDoc("triggers", id, &t)
	if err != nil || !ok {
		return nil, err
	}
	return &t, nil
}

// PutDecision inserts or replaces a Decision.
func (tx *Tx) PutDecision(d *core.Decision) error {
	doc, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("store: encode decision %q: %w", d.ID, err)
	}
	_, err = tx.sql.Exec(`INSERT INTO decisions (id, thread, at, doc) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET thread = excluded.thread, at = excluded.at, doc = excluded.doc`,
		d.ID, d.Thread, d.At.UTC().Format(timeLayout), string(doc))
	if err != nil {
		return fmt.Errorf("store: put decision %q: %w", d.ID, err)
	}
	return nil
}

// GetDecision returns the Decision with the given ID, or nil, nil if there is none.
func (tx *Tx) GetDecision(id string) (*core.Decision, error) {
	var d core.Decision
	ok, err := tx.getDoc("decisions", id, &d)
	if err != nil || !ok {
		return nil, err
	}
	return &d, nil
}

// Decisions returns the Decisions made on a Thread, most recent first; limit <= 0 returns all.
func (tx *Tx) Decisions(thread string, limit int) ([]*core.Decision, error) {
	return tx.decisions(`SELECT doc FROM decisions WHERE thread = ? ORDER BY at DESC, id DESC`, thread, limit, thread)
}

// RecentDecisions returns the Decisions of every Thread, most recent first; limit <= 0 returns all.
func (tx *Tx) RecentDecisions(limit int) ([]*core.Decision, error) {
	return tx.decisions(`SELECT doc FROM decisions WHERE thread != '' ORDER BY at DESC, id DESC`, "every thread", limit)
}

// decisions runs a query of Decision documents; what names its Threads in errors.
func (tx *Tx) decisions(q, thread string, limit int, args ...any) ([]*core.Decision, error) {
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := tx.sql.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: decisions of %q: %w", thread, err)
	}
	defer rows.Close()
	var out []*core.Decision
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("store: decisions of %q: %w", thread, err)
		}
		var d core.Decision
		if err := json.Unmarshal([]byte(doc), &d); err != nil {
			return nil, fmt.Errorf("store: decode decision: %w", err)
		}
		out = append(out, &d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: decisions of %q: %w", thread, err)
	}
	return out, nil
}

// GetThread returns the Thread with the given key, or nil, nil if there is none.
func (tx *Tx) GetThread(key string) (*core.Thread, error) {
	var doc string
	err := tx.sql.QueryRow(`SELECT doc FROM threads WHERE key = ?`, key).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get thread %q: %w", key, err)
	}
	return decodeThread(doc)
}

// PutThread inserts or replaces a Thread.
func (tx *Tx) PutThread(t *core.Thread) error {
	doc, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("store: encode thread %q: %w", t.Key, err)
	}
	active := 0
	if t.Active() {
		active = 1
	}
	_, err = tx.sql.Exec(`INSERT INTO threads (key, active, doc) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET active = excluded.active, doc = excluded.doc`, t.Key, active, string(doc))
	if err != nil {
		return fmt.Errorf("store: put thread %q: %w", t.Key, err)
	}
	return nil
}

// ActiveThreads returns every Thread with Triggers queued or an Eval in progress, ordered by key.
func (tx *Tx) ActiveThreads() ([]*core.Thread, error) {
	rows, err := tx.sql.Query(`SELECT doc FROM threads WHERE active = 1 ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("store: active threads: %w", err)
	}
	defer rows.Close()
	var out []*core.Thread
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("store: active threads: %w", err)
		}
		t, err := decodeThread(doc)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: active threads: %w", err)
	}
	return out, nil
}

func decodeThread(doc string) (*core.Thread, error) {
	var t core.Thread
	if err := json.Unmarshal([]byte(doc), &t); err != nil {
		return nil, fmt.Errorf("store: decode thread: %w", err)
	}
	return &t, nil
}

// KVGet returns the plain string stored under key; the bool is false if there is none. It shares the kv table
// with the store's own JSON values, so Source and router keys must not collide with "schema" or "seq/...".
func (tx *Tx) KVGet(key string) (string, bool, error) {
	var v string
	err := tx.sql.QueryRow(`SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: kv get %q: %w", key, err)
	}
	return v, true, nil
}

// KVSet stores a plain string under key.
func (tx *Tx) KVSet(key, value string) error {
	_, err := tx.sql.Exec(`INSERT INTO kv (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("store: kv set %q: %w", key, err)
	}
	return nil
}

// KVList returns the plain strings stored under keys that start with prefix, by key.
func (tx *Tx) KVList(prefix string) (map[string]string, error) {
	rows, err := tx.sql.Query(`SELECT key, value FROM kv WHERE substr(key, 1, ?) = ?`, utf8.RuneCountInString(prefix), prefix)
	if err != nil {
		return nil, fmt.Errorf("store: kv list %q: %w", prefix, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: kv list %q: %w", prefix, err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: kv list %q: %w", prefix, err)
	}
	return out, nil
}

// KVGet reads a plain string outside any transaction (in a transaction of its own), for Sources' cursors; the bool
// is false if there is none.
func (s *Store) KVGet(key string) (value string, ok bool, err error) {
	err = s.Tx(func(tx *Tx) error {
		var e error
		value, ok, e = tx.KVGet(key)
		return e
	})
	return value, ok, err
}

// KVList lists the keys that start with prefix, outside any transaction (in a transaction of its own).
func (s *Store) KVList(prefix string) (out map[string]string, err error) {
	err = s.Tx(func(tx *Tx) error {
		var e error
		out, e = tx.KVList(prefix)
		return e
	})
	return out, err
}

// KVSet stores a plain string outside any transaction (in a transaction of its own).
func (s *Store) KVSet(key, value string) error {
	return s.Tx(func(tx *Tx) error { return tx.KVSet(key, value) })
}

// Threads returns every stored Thread, ordered by key: the owner's view (athena threads). It reads in a transaction of
// its own.
func (s *Store) Threads() (out []core.Thread, err error) {
	err = s.Tx(func(tx *Tx) error {
		rows, err := tx.sql.Query(`SELECT doc FROM threads ORDER BY key`)
		if err != nil {
			return fmt.Errorf("store: threads: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var doc string
			if err := rows.Scan(&doc); err != nil {
				return fmt.Errorf("store: threads: %w", err)
			}
			t, err := decodeThread(doc)
			if err != nil {
				return err
			}
			out = append(out, *t)
		}
		return rows.Err()
	})
	return out, err
}

// Triggers returns the stored Triggers of a Thread (any of their Threads), or of every Thread when thread is empty,
// newest first; limit <= 0 returns all. It is the owner's view (athena triggers) and reads in a transaction of its own.
// Times are compared as times (a Trigger's At may carry any zone), so the matching rows are sorted here.
func (s *Store) Triggers(thread string, limit int) (out []core.Trigger, err error) {
	err = s.Tx(func(tx *Tx) error {
		rows, err := tx.sql.Query(`SELECT doc FROM triggers
			WHERE ? = '' OR EXISTS (SELECT 1 FROM json_each(doc, '$.threads') WHERE value = ?)`, thread, thread)
		if err != nil {
			return fmt.Errorf("store: triggers of %q: %w", thread, err)
		}
		defer rows.Close()
		for rows.Next() {
			var doc string
			if err := rows.Scan(&doc); err != nil {
				return fmt.Errorf("store: triggers of %q: %w", thread, err)
			}
			var t core.Trigger
			if err := json.Unmarshal([]byte(doc), &t); err != nil {
				return fmt.Errorf("store: decode trigger: %w", err)
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].ID > out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
