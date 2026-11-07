package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/core"
	athenav1 "github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/web"
)

// A held pull request's detail shows what the owner publishes: its title, its base ("default branch" when it names
// none) and its body, cut short when long.
func TestProposalDetailShowsThePullRequestBody(t *testing.T) {
	pull := func(payload string) string {
		return proposalDetail(nil, &core.Proposal{Kind: core.ProposalPull, Payload: []byte(payload)})
	}
	if got, want := pull(`{"title":"Fix the parser","body":"It handled tabs badly.","base":"main"}`),
		"Fix the parser → main\n\nIt handled tabs badly."; got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}
	if got, want := pull(`{"title":"Fix the parser"}`), "Fix the parser → default branch"; got != want {
		t.Errorf("detail without a base or body = %q, want %q", got, want)
	}
	long := strings.Repeat("é", 3*maxDetailBody)
	got := pull(`{"title":"T","body":"` + long + `"}`)
	if !strings.HasPrefix(got, "T → default branch\n\n") || !strings.HasSuffix(got, "…") ||
		utf8.RuneCountInString(got) > utf8.RuneCountInString("T → default branch\n\n")+maxDetailBody+1 {
		t.Errorf("a long body is not cut short: %d runes", utf8.RuneCountInString(got))
	}
}

// A failed Proposal stays in the Snapshot until the owner applies or discards it, and the dashboard's "Try again" and
// "Discard" work on it.
func TestFailedProposalCanBeRetriedOrDiscarded(t *testing.T) {
	c, _, _ := newPolicyControl(t, openStore(t))
	putTickets(t, c.st, placedTicket("T-1", "acme/web", "", time.Now()))
	src := hubSource{d: c}
	failed := func(payload string) core.Proposal {
		p, err := c.eng.Propose(core.Proposal{TicketID: "T-1", Kind: core.ProposalPull, Summary: "pr", Payload: []byte(payload)})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.eng.SettleProposal(p.ID, errors.New("GitHub said no")); err != nil {
			t.Fatal(err)
		}
		return p
	}
	listed := func(id string) *athenav1.ProposalView {
		views, err := src.Proposals()
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range views {
			if v.GetId() == id {
				return v
			}
		}
		return nil
	}
	do := func(a *athenav1.Command) *athenav1.CommandResult {
		a.Id = time.Now().Format(time.RFC3339Nano)
		return c.Do(context.Background(), web.Origin{ClientID: "phone"}, a)
	}

	p := failed(`{"title":"one"}`)
	if v := listed(p.ID); v == nil || v.GetState() != core.ProposalFailed || v.GetError() != "GitHub said no" {
		t.Fatalf("snapshot view of the failed proposal = %v, want it listed as failed with its error", v)
	}
	if r := do(&athenav1.Command{Action: &athenav1.Command_ApplyProposal{ApplyProposal: &athenav1.ApplyProposal{Id: p.ID}}}); !r.GetOk() {
		t.Fatalf("try again = %v", r)
	}
	if v := listed(p.ID); v == nil || v.GetState() != core.ProposalPending || v.GetError() != "" {
		t.Errorf("after try again = %v, want pending with no error", v)
	}

	q := failed(`{"title":"two"}`)
	if r := do(&athenav1.Command{Action: &athenav1.Command_DiscardProposal{DiscardProposal: &athenav1.DiscardProposal{Id: q.ID}}}); !r.GetOk() {
		t.Fatalf("discard = %v", r)
	}
	if v := listed(q.ID); v != nil {
		t.Errorf("a discarded proposal is still listed: %v", v)
	}
}
