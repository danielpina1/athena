package store

import (
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

func TestReceiptsRoundTripAndBehind(t *testing.T) {
	s, _ := openTemp(t)
	at := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	ch := core.ChannelRef{Source: "github", Thread: "github:o/r#1", Address: map[string]string{"comment": "1"}}
	ack := &core.Receipt{ID: "ack", TicketID: "T-0001", Purpose: core.PurposeAcknowledgement, Text: "On it.", Channel: ch,
		Conversation: ch.Thread, State: core.ReceiptWaiting, Seq: 1, QueuedAt: at}
	result := &core.Receipt{ID: "result", TicketID: "T-0001", Purpose: core.PurposeResult, Text: "Done.", Channel: ch,
		Conversation: ch.Thread, State: core.ReceiptWaiting, Seq: 2, QueuedAt: at}
	elsewhere := &core.Receipt{ID: "other", TicketID: "T-0001", Purpose: core.PurposeAnswer, Text: "Yes.",
		Conversation: "github:o/r#2", State: core.ReceiptWaiting, Seq: 3, QueuedAt: at}
	lone := &core.Receipt{ID: "lone", Purpose: core.PurposeResult, Text: "x", Conversation: ch.Thread,
		State: core.ReceiptWaiting, Seq: 4, QueuedAt: at}
	mustOK(t, s.Tx(func(tx *Tx) error {
		for _, r := range []*core.Receipt{result, ack, elsewhere, lone} {
			if err := tx.PutReceipt(r); err != nil {
				return err
			}
		}
		return nil
	}))
	mustOK(t, s.Tx(func(tx *Tx) error {
		got, err := tx.GetReceipt("ack")
		if err != nil {
			return err
		}
		if got == nil || got.Text != "On it." || got.Channel.Address["comment"] != "1" || !got.QueuedAt.Equal(at) {
			t.Errorf("round trip: %+v", got)
		}
		if none, err := tx.GetReceipt("nope"); err != nil || none != nil {
			t.Errorf("missing: %v %v", none, err)
		}
		list, err := tx.TicketReceipts("T-0001")
		if err != nil {
			return err
		}
		var ids []string
		for _, r := range list {
			ids = append(ids, r.ID)
		}
		if len(ids) != 3 || ids[0] != "ack" || ids[1] != "result" || ids[2] != "other" {
			t.Errorf("ticket receipts in seq order: %v", ids)
		}
		check := func(what string, r *core.Receipt, want bool) {
			got, err := tx.ReceiptBehind(r, 3)
			if err != nil || got != want {
				t.Errorf("%s: behind = %v, %v; want %v", what, got, err, want)
			}
		}
		check("result behind the waiting ack", result, true)
		check("the ack is first", ack, false)
		check("another conversation", elsewhere, false)
		check("no ticket", lone, false)
		ack.Attempts = 3
		if err := tx.PutReceipt(ack); err != nil {
			return err
		}
		check("the ack failed three times", result, false)
		ack.Attempts, ack.State = 0, core.ReceiptSent
		if err := tx.PutReceipt(ack); err != nil {
			return err
		}
		check("the ack went out", result, false)
		return nil
	}))
}

func mustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
