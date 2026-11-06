// Package outbox sends the Engine's replies: it runs the `reply` effect, which loads the receipt the Engine queued and
// sends it through its Source's Channel, after the reference (src/app/outbox.ts, 003 "Channel failure").
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// PausedWait is how long a reply waits while Athena is paused (a variable so tests may shorten it).
var PausedWait = 30 * time.Second

// Options are the effects runner's options for the `reply` kind: one at a time, retried after 5 s doubling to 300 s.
// The kind is not Ordered: the Outbox keeps each Ticket's messages to a conversation in order itself, so a failing
// message holds back only those, not the messages of other Tickets or conversations.
var Options = effects.Options{Concurrency: 1, Backoff: effects.Backoff{First: 5 * time.Second, Max: 300 * time.Second}}

// Handler returns the `reply` effect handler. For a waiting receipt it:
//
//   - waits (effects.Later, PausedWait) while paused() says the owner paused Athena;
//   - waits (effects.Later, 5 s) while the receipt's Source is not registered or not running;
//   - waits while an earlier receipt of the same Ticket to the same conversation is waiting and has failed fewer than
//     three times;
//   - drops it (channelGone) when the Source no longer listens to the conversation, or noChannel when the Source has
//     no Channel;
//   - otherwise records the send as started (Started), then sends it: sent settles it, gone drops it (channelGone)
//     for good, a refusal (sources.ErrRefused) drops it (refused) with the failure in LastError, and any other failure
//     counts an attempt (Attempts) on the receipt and is returned for the runner to back off.
//
// A receipt that is missing, sent or dropped needs nothing.
func Handler(reg *sources.Registry, st *store.Store, paused func() bool) effects.Handler {
	return func(ctx context.Context, key string, payload []byte) error {
		if paused != nil && paused() {
			return effects.Later(PausedWait) // held, not dropped: the owner paused Athena
		}
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
		// Record the send as started before making it: if the process dies after the Channel sent the message but before
		// it is settled, the re-run is known to be a retry and the Channel checks before posting again.
		started, err := begun(st, id)
		if err != nil {
			return fmt.Errorf("outbox: reply %s: %w", id, err)
		}
		if started < 0 {
			return nil // settled meanwhile
		}
		res, err := ch.Send(ctx, r.Channel, sources.SendRequest{ReceiptID: r.ID, Attempt: started, QueuedAt: r.QueuedAt, Text: r.Text})
		if err != nil {
			if ctx.Err() == nil && errors.Is(err, sources.ErrRefused) {
				// Retrying cannot get past it: drop it with the reason, where health shows it (Refused).
				slog.Warn("outbox: reply refused for good; dropped", "receipt", id, "ticket", r.TicketID,
					"conversation", r.Conversation, "error", redact.String(err.Error()))
				return refused(st, id, err)
			}
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

// begun counts a send as started on a waiting receipt and returns how many were started before it, or -1 when the
// receipt is no longer waiting. Started is separate from Attempts, which counts only failed sends and is what holds a
// later message back, so a failed send is counted once in each and a lost settle counts in Started alone.
func begun(st *store.Store, id string) (before int, _ error) {
	before = -1
	err := st.Tx(func(tx *store.Tx) error {
		r, err := tx.GetReceipt(id)
		if err != nil || r == nil || r.State != core.ReceiptWaiting {
			return err
		}
		before = r.Started
		r.Started++
		return tx.PutReceipt(r)
	})
	return before, err
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

// refused drops a waiting receipt the Channel refused for good, counting the failed send and keeping why.
func refused(st *store.Store, id string, sendErr error) error {
	return st.Tx(func(tx *store.Tx) error {
		r, err := tx.GetReceipt(id)
		if err != nil || r == nil || r.State != core.ReceiptWaiting {
			return err
		}
		at := st.Now()
		r.Attempts++
		r.LastError = truncate(redact.String(sendErr.Error()), lastErrorMax)
		r.State, r.DroppedAt, r.DroppedReason = core.ReceiptDropped, &at, core.DropRefused
		return tx.PutReceipt(r)
	})
}

// Refused returns the receipts dropped as refused since since, oldest first: messages that never went out and need a
// person (a missing App permission, say).
func Refused(st *store.Store, since time.Time) ([]*core.Receipt, error) {
	var out []*core.Receipt
	err := st.Tx(func(tx *store.Tx) error {
		dropped, err := tx.ReceiptsInState(core.ReceiptDropped)
		for _, r := range dropped {
			if r.DroppedReason == core.DropRefused && r.DroppedAt != nil && !r.DroppedAt.Before(since) {
				out = append(out, r)
			}
		}
		return err
	})
	return out, err
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
