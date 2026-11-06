package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	athenav1 "github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/store"
	"github.com/danielpina1/athena/internal/web"
)

const npmClass = "bash|bash-escalate:npm|acme/web"

func approveTimes(t *testing.T, c *Dispatcher, n int) {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	for i := range n {
		at := base.Add(time.Duration(i) * time.Minute)
		a := &core.Approval{ID: fmt.Sprintf("a%02d", i), TicketID: "t", Tool: "Bash", Rule: "bash-escalate:npm", Repo: "acme/web",
			State: core.ApprovalApproved, CreatedAt: at, AnsweredAt: &at}
		if err := c.st.Tx(func(tx *store.Tx) error { return tx.PutApproval(a) }); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAllowActionWritesRule(t *testing.T) {
	c, _, _ := newPolicyControl(t, openStore(t))
	approveTimes(t, c, 5)
	do := func(id string, sensitive bool, a *athenav1.Command) *athenav1.CommandResult {
		a.Id = id
		return c.Do(context.Background(), web.Origin{ClientID: "phone", Sensitive: sensitive}, a)
	}
	allow := func() *athenav1.Command {
		return &athenav1.Command{Action: &athenav1.Command_AllowAction{AllowAction: &athenav1.AllowAction{Class: npmClass}}}
	}
	if r := do("c1", false, allow()); r.GetCode() != web.CodeReauthRequired {
		t.Fatalf("allow without reauth = %v", r)
	}
	if cur, rev, _ := c.policy.saved(); rev != 0 || len(cur.Allow) != 0 {
		t.Fatalf("a refused allow changed the policy: rev %d, %v", rev, cur.Allow)
	}
	if r := do("c1", true, allow()); !r.GetOk() {
		t.Fatalf("allow with reauth = %v", r)
	}
	cur, rev, _ := c.policy.saved()
	if rev != 1 || len(cur.Allow) != 1 || cur.Allow[0].Tool != "Bash" || cur.Allow[0].Rule != "bash-escalate:npm" || cur.Allow[0].Repo != "acme/web" || cur.Allow[0].Added.IsZero() {
		t.Fatalf("policy after allow: rev %d, allow %+v", rev, cur.Allow)
	}
	if offers, err := (hubSource{d: c}).Offers(); err != nil || len(offers) != 0 {
		t.Errorf("offers after allowing = %v, %v; want none", offers, err)
	}
	if r := do("c2", true, allow()); r.GetOk() {
		t.Errorf("allowing a class twice = %v, want refused", r)
	}
}

func TestDismissOfferHidesUntilFiveMore(t *testing.T) {
	c, _, _ := newPolicyControl(t, openStore(t))
	approveTimes(t, c, 5)
	src := hubSource{d: c}
	if o, _ := src.Offers(); len(o) != 1 {
		t.Fatalf("offers = %v, want one", o)
	}
	r := c.Do(context.Background(), web.Origin{ClientID: "phone"}, &athenav1.Command{Id: "d1",
		Action: &athenav1.Command_DismissOffer{DismissOffer: &athenav1.DismissOffer{Class: npmClass}}})
	if !r.GetOk() {
		t.Fatalf("dismiss = %v", r)
	}
	if o, _ := src.Offers(); len(o) != 0 {
		t.Errorf("offers after dismiss = %v, want none", o)
	}
	approveTimes(t, c, 10) // re-puts a00-a04 and adds five more: ten approvals
	if o, _ := src.Offers(); len(o) != 1 || o[0].Approved != 10 {
		t.Errorf("offers at ten = %v, want one", o)
	}
}
