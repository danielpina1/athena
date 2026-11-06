package server

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/danielpina1/athena/internal/hub"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/store"
)

// dismissedPrefix keys the count an offer was dismissed at: offer-dismissed:<class>.
const dismissedPrefix = "offer-dismissed:"

const offerGone = "that offer is no longer open"

// dismissedOffers returns, per class, the approval count the owner dismissed its offer at.
func (d *Dispatcher) dismissedOffers() (map[string]int, error) {
	kv, err := d.st.KVList(dismissedPrefix)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(kv))
	for k, v := range kv {
		if n, err := strconv.Atoi(v); err == nil {
			out[strings.TrimPrefix(k, dismissedPrefix)] = n
		}
	}
	return out, nil
}

// offer finds the open offer of a class under the current Policy revision, and that revision.
func (d *Dispatcher) offer(class string) (hub.Offer, policy.Settings, int, error) {
	cur, rev, err := d.policy.saved()
	if err != nil {
		return hub.Offer{}, cur, 0, err
	}
	streaks, err := d.st.ApprovalStreaks()
	if err != nil {
		return hub.Offer{}, cur, 0, err
	}
	dismissed, err := d.dismissedOffers()
	if err != nil {
		return hub.Offer{}, cur, 0, err
	}
	for _, o := range hub.Offers(streaks, cur, dismissed) {
		if o.Class == class {
			return o, cur, rev, nil
		}
	}
	return hub.Offer{}, cur, rev, userErrorf(offerGone)
}

// allowAction turns an open offer into a standing allow rule, saved at the revision the offer was read at: a Policy
// change in between refuses it as stale.
func (d *Dispatcher) allowAction(class string) error {
	o, cur, rev, err := d.offer(class)
	if err != nil {
		return err
	}
	cur.Allow = append(cur.Allow, policy.AllowRule{Tool: o.Tool, Rule: o.Rule, Repo: o.Repo, Added: d.st.Now()})
	raw, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	_, err = d.setPolicy(raw, rev, policy.OriginWeb)
	return err
}

// dismissOffer hides an open offer until the class has been approved PromoteAfter more times.
func (d *Dispatcher) dismissOffer(class string) error {
	o, _, _, err := d.offer(class)
	if err != nil {
		return err
	}
	if err := d.st.KVSet(dismissedPrefix+class, strconv.Itoa(o.Approved)); err != nil {
		return err
	}
	if d.web != nil && d.web.hub != nil {
		d.web.hub.OnCommit(store.Changes{Offers: true})
	}
	return nil
}
