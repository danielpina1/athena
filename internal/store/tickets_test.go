package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

func endedTicket(id string, at time.Time) *core.Ticket {
	t := sampleTicket(id)
	t.UpdatedAt = at
	t.Ended = &core.Ended{At: at, Outcome: core.OutcomeCompleted}
	return t
}

func TestRecentlyEndedUsesLimit(t *testing.T) {
	s, _ := openTemp(t)
	for i := range 300 {
		put(t, s, endedTicket(fmt.Sprintf("T-%04d", i), t0.Add(time.Duration(i)*time.Minute)))
	}
	open := sampleTicket("T-open")
	open.UpdatedAt = t0.Add(time.Hour * 100)
	put(t, s, open)

	got, err := s.RecentlyEnded(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 100 {
		t.Fatalf("RecentlyEnded(100) = %d rows, want 100", len(got))
	}
	if got[0].ID != "T-0299" || got[99].ID != "T-0200" {
		t.Errorf("rows run %s..%s, want T-0299 down to T-0200 (the newest, never the open Ticket)", got[0].ID, got[99].ID)
	}
	if all, err := s.RecentlyEnded(0); err != nil || len(all) != 300 {
		t.Errorf("RecentlyEnded(0) = %d rows, %v; want all 300", len(all), err)
	}

	// The limit is the database's: the plan reads the index in order, with no sort and no scan of the table.
	plan := queryPlan(t, s, `SELECT doc FROM tickets WHERE ended = 1 ORDER BY updated_at DESC, id DESC LIMIT 100`)
	if !strings.Contains(plan, "tickets_ended_updated") || strings.Contains(plan, "TEMP B-TREE") || strings.Contains(plan, "SCAN tickets") {
		t.Errorf("plan does not use the index in order:\n%s", plan)
	}
}

func queryPlan(t *testing.T, s *Store, q string) string {
	t.Helper()
	var lines []string
	if err := s.Tx(func(tx *Tx) error {
		rows, err := tx.sql.Query(`EXPLAIN QUERY PLAN ` + q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				return err
			}
			lines = append(lines, detail)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

func TestAnsweredApprovalsAreBoundedInSQL(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.Tx(func(tx *Tx) error {
		for i := range 120 {
			a := sampleApproval(fmt.Sprintf("a-%03d", i), "t1", t0.Add(time.Duration(i)*time.Minute))
			a.State = core.ApprovalApproved
			if err := tx.PutApproval(a); err != nil {
				return err
			}
		}
		// One without an answer time counts by its creation; a pending one never appears.
		exp := sampleApproval("a-expired", "t2", t0.Add(500*time.Minute))
		exp.State, exp.AnsweredAt = core.ApprovalExpired, nil
		if err := tx.PutApproval(exp); err != nil {
			return err
		}
		pend := sampleApproval("a-pending", "t1", t0.Add(900*time.Minute))
		pend.AnsweredAt = nil
		return tx.PutApproval(pend)
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.AnsweredApprovals(50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 || got[0].ID != "a-expired" || got[1].ID != "a-119" || got[49].ID != "a-071" {
		var ids []string
		for _, a := range got {
			ids = append(ids, a.ID)
		}
		t.Fatalf("AnsweredApprovals(50) = %d rows %v", len(got), ids)
	}
	if all, _ := s.AnsweredApprovals(0); len(all) != 121 {
		t.Errorf("AnsweredApprovals(0) = %d rows, want 121", len(all))
	}
	plan := queryPlan(t, s, `SELECT doc FROM approvals WHERE state != 'pending' ORDER BY answered_at DESC, id DESC LIMIT 50`)
	if !strings.Contains(plan, "approvals_answered") || strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("answered approvals are sorted outside the index:\n%s", plan)
	}
	mine, err := s.TicketAnsweredApprovals("t2")
	if err != nil || len(mine) != 1 || mine[0].ID != "a-expired" {
		t.Errorf("TicketAnsweredApprovals(t2) = %v, %v", mine, err)
	}
}

func TestMigration5To6BackfillsAnswerTimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "athena.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:5] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO kv (key, value) VALUES ('schema', '5')`); err != nil {
		t.Fatal(err)
	}
	for id, doc := range map[string]string{
		"old":  `{"id":"old","ticketId":"t","state":"denied","createdAt":"2026-03-04T05:06:07Z","answeredAt":"2026-03-04T05:06:09.5+01:00"}`,
		"none": `{"id":"none","ticketId":"t","state":"expired","createdAt":"2026-03-04T05:06:07.25Z"}`,
	} {
		if _, err := db.Exec(`INSERT INTO approvals (id, state, ticket_id, doc) VALUES (?, ?, 't', ?)`, id, "x", doc); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on schema 5: %v", err)
	}
	defer s.Close()
	got := map[string]string{}
	if err := s.Tx(func(tx *Tx) error {
		rows, err := tx.sql.Query(`SELECT id, answered_at FROM approvals`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, at string
			if err := rows.Scan(&id, &at); err != nil {
				return err
			}
			got[id] = at
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if got["old"] != "2026-03-04T04:06:09.500000000Z" || got["none"] != "2026-03-04T05:06:07.250000000Z" {
		t.Errorf("backfilled answer times = %v", got)
	}
}

func TestRecentPlacements(t *testing.T) {
	s, _ := openTemp(t)
	mk := func(id, repo, dir string, created time.Time) {
		tk := sampleTicket(id)
		tk.Repo, tk.CreatedAt, tk.UpdatedAt = repo, created, created
		tk.Execution = nil
		if dir != "" {
			tk.Execution = &core.Execution{Directory: dir}
		}
		put(t, s, tk)
	}
	mk("T-1", "c/d", "", t0)
	mk("T-2", "", "/tmp/x", t0.Add(time.Hour))
	mk("T-3", "", "", t0.Add(2*time.Hour)) // names nothing
	got, err := s.RecentPlacements(200)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Dir != "/tmp/x" || got[1].Repo != "c/d" || !got[1].At.Equal(t0) {
		t.Errorf("placements = %+v", got)
	}
	if few, _ := s.RecentPlacements(1); len(few) != 0 {
		t.Errorf("RecentPlacements(1) took the newest Ticket, which names no place; got %+v", few)
	}
}
