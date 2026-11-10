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
// or below the server's applied mark are deleted once sent. A row's sent flag is set before it first goes on a link:
// a row never sent at or below the mark (facts made on a fresh node.db before its first Welcome, while the server's
// mark counts an older node.db's facts) is numbered again above the mark instead.
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
		made_at TEXT NOT NULL,
		sent INTEGER NOT NULL DEFAULT 1)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("nodelink: migrate %s: %w", dbFile, err)
	}
	// An outbox made before the sent flag: its rows count as sent (the default), so none is ever applied twice.
	var hasSent int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('outbox') WHERE name = 'sent'`).Scan(&hasSent); err != nil {
		db.Close()
		return nil, fmt.Errorf("nodelink: migrate %s: %w", dbFile, err)
	}
	if hasSent == 0 {
		if _, err := db.Exec(`ALTER TABLE outbox ADD COLUMN sent INTEGER NOT NULL DEFAULT 1`); err != nil {
			db.Close()
			return nil, fmt.Errorf("nodelink: migrate %s: %w", dbFile, err)
		}
	}
	// The checkpoint of each Ticket the server last took: the next bundle goes after it.
	// too_big marks a Ticket whose bundle went over the bound: none is built for it again.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS checkpoints (
		ticket_id TEXT PRIMARY KEY,
		sha TEXT NOT NULL,
		too_big INTEGER NOT NULL DEFAULT 0)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("nodelink: migrate %s: %w", dbFile, err)
	}
	return &outbox{db: db}, nil
}

func (o *outbox) close() error { return o.db.Close() }

// append commits one fact and returns its seq.
func (o *outbox) append(method string, args []byte, at time.Time) (uint64, error) {
	res, err := o.db.Exec(`INSERT INTO outbox (method, args, made_at, sent) VALUES (?, ?, ?, 0)`,
		method, args, at.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("nodelink: outbox: %w", err)
	}
	id, err := res.LastInsertId()
	return uint64(id), err
}

// applied deletes the sent facts through seq, and makes sure the next fact is numbered after it: a server that
// applied more than this outbox ever numbered (a node.db lost or replaced) must not skip the node's next facts as old.
// Facts at or below seq that never went on a link were not applied: they are numbered again above every row, in their
// order, with the rows after them (none of which was sent either, since facts go in seq order). renumbered maps each
// moved fact's old seq to its new one.
func (o *outbox) applied(seq uint64) (renumbered map[uint64]uint64, err error) {
	tx, err := o.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM outbox WHERE seq <= ? AND sent = 1`, seq); err != nil {
		return nil, err
	}
	var low sql.NullInt64
	if err := tx.QueryRow(`SELECT MIN(seq) FROM outbox WHERE seq <= ?`, seq).Scan(&low); err != nil {
		return nil, err
	}
	var top sql.NullInt64
	if err := tx.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'outbox'`).Scan(&top); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	next := max(seq, uint64(max(top.Int64, 0)))
	if low.Valid {
		rows, err := tx.Query(`SELECT seq FROM outbox WHERE seq >= ? ORDER BY seq`, low.Int64)
		if err != nil {
			return nil, err
		}
		var old []uint64
		for rows.Next() {
			var s uint64
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return nil, err
			}
			old = append(old, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		// Each new seq is above every row, so no update meets another row's seq.
		renumbered = map[uint64]uint64{}
		for _, s := range old {
			next++
			if _, err := tx.Exec(`UPDATE outbox SET seq = ? WHERE seq = ?`, next, s); err != nil {
				return nil, err
			}
			renumbered[s] = next
		}
	}
	if !top.Valid {
		if _, err := tx.Exec(`INSERT INTO sqlite_sequence (name, seq) VALUES ('outbox', ?)`, next); err != nil {
			return nil, err
		}
	} else if uint64(top.Int64) < next {
		if _, err := tx.Exec(`UPDATE sqlite_sequence SET seq = ? WHERE name = 'outbox'`, next); err != nil {
			return nil, err
		}
	}
	return renumbered, tx.Commit()
}

// markSent flags the facts in (after, through] as sent, before they go on a link.
func (o *outbox) markSent(after, through uint64) error {
	_, err := o.db.Exec(`UPDATE outbox SET sent = 1 WHERE seq > ? AND seq <= ? AND sent = 0`, after, through)
	return err
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

// checkpointSent is the Ticket's checkpoint the server last took, or "", and whether its bundle went over the bound.
func (o *outbox) checkpointSent(ticketID string) (sha string, tooBig bool, err error) {
	err = o.db.QueryRow(`SELECT sha, too_big FROM checkpoints WHERE ticket_id = ?`, ticketID).Scan(&sha, &tooBig)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return sha, tooBig, err
}

// setCheckpointTooBig marks the Ticket's checkpoint as over the bound.
func (o *outbox) setCheckpointTooBig(ticketID string) error {
	_, err := o.db.Exec(`INSERT INTO checkpoints (ticket_id, sha, too_big) VALUES (?, '', 1)
		ON CONFLICT (ticket_id) DO UPDATE SET too_big = 1`, ticketID)
	return err
}

// setCheckpointSent records the Ticket's checkpoint the server took; "" forgets it, so the next bundle is whole.
func (o *outbox) setCheckpointSent(ticketID, sha string) error {
	_, err := o.db.Exec(`INSERT INTO checkpoints (ticket_id, sha) VALUES (?, ?)
		ON CONFLICT (ticket_id) DO UPDATE SET sha = excluded.sha`, ticketID, sha)
	return err
}
