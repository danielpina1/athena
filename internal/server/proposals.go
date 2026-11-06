package server

import (
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/store"
)

// proposalDetail is what the owner decides on: the held reply's text, "branch @ sha7" for a push, and the title and
// base of a pull request. A payload that cannot be read gives no detail.
func proposalDetail(tx *store.Tx, p *core.Proposal) string {
	switch p.Kind {
	case core.ProposalReply:
		var pl struct {
			ReceiptID string `json:"receiptId"`
		}
		if json.Unmarshal(p.Payload, &pl) != nil {
			return ""
		}
		if rc, err := tx.GetReceipt(pl.ReceiptID); err == nil && rc != nil {
			return rc.Text
		}
	case core.ProposalPush:
		var pl struct{ Branch, Sha string }
		if json.Unmarshal(p.Payload, &pl) != nil {
			return ""
		}
		sha := pl.Sha
		if len(sha) > 7 {
			sha = sha[:7]
		}
		return pl.Branch + " @ " + sha
	case core.ProposalPull:
		var pl struct{ Title, Base string }
		if json.Unmarshal(p.Payload, &pl) != nil {
			return ""
		}
		return pl.Title + " → " + pl.Base
	}
	return ""
}

func proposalView(tx *store.Tx, p *core.Proposal) *athenav1.ProposalView {
	return &athenav1.ProposalView{Id: p.ID, TicketId: p.TicketID, Repo: p.Repo, Kind: p.Kind, State: p.State,
		Summary: p.Summary, Detail: proposalDetail(tx, p), CreatedAt: timestamppb.New(p.CreatedAt), Error: p.Error}
}

// Proposals returns the pending Proposals, oldest first.
func (s hubSource) Proposals() ([]*athenav1.ProposalView, error) {
	var out []*athenav1.ProposalView
	err := s.d.st.Tx(func(tx *store.Tx) error {
		list, err := tx.PendingProposals()
		for _, p := range list {
			out = append(out, proposalView(tx, p))
		}
		return err
	})
	return out, err
}

// Proposal returns one Proposal in whatever state it has.
func (s hubSource) Proposal(id string) (*athenav1.ProposalView, error) {
	var out *athenav1.ProposalView
	err := s.d.st.Tx(func(tx *store.Tx) error {
		p, err := tx.GetProposal(id)
		if err != nil {
			return err
		}
		if p == nil {
			return fmt.Errorf("proposal %s not found", id)
		}
		out = proposalView(tx, p)
		return nil
	})
	return out, err
}

// ProposalRow is a pending Proposal in the CLI's list.
type ProposalRow struct {
	ID      string `json:"id"`
	Repo    string `json:"repo,omitempty"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
}

func (d *Dispatcher) proposals() ([]ProposalRow, error) {
	list, err := d.eng.PendingProposals()
	if err != nil {
		return nil, err
	}
	out := make([]ProposalRow, 0, len(list))
	for _, p := range list {
		out = append(out, ProposalRow{ID: p.ID, Repo: p.Repo, Kind: p.Kind, Summary: p.Summary})
	}
	return out, nil
}
