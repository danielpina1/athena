// Package outbox sends the Engine's replies: it runs the `reply` effect, which loads the receipt the Engine queued and
// sends it through its Source's Channel, after the reference (src/app/outbox.ts, 003 "Channel failure").
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/effects"
	"github.com/danielpina1/athena/internal/redact"
	"github.com/danielpina1/athena/internal/sources"
	"github.com/danielpina1/athena/internal/store"
)

const (
	// failuresBeforeProblem is how many failed sends of an earlier message hold a later one back.
	failuresBeforeProblem = 3
	// wait is how long a message waits behind an earlier one, or for its Source to run.
	wait = 5 * time.Second
	// lastErrorMax bounds the failure a receipt records.
	lastErrorMax = 300
)

// Options are the effects runner's options for the `reply` kind: one at a time, retried after 5 s doubling to 300 s.
// The kind is not Ordered: the Outbox keeps each Ticket's messages to a conversation in order itself, so a failing
// message holds back only those, not the messages of other Tickets or conversations.
var Options = effects.Options{Concurrency: 1, Backoff: effects.Backoff{First: 5 * time.Second, Max: 300 * time.Second}}

// Handler returns the `reply` effect handler. For a waiting receipt it:
//
//   - waits (effects.Later, 5 s) while the receipt's Source is not registered or not running;
//   - waits while an earlier receipt of the same Ticket to the same conversation is waiting and has failed fewer than
//     three times;
//   - drops it (channelGone) when the Source no longer listens to the conversation, or noChannel when the Source has
//     no Channel;
//   - otherwise sends it: sent settles it, gone drops it (channelGone) for good, and a failure counts an attempt on
//     the receipt and is returned for the runner to back off.
//
// A receipt that is missing, sent or dropped needs nothing.
func Handler(reg *sources.Registry, st *store.Store) effects.Handler {
	return func(ctx context.Context, key string, payload []byte) error {
		var p struct {
			ReceiptID string `json:"receiptId"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("outbox: reply %s: payload: %w", key, err)
		}
		id := p.ReceiptID
		if id == "" {
			id = key
		}
		var r *core.Receipt
		var behind bool
		err := st.Tx(func(tx *store.Tx) error {
			var err error
			if r, err = tx.GetReceipt(id); err != nil || r == nil || r.State != core.ReceiptWaiting {
				return err
			}
			behind, err = tx.ReceiptBehind(r, failuresBeforeProblem)
			return err
		})
		if err != nil {
			return err
		}
		if r == nil || r.State != core.ReceiptWaiting {
			return nil
		}
		src := reg.Source(r.Channel.Source)
		if src == nil || !reg.Running(r.Channel.Source) {
			return effects.Later(wait)
		}
		if behind {
			return effects.Later(wait)
		}
		if !src.Listens(r.Conversation) {
			return settle(st, id, core.ReceiptDropped, core.DropChannelGone)
		}
		ch, ok := src.(sources.Channel)
		if !ok {
			return settle(st, id, core.ReceiptDropped, core.DropNoChannel)
		}
		res, err := ch.Send(ctx, r.Channel, r.ID, r.Attempts, r.Text)
		if err != nil {
			if ctx.Err() == nil {
				if ferr := failed(st, id, err); ferr != nil {
					return fmt.Errorf("outbox: reply %s: %w (recording it: %v)", id, err, ferr)
				}
			}
			return fmt.Errorf("outbox: reply %s: %w", id, err)
		}
		switch res {
		case sources.Sent:
			return settle(st, id, core.ReceiptSent, "")
		case sources.Gone:
			return settle(st, id, core.ReceiptDropped, core.DropChannelGone)
		}
		return fmt.Errorf("outbox: reply %s: source %s answered %q", id, r.Channel.Source, res)
	}
}

// settle records that a waiting receipt was sent or dropped, with the reason.
func settle(st *store.Store, id, state, reason string) error {
	return st.Tx(func(tx *store.Tx) error {
		r, err := tx.GetReceipt(id)
		if err != nil || r == nil || r.State != core.ReceiptWaiting {
			return err
		}
		at := st.Now()
		r.State = state
		if state == core.ReceiptSent {
			r.SentAt = &at
		} else {
			r.DroppedAt = &at
			r.DroppedReason = reason
		}
		return tx.PutReceipt(r)
	})
}

// failed counts a failed send on a waiting receipt.
func failed(st *store.Store, id string, sendErr error) error {
	return st.Tx(func(tx *store.Tx) error {
		r, err := tx.GetReceipt(id)
		if err != nil || r == nil || r.State != core.ReceiptWaiting {
			return err
		}
		r.Attempts++
		r.LastError = truncate(redact.String(sendErr.Error()), lastErrorMax)
		return tx.PutReceipt(r)
	})
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}
