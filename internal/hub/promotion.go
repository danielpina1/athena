package hub

import (
	"slices"
	"strings"

	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/store"
)

// Offer is an action class the owner keeps approving, offered as a standing allow rule.
type Offer struct {
	Class, Tool, Rule, Repo string
	Approved                int
}

// Offers returns the classes to offer: approved at least PromoteAfter times in a row, not a rule no allow rule may
// name, not already allowed, and approved PromoteAfter more times than when the owner last dismissed the offer
// (dismissed maps a class to the count it was dismissed at). A class with no repository is never offered: an allow
// rule names one.
func Offers(streaks []store.Streak, s policy.Settings, dismissed map[string]int) []Offer {
	after := s.PromoteAfter
	if after <= 0 {
		after = policy.DefaultPromoteAfter
	}
	var out []Offer
	for _, g := range streaks {
		class := store.ApprovalClass(g.Tool, g.Rule, g.Repo)
		switch {
		case g.Approved < after, g.Repo == "", slices.Contains(policy.NeverPromote, g.Rule):
			continue
		case g.Approved < dismissed[class]+after:
			continue
		case allowed(s, g):
			continue
		}
		out = append(out, Offer{Class: class, Tool: g.Tool, Rule: g.Rule, Repo: g.Repo, Approved: g.Approved})
	}
	return out
}

func allowed(s policy.Settings, g store.Streak) bool {
	for _, a := range s.Allow {
		if a.Tool == g.Tool && a.Rule == g.Rule && strings.EqualFold(a.Repo, g.Repo) {
			return true
		}
	}
	return false
}

func offerList(offers []Offer) *athenav1.OfferList {
	out := &athenav1.OfferList{}
	for _, o := range offers {
		out.Offers = append(out.Offers, &athenav1.PromotionOffer{Class: o.Class, Tool: o.Tool, Rule: o.Rule, Repo: o.Repo,
			Approved: uint32(o.Approved)})
	}
	return out
}
