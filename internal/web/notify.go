package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/hub"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/store"
)

// Notifier turns a Ticket's entry into a person stage into one push notification. Anti-spam is the point: one
// notification per entry (the dedupe key is stored, so it survives a restart and a resync), none for a kind the owner
// muted, and none while a dashboard is on screen.

const (
	// NotificationsBlock is the settings block holding the owner's notification preferences.
	NotificationsBlock = store.NotificationsBlock
	// sentPrefix prefixes the kv keys that remember the entries already handled.
	sentPrefix = "push.sent."
	// visibleFor is how long a Visibility report keeps a client counted as looking at the dashboard.
	visibleFor = 30 * time.Second
	// maxTitleRunes bounds the Ticket title in a notification.
	maxTitleRunes = 80
)

// pushPhrases is the one phrase each person action adds to the Ticket's title (Global Constraints, "Push copy").
var pushPhrases = map[string]string{
	"answer":   "needs your answer",
	"approval": "needs approval",
	"review":   "is ready for review",
	"delivery": "delivery failed",
	"blocked":  "is blocked",
	"failed":   "failed",
}

// NotificationKinds lists the kinds an owner can mute.
func NotificationKinds() []string {
	return []string{"approval", "answer", "review", "delivery", "blocked", "failed"}
}

// notificationPrefs is the stored form of the "notifications" settings block.
type notificationPrefs struct {
	Muted []string `json:"muted"`
}

// MutedKinds returns the kinds the owner muted.
func MutedKinds(st *store.Store) ([]string, error) {
	rev, ok, err := st.CurrentSettings(NotificationsBlock)
	if err != nil || !ok {
		return nil, err
	}
	var p notificationPrefs
	if err := json.Unmarshal([]byte(rev.Doc), &p); err != nil {
		return nil, fmt.Errorf("web: notification settings: %w", err)
	}
	return p.Muted, nil
}

// SaveMutedKinds replaces the muted kinds; every kind must be one of NotificationKinds.
func SaveMutedKinds(st *store.Store, muted []string) error {
	var clean []string
	for _, k := range muted {
		if !slices.Contains(NotificationKinds(), k) {
			return &core.UserError{Msg: fmt.Sprintf("unknown notification kind %q", k)}
		}
		if !slices.Contains(clean, k) {
			clean = append(clean, k)
		}
	}
	slices.Sort(clean)
	doc, err := json.Marshal(notificationPrefs{Muted: clean})
	if err != nil {
		return err
	}
	for range 3 { // a concurrent save moves the revision
		rev, _, err := st.CurrentSettings(NotificationsBlock)
		if err != nil {
			return err
		}
		_, err = st.SaveSettings(NotificationsBlock, rev.Revision, string(doc))
		if !errors.Is(err, store.ErrStaleRevision) {
			return err
		}
	}
	return store.ErrStaleRevision
}

// Presence tracks which dashboards are on screen: a client counts as visible for 30 s after its last report of
// Visibility{visible: true}. The zero value is not ready; use NewPresence.
type Presence struct {
	now func() time.Time
	mu  sync.Mutex
	at  map[any]time.Time // by client: when it last reported visible
}

// NewPresence returns an empty Presence reading the time from now (nil: time.Now).
func NewPresence(now func() time.Time) *Presence {
	if now == nil {
		now = time.Now
	}
	return &Presence{now: now, at: map[any]time.Time{}}
}

// Report records a client's Visibility; a client that is not visible no longer counts.
func (p *Presence) Report(client any, visible bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if visible {
		p.at[client] = p.now()
	} else {
		delete(p.at, client)
	}
}

// Forget drops a client that disconnected.
func (p *Presence) Forget(client any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.at, client)
}

// Visible reports whether any client reported visible within the last 30 s.
func (p *Presence) Visible() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for c, t := range p.at {
		if now.Sub(t) < visibleFor {
			return true
		}
		delete(p.at, c)
	}
	return false
}

// Feed is the hub as the Notifier uses it.
type Feed interface {
	Subscribe(cursor uint64, epoch string) (*hub.Sub, bool)
	Snapshot() (*athenav1.Snapshot, error)
}

// NotifierConfig is what a Notifier works from.
type NotifierConfig struct {
	Feed  Feed
	Store *store.Store
	// Visible says whether a dashboard is on screen; nil means never.
	Visible func() bool
	// Wake tells the effects runner that a push was enqueued; nil is allowed.
	Wake func()
	Log  *slog.Logger
}

// Notifier watches the hub's Ticket deltas and enqueues the "push" effects.
type Notifier struct {
	feed    Feed
	st      *store.Store
	visible func() bool
	wake    func()
	log     *slog.Logger
	now     func() time.Time
	// known is the person action each Ticket was last seen in ("" when it is not in a person stage); only Run's
	// goroutine touches it.
	known map[string]string
	// observed, when set (tests), is called with each Ticket delta after it was handled.
	observed func(*athenav1.TicketSummary)
}

// NewNotifier returns a Notifier; Run starts it.
func NewNotifier(cfg NotifierConfig) *Notifier {
	n := &Notifier{feed: cfg.Feed, st: cfg.Store, visible: cfg.Visible, wake: cfg.Wake, log: cfg.Log, known: map[string]string{}, now: time.Now}
	if n.log == nil {
		n.log = slog.New(slog.DiscardHandler)
	}
	return n
}

// Run reconciles the Tickets already waiting for a person and then follows the hub until ctx is done or the hub shuts
// down. A feed the hub closed because it fell behind is resumed from a fresh Snapshot, which notifies only the
// entries not yet notified. ready, when not nil, is closed after the first reconciliation.
func (n *Notifier) Run(ctx context.Context, ready chan<- struct{}) {
	for ctx.Err() == nil {
		// Subscribe first: whatever the Snapshot misses arrives as a delta.
		sub, _ := n.feed.Subscribe(0, "")
		snap, err := n.feed.Snapshot()
		if err != nil {
			sub.Close()
			n.log.Error("push: snapshot", "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			continue
		}
		n.reconcile(snap.GetTickets())
		if ready != nil {
			close(ready)
			ready = nil
		}
		if !n.follow(ctx, sub) {
			return
		}
	}
}

// follow handles the feed's deltas; it reports whether to subscribe again.
func (n *Notifier) follow(ctx context.Context, sub *hub.Sub) bool {
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			return false
		case d, ok := <-sub.C():
			if !ok {
				return sub.Resync()
			}
			if t := d.GetTicket(); t != nil {
				n.observe(t, false)
				if n.observed != nil {
					n.observed(t)
				}
			}
		}
	}
}

// reconcile notifies the Tickets in a person stage that were not notified before (startup, or after a resync).
func (n *Notifier) reconcile(tickets []*athenav1.TicketSummary) {
	for _, t := range tickets {
		n.observe(t, true)
	}
}

// observe takes one Ticket's current state. A Ticket entering a person stage (or moving to another person action) is
// notified, keyed by the updated_at of the state that showed the entry; when reconciling, a Ticket whose entry
// into that action is already stored is left alone.
func (n *Notifier) observe(t *athenav1.TicketSummary, reconciling bool) {
	id := t.GetId()
	if t.GetStage() == "ended" || t.GetEndedAt() != nil {
		delete(n.known, id)
		n.prune(id)
		return
	}
	if t.GetStage() != "person" {
		n.known[id] = ""
		return
	}
	action := t.GetAction()
	if n.known[id] == action {
		return
	}
	n.known[id] = action
	if reconciling {
		if seen, err := n.seen(id, action); err != nil {
			n.log.Error("push: read sent keys", "error", err)
			return
		} else if seen {
			return
		}
	}
	n.notify(t, action)
}

// seen reports whether an entry of the Ticket into the action was already handled.
func (n *Notifier) seen(id, action string) (bool, error) {
	var found bool
	err := n.st.Tx(func(tx *store.Tx) error {
		m, err := tx.KVList(sentPrefix + id + ":person:" + action + ":")
		found = len(m) > 0
		return err
	})
	return found, err
}

// prune forgets the dedupe keys of a Ticket that ended.
func (n *Notifier) prune(id string) {
	err := n.st.Tx(func(tx *store.Tx) error {
		m, err := tx.KVList(sentPrefix + id + ":")
		for k := range m {
			if err == nil {
				err = tx.KVDelete(k)
			}
		}
		return err
	})
	if err != nil {
		n.log.Error("push: prune sent keys", "ticket", id, "error", err)
	}
}

func (n *Notifier) notify(t *athenav1.TicketSummary, action string) {
	phrase, ok := pushPhrases[action]
	if !ok {
		return // not a kind that is notified
	}
	key := fmt.Sprintf("%s:person:%s:%s", t.GetId(), action, t.GetUpdatedAt().AsTime().UTC().Format(time.RFC3339Nano))
	muted, err := MutedKinds(n.st)
	if err != nil {
		// Not knowing what is muted must not push: the entry stays unhandled until the next reconciliation.
		n.log.Error("push: read notification settings, entry skipped", "error", err)
		return
	}
	subs, err := n.st.PushSubscriptions()
	if err != nil {
		n.log.Error("push: read subscriptions, entry skipped", "error", err)
		return
	}
	skip := slices.Contains(muted, action) || n.visible != nil && n.visible()

	title := t.GetTitle()
	if title == "" {
		title = t.GetId()
	}
	if utf8.RuneCountInString(title) > maxTitleRunes {
		title = string([]rune(title)[:maxTitleRunes-1]) + "…"
	}
	urgency := "normal"
	if action == "approval" {
		urgency = "high"
	}
	msg, _ := json.Marshal(struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		URL   string `json:"url"`
		Tag   string `json:"tag"`
	}{"Athena", title + " " + phrase, "/t/" + t.GetId(), t.GetId()})
	// One effect per subscription, so a failing push service is retried alone.
	jobs := make(map[string][]byte, len(subs))
	for _, s := range subs {
		sum := sha256.Sum256([]byte(s.Endpoint))
		payload, _ := json.Marshal(pushJob{Endpoint: s.Endpoint, Urgency: urgency, Ticket: t.GetId(), Action: action,
			At: n.now().Unix(), Message: msg})
		jobs[key+"|"+hex.EncodeToString(sum[:6])] = payload
	}

	enqueued := false
	err = n.st.Tx(func(tx *store.Tx) error {
		if _, done, err := tx.KVGet(sentPrefix + key); err != nil || done {
			return err
		}
		state := "sent"
		if skip {
			state = "skipped" // remembered, so a restart does not notify an entry the owner chose not to be told of
		}
		if err := tx.KVSet(sentPrefix+key, state); err != nil {
			return err
		}
		if skip {
			return nil
		}
		for k, payload := range jobs {
			enqueued = true
			if err := tx.Enqueue(PushKind, k, payload); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		n.log.Error("push: enqueue", "ticket", t.GetId(), "error", err)
		return
	}
	if enqueued && n.wake != nil {
		n.wake()
	}
}
