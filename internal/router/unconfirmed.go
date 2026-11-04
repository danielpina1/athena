package router

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/danielpina1/athena/internal/runtime/claude"
)

// The unconfirmed messages: what was written to the router session but may not be in its conversation yet. Claude Code
// holds a message sent mid-turn until a step takes it in, so a crash or a restart in between would lose it. Each message
// is kept in kv router.unconfirmed, with a uuid of its own, before it is written (so before its effect row and the
// notices it carries are settled); the session's echo of it (--replay-user-messages, sent when a turn takes the message
// in) carries that uuid and marks it taken, whatever the CLI did to its text; the Result that ends that turn drops what
// was taken. A message sent after a turn's last step is not echoed before that turn's Result, so it stays. A stop that
// lets the session exit on its own (the CLI finishes the turn it took the messages into) drops the taken ones too. On
// every start the rest goes again, as one message headed by recoveryNote: after a crash, a taken message whose turn
// never ended goes again with the untaken ones.

const (
	unconfirmedKey = "router.unconfirmed"
	// maxUnconfirmed bounds the kept messages; past it the oldest go, with a warning.
	maxUnconfirmed = 50
)

// sentMsg is one unconfirmed message: Text as written, Body without a recovery note (what a resend sends again), UUID
// the uuid it was written with (empty for a message kept before uuids, matched by its text).
type sentMsg struct {
	Body  string `json:"body"`
	Text  string `json:"text"`
	UUID  string `json:"uuid,omitempty"`
	Taken bool   `json:"taken,omitempty"`
}

// newUUID returns a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// loadUnconfirmed reads the kept messages; the Host must not be running.
func (h *Host) loadUnconfirmed() error {
	raw, ok, err := h.cfg.KV.KVGet(unconfirmedKey)
	if err != nil {
		return fmt.Errorf("router: read %s: %w", unconfirmedKey, err)
	}
	var list []sentMsg
	if ok && raw != "" {
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			h.log.Error("drop unreadable unconfirmed messages", "error", err)
			list = nil
		}
	}
	h.unMu.Lock()
	h.unconfirmed = list
	h.unMu.Unlock()
	return nil
}

// persist writes the kept messages; unMu is held.
func (h *Host) persist() error {
	if len(h.unconfirmed) > maxUnconfirmed {
		h.log.Warn("too many unconfirmed router messages; dropping the oldest", "dropped", len(h.unconfirmed)-maxUnconfirmed)
		h.unconfirmed = h.unconfirmed[len(h.unconfirmed)-maxUnconfirmed:]
	}
	raw, err := json.Marshal(h.unconfirmed)
	if err != nil {
		return err
	}
	if err := h.cfg.KV.KVSet(unconfirmedKey, string(raw)); err != nil {
		return fmt.Errorf("router: keep unconfirmed messages: %w", err)
	}
	return nil
}

// sent keeps a message about to be written and returns the uuid to write it with.
func (h *Host) sent(text string) (string, error) {
	h.unMu.Lock()
	defer h.unMu.Unlock()
	id := newUUID()
	h.unconfirmed = append(h.unconfirmed, sentMsg{Body: text, Text: text, UUID: id})
	return id, h.persist()
}

// unsent forgets the message of uuid id, whose write failed (its effect sends it again).
func (h *Host) unsent(id string) {
	h.unMu.Lock()
	defer h.unMu.Unlock()
	for i := len(h.unconfirmed) - 1; i >= 0; i-- {
		if m := h.unconfirmed[i]; !m.Taken && m.UUID == id {
			h.unconfirmed = append(h.unconfirmed[:i], h.unconfirmed[i+1:]...)
			break
		}
	}
	if err := h.persist(); err != nil {
		h.log.Error("forget an unsent message", "error", err)
	}
}

// observe follows a session event: an echo marks its message taken, a Result drops the taken ones. An echo is matched
// by its uuid; an echo whose uuid matches no message (a CLI that does not carry it back, or one that gives it a uuid of
// its own) is matched by text, to the first untaken message of that text, so its message is not sent again on every
// start.
func (h *Host) observe(ev claude.Event) {
	switch e := ev.(type) {
	case claude.Init:
		h.keepSession(e.SessionID)
	case claude.UserEcho:
		h.unMu.Lock()
		defer h.unMu.Unlock()
		i := slices.IndexFunc(h.unconfirmed, func(m sentMsg) bool { return !m.Taken && e.UUID != "" && m.UUID == e.UUID })
		if i < 0 {
			i = slices.IndexFunc(h.unconfirmed, func(m sentMsg) bool { return !m.Taken && m.Text == e.Text })
			if i >= 0 && e.UUID != "" {
				h.log.Warn("router echo carries an unknown uuid; matched by text", "uuid", e.UUID)
			}
		}
		if i < 0 {
			return
		}
		h.unconfirmed[i].Taken = true
		if err := h.persist(); err != nil {
			h.log.Error("mark a message taken", "error", err)
		}
	case claude.Result:
		h.keepSession(e.SessionID)
		h.dropTaken()
	}
}

// dropTaken forgets the messages a turn took in: they are in the session's conversation.
func (h *Host) dropTaken() {
	h.unMu.Lock()
	defer h.unMu.Unlock()
	kept := h.unconfirmed[:0]
	for _, m := range h.unconfirmed {
		if !m.Taken {
			kept = append(kept, m)
		}
	}
	if len(kept) == len(h.unconfirmed) {
		return
	}
	h.unconfirmed = kept
	if err := h.persist(); err != nil {
		h.log.Error("drop confirmed messages", "error", err)
	}
}

// pendingTexts returns the unconfirmed messages as written.
func (h *Host) pendingTexts() []string {
	h.unMu.Lock()
	defer h.unMu.Unlock()
	var out []string
	for _, m := range h.unconfirmed {
		out = append(out, m.Text)
	}
	return out
}

// resend writes a starting session what may not have reached it, as one message headed by recoveryNote, which then
// stands for them all. sendMu is held and the session is not yet published, so no other message goes first.
func (h *Host) resend(sess *claude.Session) {
	h.unMu.Lock()
	var bodies []string
	for _, m := range h.unconfirmed {
		bodies = append(bodies, m.Body)
	}
	if len(bodies) == 0 {
		h.unMu.Unlock()
		return
	}
	body := strings.Join(bodies, "\n")
	text := recoveryNote + "\n" + body
	id := newUUID()
	h.unconfirmed = []sentMsg{{Body: body, Text: text, UUID: id}}
	err := h.persist()
	h.unMu.Unlock()
	if err != nil {
		h.log.Error("keep the resend", "error", err)
	}
	h.log.Info("resending unconfirmed messages to the router session", "messages", len(bodies))
	if err := sess.SendMessage(text, id); err != nil {
		h.log.Warn("resend to the router session", "error", err)
	}
}
