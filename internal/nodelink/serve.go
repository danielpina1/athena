package nodelink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/redact"
)

// The server's calls to the node, by NodeCall.method (fact_seq 0). Each is answered with a NodeResult of its id; an
// error is DecodeError's input (EncodeError of the node's error).
const (
	ServeStart         = "start"         // ArgsStart; no value
	ServeDeliver       = "deliver"       // ArgsDeliver; no value
	ServeStop          = "stop"          // ArgsStop; no value
	ServeInterrupt     = "interrupt"     // ArgsInterrupt; no value
	ServeSetPaused     = "setPaused"     // ArgsSetPaused; no value
	ServeDropTokens    = "dropTokens"    // ArgsDropTokens; no value
	ServeApplyProposal = "applyProposal" // ArgsApplyProposal; no value
	ServeDiff          = "diff"          // ArgsDiff; node.Diff, its patch cut to fit a frame
	ServeFiles         = "files"         // ArgsFiles; ResultFiles
	ServeOpenFile      = "openFile"      // ArgsOpenFile; the file's bytes as the value itself (not JSON)
	ServeSubscribe     = "subscribe"     // ArgsSubscribe; no value, then MethodLines notifications on STREAM
	ServeUnsubscribe   = "unsubscribe"   // ArgsUnsubscribe; no value
	ServeStatus        = "status"        // ArgsStatus; node.Status
	ServeSeed          = "seed"          // not supported by this node yet
)

// Args of the server's calls. Start, Deliver and Stop carry the Ticket's snapshot, which the node keeps for Ticket
// while offline.
type (
	ArgsStart struct {
		Ticket     core.Ticket
		Assignment core.Assignment
	}
	ArgsDeliver struct {
		Ticket     core.Ticket
		Assignment core.Assignment
	}
	ArgsStop struct {
		Ticket     core.Ticket
		Assignment core.Assignment
		Reason     string
	}
	ArgsInterrupt struct{ TicketID string }
	ArgsSetPaused struct{ Paused bool }
	// ArgsDropTokens drops the node's write tokens of a repository the owner held (node.Node.DropTokens).
	ArgsDropTokens    struct{ Repo string }
	ArgsApplyProposal struct{ Proposal core.Proposal }
	ArgsDiff          struct{ TicketID string }
	ArgsFiles         struct{ TicketID string }
	ResultFiles       struct {
		Files     []node.FileEntry
		Truncated bool
	}
	ArgsOpenFile struct{ TicketID, Path string }
	// ArgsSubscribe starts a feed of the Ticket's transcript lines from From (node.Node.Subscribe). Its lines come as
	// MethodLines notifications whose Sub is this call's id: the backfill first, then the live lines.
	ArgsSubscribe struct {
		TicketID string
		From     int64
	}
	ArgsUnsubscribe struct{ Sub uint64 }
	ArgsStatus      struct{}
)

// Sentences of the node's refusals.
const (
	errTooLarge     = "too large to show from another node"
	errSeed         = "not supported by this node yet"
	errNotAttached  = "this node is not ready yet"
	maxOpenFile     = 768 << 10
	maxResultValue  = 900 << 10 // a NodeResult's value stays well under link.MaxFrame
	maxLinesBytes   = 384 << 10 // a lines notification's raw line bytes (base64 in JSON adds a third)
	applyTimeout    = 15 * time.Minute
	backfillTimeout = 30 * time.Second
)

// target is what serve calls on the node; *node.Node is one.
type target interface {
	Start(t core.Ticket, a core.Assignment)
	Deliver(t core.Ticket, a core.Assignment)
	Stop(t core.Ticket, a core.Assignment, reason string)
	Interrupt(ticketID string) error
	SetPaused(paused bool)
	DropTokens(repo string)
	ApplyProposal(ctx context.Context, p core.Proposal) error
	TicketDiff(ctx context.Context, ticketID string) (node.Diff, error)
	TicketFiles(ctx context.Context, ticketID string) ([]node.FileEntry, bool, error)
	OpenTicketFile(ctx context.Context, ticketID, rel string) (*os.File, error)
	Subscribe(ticketID string, from int64) ([]node.Line, <-chan node.Line, func(), error)
	Status() node.Status
}

var _ target = (*node.Node)(nil)

// decodeArgs decodes a call's args strictly.
func decodeArgs(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &core.UserError{Msg: "bad arguments: " + err.Error()}
	}
	return nil
}

// serve handles one server call. Start, Deliver, Stop, SetPaused and DropTokens only post to the node's mailbox, so they run here,
// on the reader goroutine, in the order they came; the rest run on their own goroutines.
func (c *Client) serve(cn *conn, call *athenav1.NodeCall) {
	id := call.GetId()
	t := c.target()
	if t == nil {
		c.reply(cn, id, nil, errors.New(errNotAttached))
		return
	}
	switch call.GetMethod() {
	case ServeStart:
		var a ArgsStart
		if err := decodeArgs(call.GetArgs(), &a); err != nil {
			c.reply(cn, id, nil, err)
			return
		}
		c.cacheTicket(a.Ticket)
		t.Start(a.Ticket, a.Assignment)
		c.reply(cn, id, nil, nil)
	case ServeDeliver:
		var a ArgsDeliver
		if err := decodeArgs(call.GetArgs(), &a); err != nil {
			c.reply(cn, id, nil, err)
			return
		}
		c.cacheTicket(a.Ticket)
		t.Deliver(a.Ticket, a.Assignment)
		c.reply(cn, id, nil, nil)
	case ServeStop:
		var a ArgsStop
		if err := decodeArgs(call.GetArgs(), &a); err != nil {
			c.reply(cn, id, nil, err)
			return
		}
		c.cacheTicket(a.Ticket)
		t.Stop(a.Ticket, a.Assignment, a.Reason)
		c.reply(cn, id, nil, nil)
	case ServeSetPaused:
		var a ArgsSetPaused
		if err := decodeArgs(call.GetArgs(), &a); err != nil {
			c.reply(cn, id, nil, err)
			return
		}
		t.SetPaused(a.Paused)
		c.reply(cn, id, nil, nil)
	case ServeDropTokens:
		var a ArgsDropTokens
		if err := decodeArgs(call.GetArgs(), &a); err != nil {
			c.reply(cn, id, nil, err)
			return
		}
		t.DropTokens(a.Repo)
		c.reply(cn, id, nil, nil)
	case ServeUnsubscribe:
		var a ArgsUnsubscribe
		if err := decodeArgs(call.GetArgs(), &a); err != nil {
			c.reply(cn, id, nil, err)
			return
		}
		cn.mu.Lock()
		stop := cn.subs[a.Sub]
		delete(cn.subs, a.Sub)
		cn.mu.Unlock()
		if stop != nil {
			stop()
		}
		c.reply(cn, id, nil, nil)
	case ServeSeed:
		c.reply(cn, id, nil, &core.UserError{Msg: errSeed})
	case ServeInterrupt, ServeApplyProposal, ServeDiff, ServeFiles, ServeOpenFile, ServeSubscribe, ServeStatus:
		go c.serveSlow(cn, t, call)
	default:
		c.reply(cn, id, nil, &core.UserError{Msg: fmt.Sprintf("unknown call %q", call.GetMethod())})
	}
}

// serveSlow handles a call that may take a while; it ends with the connection, except ApplyProposal, which a link
// drop must not cut halfway through a push.
func (c *Client) serveSlow(cn *conn, t target, call *athenav1.NodeCall) {
	id := call.GetId()
	var after func()
	value, err := func() ([]byte, error) {
		switch call.GetMethod() {
		case ServeInterrupt:
			var a ArgsInterrupt
			if err := decodeArgs(call.GetArgs(), &a); err != nil {
				return nil, err
			}
			return nil, t.Interrupt(a.TicketID)
		case ServeApplyProposal:
			var a ArgsApplyProposal
			if err := decodeArgs(call.GetArgs(), &a); err != nil {
				return nil, err
			}
			ctx := context.Background()
			if rc := c.runCtx.Load(); rc != nil {
				ctx = *rc
			}
			ctx, cancel := context.WithTimeout(ctx, applyTimeout)
			defer cancel()
			return nil, t.ApplyProposal(ctx, a.Proposal)
		case ServeDiff:
			var a ArgsDiff
			if err := decodeArgs(call.GetArgs(), &a); err != nil {
				return nil, err
			}
			d, err := t.TicketDiff(cn.ctx, a.TicketID)
			if err != nil {
				return nil, err
			}
			return encodeDiff(d)
		case ServeFiles:
			var a ArgsFiles
			if err := decodeArgs(call.GetArgs(), &a); err != nil {
				return nil, err
			}
			files, cut, err := t.TicketFiles(cn.ctx, a.TicketID)
			if err != nil {
				return nil, err
			}
			return json.Marshal(ResultFiles{Files: files, Truncated: cut})
		case ServeOpenFile:
			var a ArgsOpenFile
			if err := decodeArgs(call.GetArgs(), &a); err != nil {
				return nil, err
			}
			return openFile(cn.ctx, t, a)
		case ServeSubscribe:
			var a ArgsSubscribe
			if err := decodeArgs(call.GetArgs(), &a); err != nil {
				return nil, err
			}
			start, err := c.subscribe(cn, t, id, a)
			if err == nil {
				after = start // the lines follow the answer
			}
			return nil, err
		case ServeStatus:
			return json.Marshal(t.Status())
		}
		return nil, nil
	}()
	c.reply(cn, id, value, err)
	if after != nil {
		after()
	}
}

// openFile reads a Ticket's file whole, refusing one over 768 KiB.
func openFile(ctx context.Context, t target, a ArgsOpenFile) ([]byte, error) {
	f, err := t.OpenTicketFile(ctx, a.TicketID, a.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > maxOpenFile {
		return nil, &core.UserError{Msg: errTooLarge}
	}
	b, err := io.ReadAll(io.LimitReader(f, maxOpenFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxOpenFile {
		return nil, &core.UserError{Msg: errTooLarge}
	}
	return b, nil
}

// encodeDiff is d's JSON, its patch cut (and Truncated set) so it fits in a frame.
func encodeDiff(d node.Diff) ([]byte, error) {
	b, err := json.Marshal(d)
	if err != nil || len(b) <= maxResultValue {
		return b, err
	}
	// JSON escaping can make the patch bigger than its bytes: cut by what is over, then again until it fits.
	for len(b) > maxResultValue && d.Patch != "" {
		over := len(b) - maxResultValue
		keep := max(len(d.Patch)-over-1024, 0)
		for keep > 0 && !utf8.RuneStart(d.Patch[keep]) {
			keep--
		}
		d.Patch, d.Truncated = d.Patch[:keep], true
		if b, err = json.Marshal(d); err != nil {
			return nil, err
		}
	}
	if len(b) > maxResultValue {
		return nil, errors.New("the diff's file list is too long to send")
	}
	return b, nil
}

// subscribe starts a feed; start runs its sender, called once the subscribe call is answered so no line precedes the
// answer.
func (c *Client) subscribe(cn *conn, t target, sub uint64, a ArgsSubscribe) (start func(), err error) {
	backfill, live, stop, err := t.Subscribe(a.TicketID, a.From)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(cn.ctx)
	end := func() { cancel(); stop() }
	cn.mu.Lock()
	if cn.closed {
		cn.mu.Unlock()
		end()
		return nil, errLinkLost
	}
	cn.subs[sub] = end
	cn.mu.Unlock()
	return func() { go c.sendLines(cn, sctx, sub, backfill, live) }, nil
}

// sendLines sends a subscription's backfill, then its live lines in batches, until it ends. The backfill waits out
// backpressure; a live batch the link refuses is dropped and the next batch starts with a gap Line.
func (c *Client) sendLines(cn *conn, ctx context.Context, sub uint64, backfill []node.Line, live <-chan node.Line) {
	defer func() {
		cn.mu.Lock()
		if stop := cn.subs[sub]; stop != nil {
			delete(cn.subs, sub)
			go stop()
		}
		cn.mu.Unlock()
	}()
	send := func(lines []node.Line, closed bool, wait bool) bool {
		b, err := json.Marshal(ArgsLines{Sub: sub, Lines: lines, Closed: closed})
		if err != nil {
			return false
		}
		f := func() *athenav1.Frame {
			return &athenav1.Frame{Body: &athenav1.Frame_NodeCall{NodeCall: &athenav1.NodeCall{Method: MethodLines, Args: b}}}
		}
		deadline := time.Now().Add(backfillTimeout)
		for {
			err := cn.sess.Send(athenav1.Channel_STREAM, false, f())
			if err == nil {
				return true
			}
			if !wait || !errors.Is(err, link.ErrBackpressure) || time.Now().After(deadline) {
				return false
			}
			select {
			case <-ctx.Done():
				return false
			case <-time.After(retryPause):
			}
		}
	}
	for len(backfill) > 0 {
		n, size := 0, 0
		for n < len(backfill) && (n == 0 || size+len(backfill[n].Data) <= maxLinesBytes) {
			size += len(backfill[n].Data)
			n++
		}
		if !send(backfill[:n], false, true) {
			return
		}
		backfill = backfill[n:]
	}
	gapFrom := int64(-1)
	for {
		var batch []node.Line
		select {
		case <-ctx.Done():
			return
		case l, ok := <-live:
			if !ok {
				send(nil, true, true)
				return
			}
			batch = append(batch, l)
		}
		size := len(batch[0].Data)
	more:
		for size < maxLinesBytes {
			select {
			case l, ok := <-live:
				if !ok {
					break more
				}
				batch = append(batch, l)
				size += len(l.Data)
			default:
				break more
			}
		}
		if gapFrom >= 0 {
			batch = append([]node.Line{{Offset: gapFrom, GapTo: batch[0].Offset}}, batch...)
		}
		if send(batch, false, false) {
			gapFrom = -1
		} else if gapFrom < 0 {
			gapFrom = batch[0].Offset
		}
	}
}

// reply answers a server call. A reply the link pushes back on is retried from its own goroutine.
func (c *Client) reply(cn *conn, id uint64, value []byte, err error) {
	msg, user := EncodeError(err)
	if msg != "" {
		value = nil
	}
	f := func() *athenav1.Frame {
		return &athenav1.Frame{Body: &athenav1.Frame_NodeResult{NodeResult: &athenav1.NodeResult{Id: id, Value: value, Error: msg, UserError: user}}}
	}
	if err := cn.sess.Send(athenav1.Channel_CONTROL, true, f()); !errors.Is(err, link.ErrBackpressure) {
		return
	}
	go func() {
		for {
			select {
			case <-cn.ctx.Done():
				return
			case <-time.After(retryPause):
			}
			if err := cn.sess.Send(athenav1.Channel_CONTROL, true, f()); !errors.Is(err, link.ErrBackpressure) {
				return
			}
		}
	}()
}

// reportStatus sends a NodeStatusReport when the link comes up and whenever the node's status changes (StatusChanged),
// skipping one equal to the last sent.
func (c *Client) reportStatus(cn *conn) {
	var last *athenav1.NodeStatus
	for {
		if t := c.target(); t != nil {
			s := c.statusProto(t.Status())
			if last == nil || !proto.Equal(s, last) {
				err := cn.sess.Send(athenav1.Channel_CONTROL, false, &athenav1.Frame{Body: &athenav1.Frame_NodeStatusReport{
					NodeStatusReport: &athenav1.NodeStatusReport{Status: s}}})
				if err == nil {
					last = s
				} else {
					signal(c.statWake) // try again on the next change or below
				}
			}
		}
		select {
		case <-cn.ctx.Done():
			return
		case <-c.statWake:
			// Coalesce a burst of changes.
			select {
			case <-cn.ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

// statusProto is the node's status as the dashboard shows it; the fleet fills id, state and last seen.
func (c *Client) statusProto(s node.Status) *athenav1.NodeStatus {
	v := &athenav1.NodeStatus{Name: s.Name, Slots: uint32(max(s.Slots, 0)), SlotsInUse: uint32(max(s.SlotsInUse, 0)),
		LiveTicketIds: s.LiveTicketIDs, Queued: uint32(max(s.Queued, 0)), ClaudeVersion: s.ClaudeVersion,
		Id: c.id.NodeID, Os: runtime.GOOS, Arch: runtime.GOARCH, Version: c.opts.Version}
	for _, r := range s.Runtimes {
		q := r.Quota
		rv := &athenav1.RuntimeStatus{Name: r.Name, Version: r.Version, Ready: r.Ready, Tested: r.Tested,
			Why: redact.String(r.Why), Live: uint32(max(r.Live, 0)), LimitedUntil: tsp(q.LimitedUntil), Measured: q.Measured}
		if !q.ReadAt.IsZero() {
			rv.ReadAt = timestamppb.New(q.ReadAt)
		}
		for _, m := range r.Models {
			rv.Models = append(rv.Models, &athenav1.RuntimeModel{Id: m.ID, Name: m.Name, Default: m.Default})
		}
		for _, w := range q.Windows {
			name := w.Name
			if w.Bucket != "" {
				name = w.Bucket + " " + name
			}
			rv.Windows = append(rv.Windows, &athenav1.QuotaWindow{Name: name, Used: float32(w.Used), ResetsAt: tsp(w.ResetsAt), Stale: w.Stale})
		}
		v.Runtimes = append(v.Runtimes, rv)
	}
	return v
}

func tsp(t *time.Time) *timestamppb.Timestamp {
	if t == nil || t.IsZero() {
		return nil
	}
	return timestamppb.New(*t)
}
