package hub

import (
	"testing"

	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/store"
)

func TestOffersAtThreshold(t *testing.T) {
	streak := func(rule string, n int) []store.Streak {
		return []store.Streak{{Tool: "Bash", Rule: rule, Repo: "acme/web", Approved: n}}
	}
	class := "bash|bash-escalate:npm|acme/web"
	var none policy.Settings

	if got := Offers(streak("bash-escalate:npm", 5), none, nil); len(got) != 1 || got[0].Class != class || got[0].Approved != 5 {
		t.Errorf("5 approvals = %+v, want one offer of %s", got, class)
	}
	if got := Offers(streak("bash-escalate:npm", 4), none, nil); len(got) != 0 {
		t.Errorf("4 approvals = %+v, want none", got)
	}
	if got := Offers(streak("credential-read", 10), none, nil); len(got) != 0 {
		t.Errorf("credential-read at 10 = %+v, want none", got)
	}
	withRule := policy.Settings{Allow: []policy.AllowRule{{Tool: "Bash", Rule: "bash-escalate:npm", Repo: "acme/web"}}}
	if got := Offers(streak("bash-escalate:npm", 9), withRule, nil); len(got) != 0 {
		t.Errorf("already allowed = %+v, want none", got)
	}
	dismissed := map[string]int{class: 5}
	if got := Offers(streak("bash-escalate:npm", 9), none, dismissed); len(got) != 0 {
		t.Errorf("dismissed at 5, now 9 = %+v, want none", got)
	}
	if got := Offers(streak("bash-escalate:npm", 10), none, dismissed); len(got) != 1 {
		t.Errorf("dismissed at 5, now 10 = %+v, want one", got)
	}
	if got := Offers(streak("bash-escalate:npm", 3), policy.Settings{PromoteAfter: 3}, nil); len(got) != 1 {
		t.Errorf("promoteAfter 3 with 3 = %+v, want one", got)
	}
}
