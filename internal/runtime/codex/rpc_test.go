package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// pipeConn is a Conn wired to a fake server: the test reads what the Conn wrote from `in` and writes the server's
// lines to `out`.
type pipeConn struct {
	conn  *Conn
	in    *bufio.Reader
	out   io.WriteCloser
	lines chan []byte // onLine
	msgs  chan Message
	done  chan struct{}
}

func newPipeConn(t *testing.T) *pipeConn {
	t.Helper()
	cr, sw := io.Pipe() // server -> conn
	sr, cw := io.Pipe() // conn -> server
	p := &pipeConn{in: bufio.NewReader(sr), out: sw, lines: make(chan []byte, 16), msgs: make(chan Message, 16), done: make(chan struct{})}
	p.conn = NewConn(cr, cw, func(raw []byte) { p.lines <- append([]byte(nil), raw...) })
	go func() {
		p.conn.Run(func(m Message) { p.msgs <- m })
		close(p.done)
	}()
	t.Cleanup(func() { sw.Close(); cw.Close(); sr.Close() })
	return p
}

func (p *pipeConn) readRequest(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	line, err := p.in.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("%q: %v", line, err)
	}
	if _, has := m["jsonrpc"]; has {
		t.Fatalf("the protocol has no jsonrpc field: %s", line)
	}
	return m
}

func (p *pipeConn) send(t *testing.T, s string) {
	t.Helper()
	if _, err := io.WriteString(p.out, s+"\n"); err != nil {
		t.Fatal(err)
	}
}

func TestConnCallMatchesResponseByID(t *testing.T) {
	p := newPipeConn(t)
	type res struct{ N int }
	var a, b res
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	call := func(method string, out *res) {
		defer wg.Done()
		errs <- p.conn.Call(context.Background(), method, map[string]string{"k": method}, out)
	}
	wg.Add(2)
	go call("first", &a)
	req1 := p.readRequest(t)
	go call("second", &b)
	req2 := p.readRequest(t)
	if string(req1["id"]) != "1" || string(req2["id"]) != "2" {
		t.Fatalf("ids = %s, %s; want 1, 2", req1["id"], req2["id"])
	}
	m1, m2 := strings.Trim(string(req1["method"]), `"`), strings.Trim(string(req2["method"]), `"`)
	if m1 != "first" || m2 != "second" {
		t.Fatalf("methods = %s, %s", m1, m2)
	}
	// Answered out of order, and with a notification in between that must not confuse the match.
	p.send(t, `{"id":2,"result":{"N":22}}`)
	p.send(t, `{"method":"thread/started","params":{}}`)
	p.send(t, `{"id":1,"result":{"N":11}}`)
	wg.Wait()
	if e := <-errs; e != nil {
		t.Fatal(e)
	}
	if e := <-errs; e != nil {
		t.Fatal(e)
	}
	if a.N != 11 || b.N != 22 {
		t.Fatalf("results = %+v, %+v; want 11 and 22", a, b)
	}
}

func TestConnRoutesServerRequestsAndNotifications(t *testing.T) {
	p := newPipeConn(t)
	p.send(t, `{"method":"turn/started","params":{"threadId":"thr_fixture_1"}}`)
	p.send(t, `{"id":"srv-7","method":"item/commandExecution/requestApproval","params":{"itemId":"item_1"}}`)
	p.send(t, `{"id":99,"result":{"nobody":"waits"}}`) // a response nobody waits for is dropped

	n := <-p.msgs
	if n.Method != "turn/started" || len(n.ID) != 0 || !strings.Contains(string(n.Params), "thr_fixture_1") {
		t.Fatalf("notification = %+v", n)
	}
	r := <-p.msgs
	if r.Method != "item/commandExecution/requestApproval" || string(r.ID) != `"srv-7"` {
		t.Fatalf("server request = %+v", r)
	}
	go p.conn.Respond(r.ID, map[string]string{"decision": "accept"})
	got := p.readRequest(t)
	if string(got["id"]) != `"srv-7"` || !strings.Contains(string(got["result"]), `"accept"`) {
		t.Fatalf("response = %v", got)
	}
	go p.conn.RespondError(r.ID, -32601, "no")
	got = p.readRequest(t)
	if string(got["id"]) != `"srv-7"` || !strings.Contains(string(got["error"]), `"code":-32601`) {
		t.Fatalf("error response = %v", got)
	}
	go p.conn.Notify("initialized", nil)
	got = p.readRequest(t)
	if string(got["method"]) != `"initialized"` || got["id"] != nil {
		t.Fatalf("notification = %v", got)
	}
}

// R9: the transcript hook sees notifications and server requests, never a response (it may hold account data or the
// prompt), and never what the Conn wrote.
func TestConnOnLineSeesIncomingNotificationsAndRequestsOnly(t *testing.T) {
	p := newPipeConn(t)
	go func() { _ = p.conn.Call(context.Background(), "account/read", nil, nil) }()
	p.readRequest(t)
	p.send(t, `{"id":1,"result":{"account":{"type":"chatgpt","email":"x"}}}`)
	p.send(t, `{"method":"account/updated","params":{}}`)
	p.send(t, `{"id":5,"method":"item/tool/call","params":{}}`)
	var seen []string
	for len(seen) < 2 {
		seen = append(seen, string(<-p.lines))
	}
	if !strings.Contains(seen[0], "account/updated") || !strings.Contains(seen[1], "item/tool/call") {
		t.Fatalf("lines = %q", seen)
	}
	select {
	case l := <-p.lines:
		t.Fatalf("an extra line reached onLine: %s", l)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestConnRPCErrorIsTyped(t *testing.T) {
	p := newPipeConn(t)
	errc := make(chan error, 1)
	go func() { errc <- p.conn.Call(context.Background(), "turn/steer", nil, nil) }()
	p.readRequest(t)
	p.send(t, `{"id":1,"error":{"code":-32600,"message":"no active turn"}}`)
	err := <-errc
	var re *RPCError
	if !errors.As(err, &re) || re.Code != -32600 || re.Message != "no active turn" {
		t.Fatalf("err = %v, want *RPCError{-32600, no active turn}", err)
	}
}

func TestConnCallEndsWhenTheServerGoesAwayOrTheContextEnds(t *testing.T) {
	p := newPipeConn(t)
	errc := make(chan error, 1)
	go func() { errc <- p.conn.Call(context.Background(), "x", nil, nil) }()
	p.readRequest(t)
	p.out.Close()
	if err := <-errc; !errors.Is(err, ErrConnClosed) {
		t.Fatalf("err = %v, want ErrConnClosed", err)
	}
	<-p.done
	if err := p.conn.Call(context.Background(), "y", nil, nil); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("Call after the end = %v, want ErrConnClosed", err)
	}

	q := newPipeConn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	go func() { _, _ = q.in.ReadBytes('\n') }()
	if err := q.conn.Call(ctx, "z", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
}

// R8: a line has no size cap; a command's output of 1 MiB must not end the session.
func TestConnReadsAMiBLine(t *testing.T) {
	p := newPipeConn(t)
	big := strings.Repeat("x", 1<<20)
	go func() { _, _ = io.WriteString(p.out, `{"method":"item/completed","params":{"output":"`+big+`"}}`+"\n") }()
	m := <-p.msgs
	var params struct{ Output string }
	if err := json.Unmarshal(m.Params, &params); err != nil || len(params.Output) != 1<<20 {
		t.Fatalf("params: len %d, err %v", len(params.Output), err)
	}
	if l := <-p.lines; len(l) < 1<<20 {
		t.Fatalf("onLine got %d bytes, want the whole line", len(l))
	}
	// And the connection still works after it.
	p.send(t, `{"method":"after","params":{}}`)
	if m := <-p.msgs; m.Method != "after" {
		t.Fatalf("next message = %+v", m)
	}
}

func TestConnSkipsLinesThatAreNotJSON(t *testing.T) {
	p := newPipeConn(t)
	p.send(t, `not json`)
	p.send(t, ``)
	p.send(t, `{"method":"ok","params":{}}`)
	if m := <-p.msgs; m.Method != "ok" {
		t.Fatalf("msg = %+v", m)
	}
}

// A nil params goes out as {}: the real protocol requires an object for most methods and rejects null.
func TestConnNilParamsAreAnEmptyObject(t *testing.T) {
	p := newPipeConn(t)
	go func() { _ = p.conn.Call(context.Background(), "account/read", nil, nil) }()
	if req := p.readRequest(t); string(req["params"]) != "{}" {
		t.Fatalf("params = %s, want {}", req["params"])
	}
}

// R46: a server request is handled off the read loop, so its handler may wait for a Call (an interrupt, while an
// approval is pending) and the loop keeps reading, which that Call's response needs.
func TestConnServerRequestHandlerMayCall(t *testing.T) {
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	defer sw.Close()
	conn := NewConn(cr, cw, nil)
	in := bufio.NewReader(sr)
	handled := make(chan error, 1)
	go conn.Run(func(m Message) {
		if len(m.ID) == 0 {
			return
		}
		err := conn.Call(context.Background(), "turn/interrupt", map[string]string{"threadId": "t"}, nil)
		if err == nil {
			err = conn.Respond(m.ID, map[string]string{"decision": "decline"})
		}
		handled <- err
	})
	io.WriteString(sw, `{"id":42,"method":"item/commandExecution/requestApproval","params":{}}`+"\n")
	line, err := in.ReadBytes('\n') // the handler's Call
	if err != nil || !strings.Contains(string(line), `"turn/interrupt"`) {
		t.Fatalf("line = %q, %v", line, err)
	}
	io.WriteString(sw, `{"id":1,"result":{}}`+"\n")
	line, err = in.ReadBytes('\n') // then its answer to the request
	if err != nil || !strings.Contains(string(line), `"id":42`) || !strings.Contains(string(line), "decline") {
		t.Fatalf("line = %q, %v", line, err)
	}
	if err := <-handled; err != nil {
		t.Fatal(err)
	}
}

// The transcript must not keep the account a token refresh request names.
func TestConnOnLineRedactsTheAuthRefreshRequest(t *testing.T) {
	p := newPipeConn(t)
	p.send(t, `{"id":3,"method":"account/chatgptAuthTokens/refresh","params":{"reason":"unauthorized","previousAccountId":"acct_real_123"}}`)
	line := string(<-p.lines)
	if strings.Contains(line, "acct_real_123") || !strings.Contains(line, "account/chatgptAuthTokens/refresh") || !strings.Contains(line, "unauthorized") {
		t.Fatalf("transcript line = %s", line)
	}
	if m := <-p.msgs; !strings.Contains(string(m.Params), "acct_real_123") {
		t.Fatalf("the handler must still get the real params: %s", m.Params)
	}
}

// errReader gives its data, then fails.
type errReader struct {
	data string
	err  error
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.data == "" {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestConnRunReturnsReadErrorsDistinctFromEOF(t *testing.T) {
	boom := errors.New("boom")
	conn := NewConn(&errReader{data: "{\"method\":\"a\",\"params\":{}}\n", err: boom}, io.Discard, nil)
	var got []string
	if err := conn.Run(func(m Message) { got = append(got, m.Method) }); !errors.Is(err, boom) || len(got) != 1 {
		t.Fatalf("Run = %v after %v, want the read error", err, got)
	}
	if err := conn.Call(context.Background(), "x", nil, nil); !errors.Is(err, ErrConnClosed) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Call after a read error = %v", err)
	}
	if err := NewConn(strings.NewReader(""), io.Discard, nil).Run(nil); err != nil {
		t.Fatalf("Run at EOF = %v, want nil", err)
	}
}

func TestConnAReplyWithoutAUsableIDDoesNotStrandCallers(t *testing.T) {
	p := newPipeConn(t)
	var logged []string
	p.conn.Logf = func(f string, a ...any) { logged = append(logged, f) }
	errc := make(chan error, 1)
	go func() { errc <- p.conn.Call(context.Background(), "x", nil, nil) }()
	p.readRequest(t)
	// A parse error the server could not tie to a request ends the waiting call with that error.
	p.send(t, `{"id":null,"error":{"code":-32700,"message":"parse error"}}`)
	var re *RPCError
	if err := <-errc; !errors.As(err, &re) || re.Code != -32700 {
		t.Fatalf("err = %v, want the parse error", err)
	}
	// A reply with a string id cannot be matched: it is logged, and the connection goes on.
	p.send(t, `{"id":"abc","result":{}}`)
	p.send(t, `{"method":"after","params":{}}`)
	if m := <-p.msgs; m.Method != "after" {
		t.Fatalf("msg = %+v", m)
	}
	if len(logged) != 1 {
		t.Fatalf("logged = %v", logged)
	}
}

// A server that stopped reading its stdin must not hold a Call past its deadline.
func TestConnCallWriteHonoursTheContext(t *testing.T) {
	cr, sw := io.Pipe()
	_, cw := io.Pipe() // nobody reads it: a write blocks
	defer sw.Close()
	conn := NewConn(cr, cw, nil)
	go conn.Run(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := conn.Call(ctx, "x", nil, nil); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}
