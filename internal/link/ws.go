package link

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coder/websocket"
)

// Subprotocol is the WebSocket subprotocol of the link.
const Subprotocol = "athena.v1"

// Conn is a message connection carrying one encoded Frame per message. Write must be safe to call concurrently with
// itself (a refusal is written from the read side while the write loop runs); Read is called from one goroutine.
type Conn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, b []byte) error
	Close(code int, reason string) error
}

type wsConn struct{ c *websocket.Conn }

// NewWS adapts a WebSocket to a Conn: binary messages only, at most MaxFrame bytes each, decompressed.
func NewWS(c *websocket.Conn) Conn {
	// The limit covers the decompressed message (read.go sets the limitReader over the flate reader), so a deflate
	// bomb stops here. One byte over MaxFrame: a message of exactly MaxFrame must not trip the reader's end check,
	// and Decode refuses anything above MaxFrame.
	c.SetReadLimit(MaxFrame + 1)
	return &wsConn{c: c}
}

func (w *wsConn) Read(ctx context.Context) ([]byte, error) {
	typ, b, err := w.c.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageBinary {
		return nil, fmt.Errorf("%w: text message", ErrProtocol)
	}
	return b, nil
}

func (w *wsConn) Write(ctx context.Context, b []byte) error {
	return w.c.Write(ctx, websocket.MessageBinary, b)
}

func (w *wsConn) Close(code int, reason string) error {
	return w.c.Close(websocket.StatusCode(code), reason)
}

// Accept upgrades r to a link WebSocket, negotiating athena.v1 with permessage-deflate and no context takeover.
// A client that did not offer athena.v1 is refused with 400 before the upgrade. Accept checks nothing else about
// the request (the exact-Origin rule belongs to the caller) beyond the library's same-host Origin check.
func Accept(w http.ResponseWriter, r *http.Request) (Conn, error) {
	if !offers(r, Subprotocol) {
		http.Error(w, "subprotocol "+Subprotocol+" required", http.StatusBadRequest)
		return nil, errors.New("link: client did not offer subprotocol " + Subprotocol)
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{Subprotocol},
		CompressionMode: websocket.CompressionNoContextTakeover,
	})
	if err != nil {
		return nil, err
	}
	return NewWS(c), nil
}

func offers(r *http.Request, proto string) bool {
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(h, ",") {
			if strings.TrimSpace(p) == proto {
				return true
			}
		}
	}
	return false
}
