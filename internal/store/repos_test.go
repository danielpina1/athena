package store

import (
	"errors"
	"testing"

	"github.com/danielpina1/athena/internal/core"
)

func TestNextSeqMonotonicPerName(t *testing.T) {
	s, _ := openTemp(t)
	next := func(name string) int {
		t.Helper()
		var n int
		if err := s.Tx(func(tx *Tx) (err error) { n, err = tx.NextSeq(name); return err }); err != nil {
			t.Fatalf("NextSeq(%q): %v", name, err)
		}
		return n
	}
	for want := 1; want <= 3; want++ {
		if got := next("ticket"); got != want {
			t.Fatalf("NextSeq(ticket) = %d, want %d", got, want)
		}
	}
	if got := next("other"); got != 1 {
		t.Fatalf("NextSeq(other) = %d, want 1 (counters are per name)", got)
	}
}

func TestNextSeqMovesOnlyOnCommit(t *testing.T) {
	s, _ := openTemp(t)
	boom := errors.New("boom")
	err := s.Tx(func(tx *Tx) error {
		if _, err := tx.NextSeq("ticket"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx = %v, want boom", err)
	}
	var n int
	if err := s.Tx(func(tx *Tx) (err error) { n, err = tx.NextSeq("ticket"); return err }); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("NextSeq after rollback = %d, want 1", n)
	}
}

func TestAllTicketsIncludesEnded(t *testing.T) {
	s, _ := openTemp(t)
	open := sampleTicket("T-1")
	ended := sampleTicket("T-2")
	ended.UpdatedAt = open.UpdatedAt.Add(-1)
	ended.Ended = &core.Ended{At: t0, By: core.EndByHuman, Outcome: core.OutcomeStopped}
	put(t, s, open)
	put(t, s, ended)
	var all, openList []*core.Ticket
	if err := s.Tx(func(tx *Tx) (err error) {
		if all, err = tx.AllTickets(); err != nil {
			return err
		}
		openList, err = tx.OpenTickets()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != "T-2" || all[1].ID != "T-1" {
		t.Fatalf("AllTickets = %v, want [T-2 T-1] (least recently updated first)", ids(all))
	}
	if len(openList) != 1 || openList[0].ID != "T-1" {
		t.Fatalf("OpenTickets = %v, want [T-1]", ids(openList))
	}
}

func TestReposRunsEngine(t *testing.T) {
	s, _ := openTemp(t)
	eng := core.NewEngine(s.Repos(), nopExecutor{}, nil, nil)
	a, err := eng.SubmitOwnerTask("first task", core.TaskOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := eng.SubmitOwnerTask("second task", core.TaskOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "T-0001" || b.ID != "T-0002" {
		t.Fatalf("ids = %s, %s; want T-0001, T-0002", a.ID, b.ID)
	}
	if err := eng.StopTicket(a.ID, "no"); err != nil {
		t.Fatal(err)
	}
	all, err := eng.Tickets(true)
	if err != nil {
		t.Fatal(err)
	}
	open, err := eng.Tickets(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || len(open) != 1 || open[0].ID != b.ID {
		t.Fatalf("all = %d, open = %v; want 2 and [%s]", len(all), open, b.ID)
	}
}

type nopExecutor struct{}

func (nopExecutor) Start(core.Ticket, core.Assignment)        {}
func (nopExecutor) Deliver(core.Ticket, core.Assignment)      {}
func (nopExecutor) Stop(core.Ticket, core.Assignment, string) {}

func ids(ts []*core.Ticket) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.ID
	}
	return out
}
