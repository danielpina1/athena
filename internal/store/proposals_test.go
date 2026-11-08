package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

func TestProposalsRoundTripAndReopen(t *testing.T) {
	s, path := openTemp(t)
	at := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	mk := func(id, ticket, state string, plus int) *core.Proposal {
		return &core.Proposal{ID: id, TicketID: ticket, Repo: "o/r", Kind: core.ProposalPush, State: state, Summary: "s " + id,
			Payload: json.RawMessage(`{"branch":"b","sha":"1"}`), CreatedAt: at.Add(time.Duration(plus) * time.Minute)}
	}
	var seen Changes
	s.OnCommit(func(c Changes) { seen = c })
	mustOK(t, s.Tx(func(tx *Tx) error {
		for _, p := range []*core.Proposal{mk("b", "T1", core.ProposalPending, 2), mk("a", "T1", core.ProposalPending, 1),
			mk("c", "T1", core.ProposalApplied, 3), mk("d", "T2", core.ProposalPending, 4)} {
			if err := tx.PutProposal(p); err != nil {
				return err
			}
		}
		return nil
	}))
	if len(seen.Proposals) != 4 || seen.empty() {
		t.Errorf("changes: %+v", seen)
	}
	check := func(s *Store) {
		t.Helper()
		mustOK(t, s.Tx(func(tx *Tx) error {
			got, err := tx.GetProposal("a")
			if err != nil || got == nil || got.Summary != "s a" || string(got.Payload) != `{"branch":"b","sha":"1"}` || !got.CreatedAt.Equal(at.Add(time.Minute)) {
				t.Errorf("round trip: %+v %v", got, err)
			}
			if none, err := tx.GetProposal("nope"); err != nil || none != nil {
				t.Errorf("missing: %v %v", none, err)
			}
			pend, err := tx.PendingProposals()
			if err != nil || len(pend) != 3 || pend[0].ID != "a" || pend[1].ID != "b" || pend[2].ID != "d" {
				t.Errorf("pending: %+v %v", pend, err)
			}
			all, err := tx.TicketProposals("T1")
			if err != nil || len(all) != 3 || all[2].ID != "c" {
				t.Errorf("ticket proposals: %+v %v", all, err)
			}
			return nil
		}))
	}
	check(s)
	mustOK(t, s.Close())
	s2, err := Open(path)
	mustOK(t, err)
	t.Cleanup(func() { s2.Close() })
	check(s2)
}
