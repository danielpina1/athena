package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrConnClosed is what a Call returns once the server's output has ended (or the Conn was never able to read).
var ErrConnClosed = errors.New("codex: connection closed")

// RPCError is a JSON-RPC error the server answered a request with.
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string { return fmt.Sprintf("codex rpc error %d: %s", e.Code, e.Message) }

// Message is a notification (ID empty) or a server request (ID set, to be answered with Respond) from the server.
type Message struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

// Conn is the JSON-RPC client over `codex app-server`'s stdio: one JSON object per line, with no "jsonrpc" field. It
// is safe for concurrent use; Run reads, everything else writes.
type Conn struct {
	r      *bufio.Reader
	onLine func(raw []byte)

	wmu sync.Mutex // serialises writes: a line is written whole
	w   io.Writer

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan reply
	done    bool // Run has ended: no response will come
}

type reply struct {
	result json.RawMessage
	err    error
}

// NewConn wraps the server's stdout (r) and stdin (w). onLine, when not nil, gets every notification and server
// request line the server sends (the node's transcript), as the server printed it, and runs on Run's goroutine so it
// must not block. It never gets a response to a request of ours: responses carry account data and our prompts back
// (thread/start echoes the system prompt), and the transcript must not hold them. It never gets what Conn writes
// either, for the same reason.
func NewConn(r io.Reader, w io.Writer, onLine func(raw []byte)) *Conn {
	return &Conn{r: bufio.NewReaderSize(r, 1<<16), w: w, onLine: onLine, pending: map[int64]chan reply{}}
}

// wire is any line of the protocol.
type wire struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Call sends a request and waits for its response, decoding the result into result (when not nil). A JSON-RPC error
// comes back as *RPCError. It returns ctx's error if ctx ends first (a late response is dropped), and ErrConnClosed
// when the server's output ended first.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return ErrConnClosed
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}

	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		forget()
		return err
	}
	select {
	case rep := <-ch:
		if rep.err != nil {
			return rep.err
		}
		if result != nil && len(rep.result) > 0 {
			if err := json.Unmarshal(rep.result, result); err != nil {
				return fmt.Errorf("codex %s: decode result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		forget()
		return ctx.Err()
	}
}

// Notify sends a notification.
func (c *Conn) Notify(method string, params any) error {
	m := map[string]any{"method": method}
	if params != nil {
		m["params"] = params
	}
	return c.write(m)
}

// Respond answers the server request with this id.
func (c *Conn) Respond(id json.RawMessage, result any) error {
	return c.write(map[string]any{"id": id, "result": result})
}

// RespondError answers the server request with this id with a JSON-RPC error.
func (c *Conn) RespondError(id json.RawMessage, code int, msg string) error {
	return c.write(map[string]any{"id": id, "error": map[string]any{"code": code, "message": msg}})
}

func (c *Conn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(b)
	return err
}

// Run reads the server's output until it ends: a response goes to its caller, a notification or a server request to
// handle, in order, on this goroutine. handle must not block on a Call (nothing would read the response) or wait for a
// person: a handler that does hands the message to its own goroutine. Lines are not capped in size (a command's output
// can be megabytes); a line that is not a JSON object is skipped. When Run returns, every Call still waiting gets
// ErrConnClosed.
func (c *Conn) Run(handle func(Message)) {
	defer c.finish()
	for {
		line, err := c.r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			c.dispatch(line, handle)
		}
		if err != nil {
			return
		}
	}
}

func (c *Conn) finish() {
	c.mu.Lock()
	c.done = true
	pending := c.pending
	c.pending = map[int64]chan reply{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- reply{err: ErrConnClosed}
	}
}

func (c *Conn) dispatch(line []byte, handle func(Message)) {
	var w wire
	if json.Unmarshal(line, &w) != nil {
		return
	}
	if w.Method == "" {
		c.answer(w)
		return
	}
	if c.onLine != nil {
		c.onLine(line)
	}
	if handle != nil {
		handle(Message{ID: w.ID, Method: w.Method, Params: w.Params})
	}
}

// answer routes a response to the Call that is waiting for it; one nobody waits for is dropped.
func (c *Conn) answer(w wire) {
	var id int64
	if json.Unmarshal(w.ID, &id) != nil {
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if !ok {
		return
	}
	if w.Error != nil {
		ch <- reply{err: &RPCError{Code: w.Error.Code, Message: w.Error.Message}}
		return
	}
	ch <- reply{result: w.Result}
}
