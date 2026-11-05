package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// ErrConnClosed is what a Call returns once the server's output has ended; when it ended on a read error, the error
// follows it ("codex: connection closed: <read error>").
var ErrConnClosed = errors.New("codex: connection closed")

// RPCError is a JSON-RPC error the server answered a request with. Data is the error's data, when it had any (Codex
// puts a turn error's codexErrorInfo there).
type RPCError struct {
	Code    int
	Message string
	Data    json.RawMessage
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

	// Logf, when set before Run, is told of what the Conn could not use (a reply it cannot match to a call). It must
	// not log the line itself: it may hold account data.
	Logf func(format string, args ...any)

	wmu sync.Mutex // serialises writes: a line is written whole
	w   io.Writer

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan reply
	done    bool  // Run has ended: no response will come
	endErr  error // why: ErrConnClosed, with the read error when there was one
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
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data,omitempty"`
	} `json:"error,omitempty"`
}

// Call sends a request and waits for its response, decoding the result into result (when not nil). A JSON-RPC error
// comes back as *RPCError. It returns ctx's error if ctx ends first, while waiting or while writing (a late response
// is dropped), and ErrConnClosed when the server's output ended first. A nil params is sent as {}.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.done {
		err := c.endErr
		c.mu.Unlock()
		return err
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

	if params == nil {
		params = struct{}{} // the protocol wants an object where it wants params at all, and rejects null
	}
	if err := c.writeCtx(ctx, map[string]any{"id": id, "method": method, "params": params}); err != nil {
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
	return c.writeLine(append(b, '\n'))
}

func (c *Conn) writeLine(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.w.Write(b)
	return err
}

// writeCtx is write, but gives up when ctx ends: a server that stopped reading its stdin must not hold a Call past
// its deadline. The abandoned write stays pending in its own goroutine until the pipe moves or closes.
func (c *Conn) writeCtx(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- c.writeLine(append(b, '\n')) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run reads the server's output until it ends and returns why: nil at EOF, the read error otherwise. A response goes
// to its caller; a notification goes to handle, in order, on this goroutine; a server request goes to handle on a
// goroutine of its own (R46), so a handler may wait for a person or for a Call without stopping the reading its own
// response needs. Lines are not capped in size (a command's output can be megabytes); a line that is not a JSON
// object is skipped. When Run returns, every Call still waiting gets ErrConnClosed.
func (c *Conn) Run(handle func(Message)) error {
	var rerr error
	defer func() { c.finish(rerr) }()
	for {
		line, err := c.r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			c.dispatch(line, handle)
		}
		if err != nil {
			if err != io.EOF {
				rerr = err
			}
			return rerr
		}
	}
}

func (c *Conn) finish(cause error) {
	end := ErrConnClosed
	if cause != nil {
		end = fmt.Errorf("%w: %v", ErrConnClosed, cause)
	}
	c.mu.Lock()
	c.done, c.endErr = true, end
	c.mu.Unlock()
	c.failAll(end)
}

// failAll ends every Call that waits with err.
func (c *Conn) failAll(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[int64]chan reply{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- reply{err: err}
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
		c.onLine(transcriptLine(line, w))
	}
	if handle == nil {
		return
	}
	m := Message{ID: w.ID, Method: w.Method, Params: w.Params}
	if len(w.ID) > 0 {
		go handle(m)
		return
	}
	handle(m)
}

// transcriptLine is the line the transcript keeps: the server's own, except a token refresh request, whose params name
// the account.
func transcriptLine(line []byte, w wire) []byte {
	if w.Method != "account/chatgptAuthTokens/refresh" {
		return line
	}
	params := map[string]json.RawMessage{}
	_ = json.Unmarshal(w.Params, &params)
	for k := range params {
		if l := strings.ToLower(k); strings.Contains(l, "account") || strings.Contains(l, "token") || strings.Contains(l, "email") {
			params[k] = json.RawMessage(`"[redacted]"`)
		}
	}
	b, err := json.Marshal(map[string]any{"id": w.ID, "method": w.Method, "params": params})
	if err != nil {
		return []byte(`{"method":"` + w.Method + `"}`)
	}
	return b
}

// answer routes a response to the Call that is waiting for it; one nobody waits for is dropped. An error reply with no
// usable id (a parse error the server could not tie to a request) ends every waiting Call with it, since one of them
// was not understood and would otherwise wait for its context; any other unusable id is only logged.
func (c *Conn) answer(w wire) {
	var id int64
	noID := len(w.ID) == 0 || string(w.ID) == "null"
	if noID && w.Error != nil {
		c.failAll(&RPCError{Code: w.Error.Code, Message: w.Error.Message})
		return
	}
	if noID || json.Unmarshal(w.ID, &id) != nil {
		if c.Logf != nil {
			c.Logf("codex: a reply with an id that is not a number was dropped")
		}
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
		ch <- reply{err: &RPCError{Code: w.Error.Code, Message: w.Error.Message, Data: w.Error.Data}}
		return
	}
	ch <- reply{result: w.Result}
}
