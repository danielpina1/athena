package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/store"
)

// maxDetailBody bounds, in characters, the body of a held pull request its detail shows.
const maxDetailBody = 2000

// proposalDetail is what the owner decides on: the held reply's text, "branch @ sha7" for a push, and for a pull request
// its title, its base ("default branch" when it names none) and its body, cut to maxDetailBody characters: what a
// worker wrote is published as it is. A payload that cannot be read gives no detail.
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
		var pl struct{ Title, Base, Body string }
		if json.Unmarshal(p.Payload, &pl) != nil {
			return ""
		}
		base := pl.Base
		if base == "" {
			base = "default branch"
		}
		out := pl.Title + " → " + base
		if body := strings.TrimSpace(pl.Body); body != "" {
			if utf8.RuneCountInString(body) > maxDetailBody {
				body = string([]rune(body)[:maxDetailBody]) + "…"
			}
			out += "\n\n" + body
		}
		return out
	}
	return ""
}

func proposalView(tx *store.Tx, p *core.Proposal) *athenav1.ProposalView {
	return &athenav1.ProposalView{Id: p.ID, TicketId: p.TicketID, Repo: p.Repo, Kind: p.Kind, State: p.State,
		Summary: p.Summary, Detail: proposalDetail(tx, p), CreatedAt: timestamppb.New(p.CreatedAt), Error: p.Error}
}

// Proposals returns the Proposals that wait on the owner, oldest first: the pending ones and the failed ones, which stay
// until the owner tries them again or discards them.
func (s hubSource) Proposals() ([]*athenav1.ProposalView, error) {
	var out []*athenav1.ProposalView
	err := s.d.st.Tx(func(tx *store.Tx) error {
		list, err := tx.UndecidedProposals()
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

// ProposalRow is a Proposal in the CLI's list: one that waits on the owner, pending or failed.
type ProposalRow struct {
	ID      string `json:"id"`
	Repo    string `json:"repo,omitempty"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
}

func (d *Dispatcher) proposals() ([]ProposalRow, error) {
	out := []ProposalRow{}
	err := d.st.Tx(func(tx *store.Tx) error {
		list, err := tx.UndecidedProposals()
		for _, p := range list {
			out = append(out, ProposalRow{ID: p.ID, Repo: p.Repo, Kind: p.Kind, Summary: p.Summary, State: p.State, Error: p.Error})
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
