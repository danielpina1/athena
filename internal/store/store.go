// Package store persists Athena's domain aggregates in a single SQLite file.
//
// Aggregates are stored as JSON documents (the reference stores JSON rows too), with a few
// columns lifted out for queries. All access goes through Store.Tx: one transaction per Engine command.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// migrations is the ordered list of schema migrations; migration i brings the schema to version i+1.
// Append only: a released migration must never change.
var migrations = []string{
	// 1
	`CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT NOT NULL);
	CREATE TABLE tickets (
		id TEXT PRIMARY KEY,
		ended INTEGER NOT NULL,
		updated_at TEXT NOT NULL,
		doc TEXT NOT NULL
	);
	CREATE INDEX tickets_ended ON tickets (ended);
	CREATE TABLE triggers (id TEXT PRIMARY KEY, doc TEXT NOT NULL);
	CREATE TABLE decisions (id TEXT PRIMARY KEY, doc TEXT NOT NULL);`,
}

const schemaKey = "schema"

// Store is a SQLite-backed repository. It is safe for concurrent use; transactions are serialized.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies pending migrations.
// The parent directory is created 0700 and the file 0600.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: create dir: %w", err)
	}
	// Create the file ourselves so it is 0600 from the start; SQLite gives -wal/-shm the same mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: create file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}

	// Pragmas go in the DSN so they apply to every connection the pool opens.
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "busy_timeout(5000)")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// One connection: SQLite has a single writer, and this serializes every transaction.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	return s.Tx(func(tx *Tx) error {
		current := 0
		var n int
		if err := tx.sql.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='kv'`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			if _, err := tx.KVGet(schemaKey, &current); err != nil {
				return fmt.Errorf("store: read schema version: %w", err)
			}
		}
		if current > len(migrations) {
			return fmt.Errorf("store: database schema %d is newer than this binary supports (%d)", current, len(migrations))
		}
		for i := current; i < len(migrations); i++ {
			if _, err := tx.sql.Exec(migrations[i]); err != nil {
				return fmt.Errorf("store: migration %d: %w", i+1, err)
			}
			if err := tx.KVSet(schemaKey, i+1); err != nil {
				return err
			}
		}
		return nil
	})
}

// Tx runs fn in a transaction: committed if fn returns nil, rolled back if it returns an error or panics
// (a panic is re-raised). Transactions are serialized, so fn must not start another Tx on the same Store.
func (s *Store) Tx(fn func(tx *Tx) error) (err error) {
	sqlTx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if r := recover(); r != nil {
			_ = sqlTx.Rollback()
			panic(r)
		}
	}()
	if err := fn(&Tx{sql: sqlTx}); err != nil {
		if rbErr := sqlTx.Rollback(); rbErr != nil {
			return errors.Join(err, rbErr)
		}
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// Tx is the handle for one transaction; it is valid only inside the function passed to Store.Tx.
type Tx struct {
	sql *sql.Tx
}

// KVGet loads the JSON value stored under key into v. It reports false if key is absent.
func (tx *Tx) KVGet(key string, v any) (bool, error) {
	var raw string
	err := tx.sql.QueryRow(`SELECT value FROM kv WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: kv get %q: %w", key, err)
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return false, fmt.Errorf("store: kv decode %q: %w", key, err)
	}
	return true, nil
}

// KVSet stores v, JSON-encoded, under key.
func (tx *Tx) KVSet(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: kv encode %q: %w", key, err)
	}
	_, err = tx.sql.Exec(`INSERT INTO kv (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, string(raw))
	if err != nil {
		return fmt.Errorf("store: kv set %q: %w", key, err)
	}
	return nil
}
