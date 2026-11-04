package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Changes are the records one committed transaction wrote: the ids of the Tickets, Approvals, Triggers, Threads
// (by key) and Decisions it put, each once, in the order first written, and whether it saved a Policy revision.
type Changes struct {
	Tickets, Approvals, Triggers, Threads, Decisions []string
	Policy                                           bool
}

func (c *Changes) empty() bool {
	return !c.Policy && len(c.Tickets)+len(c.Approvals)+len(c.Triggers)+len(c.Threads)+len(c.Decisions) == 0
}

// addID appends id to ids unless it is already there.
func addID(ids []string, id string) []string {
	if slices.Contains(ids, id) {
		return ids
	}
	return append(ids, id)
}

// OnCommit sets the function called with the Changes of every transaction that commits and wrote any. It runs on the
// committing goroutine after the commit, so it must not block or start a transaction; a rolled-back transaction reports
// nothing. A nil f removes the hook.
func (s *Store) OnCommit(f func(Changes)) {
	if f == nil {
		s.onCommit.Store(nil)
		return
	}
	s.onCommit.Store(&f)
}

func (s *Store) report(c Changes) {
	if f := s.onCommit.Load(); f != nil && !c.empty() {
		(*f)(c)
	}
}

// Session lifetimes (spec §11): a web session ends after 30 days unseen or 90 days from its creation.
const (
	WebSessionIdle     = 30 * 24 * time.Hour
	WebSessionAbsolute = 90 * 24 * time.Hour
)

// formatTime and parseTime are the stored form of the web tables' times. The zero time is stored as "".
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeLayout)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: parse time %q: %w", s, err)
	}
	return t, nil
}

// Passkey is an enrolled WebAuthn credential; Credential is its serialized form.
type Passkey struct {
	ID         string
	Name       string
	Credential []byte
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// PutPasskey inserts a Passkey, or replaces the one with the same id.
func (s *Store) PutPasskey(p Passkey) error {
	_, err := s.db.Exec(`INSERT INTO passkeys (id, name, credential, created_at, last_used_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, credential = excluded.credential,
			created_at = excluded.created_at, last_used_at = excluded.last_used_at`,
		p.ID, p.Name, p.Credential, formatTime(p.CreatedAt), formatTime(p.LastUsedAt))
	if err != nil {
		return fmt.Errorf("store: put passkey: %w", err)
	}
	return nil
}

// Passkey returns the Passkey with the given id; the bool is false if there is none.
func (s *Store) Passkey(id string) (Passkey, bool, error) {
	rows, err := s.passkeys(`WHERE id = ?`, id)
	if err != nil || len(rows) == 0 {
		return Passkey{}, false, err
	}
	return rows[0], true, nil
}

// Passkeys returns every Passkey, oldest first.
func (s *Store) Passkeys() ([]Passkey, error) { return s.passkeys(``) }

func (s *Store) passkeys(where string, args ...any) ([]Passkey, error) {
	rows, err := s.db.Query(`SELECT id, name, credential, created_at, last_used_at FROM passkeys `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: passkeys: %w", err)
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		var p Passkey
		var created, used string
		if err := rows.Scan(&p.ID, &p.Name, &p.Credential, &created, &used); err != nil {
			return nil, fmt.Errorf("store: passkeys: %w", err)
		}
		if p.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if p.LastUsedAt, err = parseTime(used); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// TouchPasskey records a use of the Passkey at the given time and replaces its stored credential (a login moves the
// credential's sign counter).
func (s *Store) TouchPasskey(id string, at time.Time, credential []byte) error {
	_, err := s.db.Exec(`UPDATE passkeys SET last_used_at = ?, credential = ? WHERE id = ?`, formatTime(at), credential, id)
	if err != nil {
		return fmt.Errorf("store: touch passkey: %w", err)
	}
	return nil
}

// DeletePasskey removes a Passkey and, in the same transaction, every web session it opened (the foreign key cascades),
// and returns the hashes of those sessions. A Passkey that does not exist deletes nothing and returns none.
func (s *Store) DeletePasskey(id string) ([][]byte, error) {
	var hashes [][]byte
	err := s.Tx(func(tx *Tx) error {
		rows, err := tx.sql.Query(`SELECT hash FROM web_sessions WHERE passkey_id = ? ORDER BY created_at, hash`, id)
		if err != nil {
			return fmt.Errorf("store: delete passkey sessions: %w", err)
		}
		for rows.Next() {
			var h []byte
			if err := rows.Scan(&h); err != nil {
				rows.Close()
				return fmt.Errorf("store: delete passkey sessions: %w", err)
			}
			hashes = append(hashes, h)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if _, err := tx.sql.Exec(`DELETE FROM web_sessions WHERE passkey_id = ?`, id); err != nil {
			return fmt.Errorf("store: delete passkey sessions: %w", err)
		}
		if _, err := tx.sql.Exec(`DELETE FROM passkeys WHERE id = ?`, id); err != nil {
			return fmt.Errorf("store: delete passkey: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return hashes, nil
}

// WebSession is a logged-in browser. Hash is the SHA-256 of the cookie value, which the store never sees.
type WebSession struct {
	Hash      []byte
	CreatedAt time.Time
	SeenAt    time.Time
	// ReauthAt is when a passkey assertion last vouched for the session.
	ReauthAt  time.Time
	PasskeyID string
}

// PutWebSession inserts a web session, or replaces the one with the same hash. The session's Passkey must exist: a
// revoke that committed first makes this fail (foreign key), so no session outlives its passkey.
func (s *Store) PutWebSession(w WebSession) error {
	_, err := s.db.Exec(`INSERT INTO web_sessions (hash, created_at, seen_at, reauth_at, passkey_id) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(hash) DO UPDATE SET created_at = excluded.created_at, seen_at = excluded.seen_at,
			reauth_at = excluded.reauth_at, passkey_id = excluded.passkey_id`,
		w.Hash, formatTime(w.CreatedAt), formatTime(w.SeenAt), formatTime(w.ReauthAt), w.PasskeyID)
	if err != nil {
		return fmt.Errorf("store: put web session: %w", err)
	}
	return nil
}

// WebSession returns the web session with the given cookie hash; the bool is false if there is none.
func (s *Store) WebSession(hash []byte) (WebSession, bool, error) {
	var w WebSession
	var created, seen, reauth string
	err := s.db.QueryRow(`SELECT hash, created_at, seen_at, reauth_at, passkey_id FROM web_sessions WHERE hash = ?`, hash).
		Scan(&w.Hash, &created, &seen, &reauth, &w.PasskeyID)
	if errors.Is(err, sql.ErrNoRows) {
		return WebSession{}, false, nil
	}
	if err != nil {
		return WebSession{}, false, fmt.Errorf("store: web session: %w", err)
	}
	var perr error
	if w.CreatedAt, perr = parseTime(created); perr != nil {
		return WebSession{}, false, perr
	}
	if w.SeenAt, perr = parseTime(seen); perr != nil {
		return WebSession{}, false, perr
	}
	if w.ReauthAt, perr = parseTime(reauth); perr != nil {
		return WebSession{}, false, perr
	}
	return w, true, nil
}

// WebSessionsOf returns the web sessions the Passkey opened, oldest first.
func (s *Store) WebSessionsOf(passkeyID string) ([]WebSession, error) {
	rows, err := s.db.Query(`SELECT hash, created_at, seen_at, reauth_at FROM web_sessions WHERE passkey_id = ? ORDER BY created_at, hash`, passkeyID)
	if err != nil {
		return nil, fmt.Errorf("store: web sessions of passkey: %w", err)
	}
	defer rows.Close()
	var out []WebSession
	for rows.Next() {
		w := WebSession{PasskeyID: passkeyID}
		var created, seen, reauth string
		if err := rows.Scan(&w.Hash, &created, &seen, &reauth); err != nil {
			return nil, fmt.Errorf("store: web sessions of passkey: %w", err)
		}
		if w.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if w.SeenAt, err = parseTime(seen); err != nil {
			return nil, err
		}
		if w.ReauthAt, err = parseTime(reauth); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// TouchWebSession sets when a web session was last seen and last reauthenticated.
func (s *Store) TouchWebSession(hash []byte, seenAt, reauthAt time.Time) error {
	_, err := s.db.Exec(`UPDATE web_sessions SET seen_at = ?, reauth_at = ? WHERE hash = ?`, formatTime(seenAt), formatTime(reauthAt), hash)
	if err != nil {
		return fmt.Errorf("store: touch web session: %w", err)
	}
	return nil
}

// DeleteWebSession removes a web session (a logout).
func (s *Store) DeleteWebSession(hash []byte) error {
	if _, err := s.db.Exec(`DELETE FROM web_sessions WHERE hash = ?`, hash); err != nil {
		return fmt.Errorf("store: delete web session: %w", err)
	}
	return nil
}

// PruneWebSessions removes the web sessions that are idle past WebSessionIdle or older than WebSessionAbsolute at
// now, and returns how many.
func (s *Store) PruneWebSessions(now time.Time) (int, error) {
	res, err := s.db.Exec(`DELETE FROM web_sessions WHERE seen_at < ? OR created_at < ?`,
		formatTime(now.Add(-WebSessionIdle)), formatTime(now.Add(-WebSessionAbsolute)))
	return rowsAffected(res, err, "prune web sessions")
}

// PutEnrollToken stores the hash of an enrollment token that is good until expiresAt.
func (s *Store) PutEnrollToken(hash []byte, expiresAt time.Time) error {
	_, err := s.db.Exec(`INSERT INTO enroll_tokens (hash, expires_at) VALUES (?, ?)`, hash, formatTime(expiresAt))
	if err != nil {
		return fmt.Errorf("store: put enroll token: %w", err)
	}
	return nil
}

// UseEnrollToken spends an enrollment token: true if the hash names a token that is unspent and not expired at now,
// which is then spent, in one statement, so two concurrent uses cannot both succeed.
func (s *Store) UseEnrollToken(hash []byte, now time.Time) (bool, error) {
	res, err := s.db.Exec(`UPDATE enroll_tokens SET used_at = ? WHERE hash = ? AND used_at = '' AND expires_at > ?`,
		formatTime(now), hash, formatTime(now))
	n, err := rowsAffected(res, err, "use enroll token")
	return n > 0, err
}

// PutCommandResult records the result of a command under (clientID, commandID) and returns the result now stored. A
// key that already has one keeps it, and its result is returned: a command resent by a client acts once.
func (s *Store) PutCommandResult(clientID, commandID string, result []byte, at time.Time) ([]byte, error) {
	var stored []byte
	err := s.Tx(func(tx *Tx) error {
		if _, err := tx.sql.Exec(`INSERT OR IGNORE INTO command_results (client_id, command_id, result, at) VALUES (?, ?, ?, ?)`,
			clientID, commandID, result, formatTime(at)); err != nil {
			return fmt.Errorf("store: put command result: %w", err)
		}
		return tx.sql.QueryRow(`SELECT result FROM command_results WHERE client_id = ? AND command_id = ?`, clientID, commandID).Scan(&stored)
	})
	if err != nil {
		return nil, err
	}
	return stored, nil
}

// CommandResult returns the stored result of a command; the bool is false if there is none.
func (s *Store) CommandResult(clientID, commandID string) ([]byte, bool, error) {
	var result []byte
	err := s.db.QueryRow(`SELECT result FROM command_results WHERE client_id = ? AND command_id = ?`, clientID, commandID).Scan(&result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: command result: %w", err)
	}
	return result, true, nil
}

// PruneCommandResults removes the results recorded before the given time and returns how many.
func (s *Store) PruneCommandResults(before time.Time) (int, error) {
	res, err := s.db.Exec(`DELETE FROM command_results WHERE at < ?`, formatTime(before))
	return rowsAffected(res, err, "prune command results")
}

// PushSubscription is a browser's Web Push subscription; Failures counts consecutive failed deliveries.
type PushSubscription struct {
	Endpoint  string
	P256dh    string
	Auth      string
	CreatedAt time.Time
	Failures  int
}

// PutPushSubscription stores a subscription, replacing the one for the same endpoint and resetting its failures.
func (s *Store) PutPushSubscription(p PushSubscription) error {
	_, err := s.db.Exec(`INSERT INTO push_subscriptions (endpoint, p256dh, auth, created_at, failures) VALUES (?, ?, ?, ?, 0)
		ON CONFLICT(endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth,
			created_at = excluded.created_at, failures = 0`,
		p.Endpoint, p.P256dh, p.Auth, formatTime(p.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: put push subscription: %w", err)
	}
	return nil
}

// PushSubscriptions returns every subscription, oldest first.
func (s *Store) PushSubscriptions() ([]PushSubscription, error) {
	rows, err := s.db.Query(`SELECT endpoint, p256dh, auth, created_at, failures FROM push_subscriptions ORDER BY created_at, endpoint`)
	if err != nil {
		return nil, fmt.Errorf("store: push subscriptions: %w", err)
	}
	defer rows.Close()
	var out []PushSubscription
	for rows.Next() {
		var p PushSubscription
		var created string
		if err := rows.Scan(&p.Endpoint, &p.P256dh, &p.Auth, &created, &p.Failures); err != nil {
			return nil, fmt.Errorf("store: push subscriptions: %w", err)
		}
		if p.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// BumpPushFailures counts one more failed delivery to the endpoint and returns the new count (0 if it is unknown).
func (s *Store) BumpPushFailures(endpoint string) (int, error) {
	var n int
	err := s.db.QueryRow(`UPDATE push_subscriptions SET failures = failures + 1 WHERE endpoint = ? RETURNING failures`, endpoint).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: bump push failures: %w", err)
	}
	return n, nil
}

// ResetPushFailures zeroes the failure count of the endpoint's subscription (a delivery succeeded).
func (s *Store) ResetPushFailures(endpoint string) error {
	if _, err := s.db.Exec(`UPDATE push_subscriptions SET failures = 0 WHERE endpoint = ?`, endpoint); err != nil {
		return fmt.Errorf("store: reset push failures: %w", err)
	}
	return nil
}

// DeletePushSubscription removes the subscription for an endpoint.
func (s *Store) DeletePushSubscription(endpoint string) error {
	if _, err := s.db.Exec(`DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint); err != nil {
		return fmt.Errorf("store: delete push subscription: %w", err)
	}
	return nil
}

func rowsAffected(res sql.Result, err error, what string) (int, error) {
	if err != nil {
		return 0, fmt.Errorf("store: %s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: %s: %w", what, err)
	}
	return int(n), nil
}
