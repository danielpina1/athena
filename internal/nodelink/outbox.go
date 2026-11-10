package nodelink

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// dbFile is the node's own database under its state directory: the outbox of facts.
const dbFile = "node.db"

// outbox holds the node's facts until the server has applied them. A fact's seq is its fact_seq on the wire; rows at
// or below the server's applied mark are deleted.
type outbox struct{ db *sql.DB }

type factRow struct {
	seq    uint64
	method string
	args   []byte
}

func openOutbox(dir string) (*outbox, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("nodelink: state directory: %w", err)
	}
	path := filepath.Join(dir, dbFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("nodelink: create %s: %w", dbFile, err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "busy_timeout(5000)")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("nodelink: open %s: %w", dbFile, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS outbox (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		method TEXT NOT NULL,
		args BLOB NOT NULL,
		made_at TEXT NOT NULL)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("nodelink: migrate %s: %w", dbFile, err)
	}
	return &outbox{db: db}, nil
}

func (o *outbox) close() error { return o.db.Close() }

// append commits one fact and returns its seq.
func (o *outbox) append(method string, args []byte, at time.Time) (uint64, error) {
	res, err := o.db.Exec(`INSERT INTO outbox (method, args, made_at) VALUES (?, ?, ?)`,
		method, args, at.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("nodelink: outbox: %w", err)
	}
	id, err := res.LastInsertId()
	return uint64(id), err
}

// applied deletes the facts through seq, and makes sure the next fact is numbered after it: a server that applied
// more than this outbox ever numbered (a node.db lost or replaced) must not skip the node's next facts as old.
func (o *outbox) applied(seq uint64) error {
	tx, err := o.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM outbox WHERE seq <= ?`, seq); err != nil {
		return err
	}
	var last sql.NullInt64
	if err := tx.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'outbox'`).Scan(&last); err != nil && err != sql.ErrNoRows {
		return err
	}
	if !last.Valid {
		if _, err := tx.Exec(`INSERT INTO sqlite_sequence (name, seq) VALUES ('outbox', ?)`, seq); err != nil {
			return err
		}
	} else if uint64(last.Int64) < seq {
		if _, err := tx.Exec(`UPDATE sqlite_sequence SET seq = ? WHERE name = 'outbox'`, seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// deleteThrough deletes the facts through seq, once the server answered them.
func (o *outbox) deleteThrough(seq uint64) error {
	_, err := o.db.Exec(`DELETE FROM outbox WHERE seq <= ?`, seq)
	return err
}

// after returns at most limit facts above seq, in seq order.
func (o *outbox) after(seq uint64, limit int) ([]factRow, error) {
	rows, err := o.db.Query(`SELECT seq, method, args FROM outbox WHERE seq > ? ORDER BY seq LIMIT ?`, seq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []factRow
	for rows.Next() {
		var r factRow
		if err := rows.Scan(&r.seq, &r.method, &r.args); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
