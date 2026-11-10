package nodelink

import (
	"slices"
	"strings"

	"github.com/danielpina1/athena/internal/link"
)

// closeRevoked is the close code of a link the server ends because the node was revoked (fleet.CloseRevoked).
const closeRevoked = 4003

// heldCall is a start or deliver of the server's that waits for Serving.
type heldCall struct {
	ticketID, assignmentID string
	run                    func(t target)
}

// whenServing runs fn on the node now, or, with HoldUntilServing before Serving, once Serving is called, after the
// calls held before it. Seeds are not held: they only stage a checkpoint for the start that follows them, so they
// stay ahead of it.
func (c *Client) whenServing(ticketID, assignmentID string, fn func(t target)) {
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	if !c.serving {
		c.held = append(c.held, heldCall{ticketID, assignmentID, fn})
		return
	}
	if t := c.target(); t != nil {
		fn(t)
	}
}

// dropHeld cancels the held calls of an Assignment the server stops: it must not start after its stop.
func (c *Client) dropHeld(ticketID, assignmentID string) {
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	c.held = slices.DeleteFunc(c.held, func(h heldCall) bool {
		return h.ticketID == ticketID && (assignmentID == "" || h.assignmentID == assignmentID)
	})
}

// Serving says the node's socket serves: the held starts and delivers go to the node, in the order they came, and
// later ones go at once. Only the target's mailbox is touched, so this never blocks.
func (c *Client) Serving() {
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	if c.serving {
		return
	}
	c.serving = true
	t := c.target()
	for _, h := range c.held {
		if t != nil {
			h.run(t)
		}
	}
	c.held = nil
}

// revoked reports whether a link ended because the server revoked this node: it closed the link with closeRevoked.
func revoked(err error) bool {
	return err != nil && link.CloseCode(err) == closeRevoked
}

// certRefused reports whether the server refused this node's certificate at the TLS handshake (a revoked or replaced
// node, or one it does not know): the server's bad_certificate alert. The alert reaches the client as a net.OpError
// "remote error" whose type crypto/tls does not export, through layers that do not all wrap it, so it is matched by
// its text.
func certRefused(err error) bool {
	return err != nil && strings.Contains(err.Error(), "remote error: tls: bad certificate")
}
