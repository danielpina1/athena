package web

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/render"
)

// Live session streams. A client opens one per Ticket (StreamOpen) and pays for what it receives with credit: the
// connection starts with InitialStreamCredit, and the total of STREAM items the client has granted (link
// Session.Credit) is added; that is what the connection may have sent, in all its
// streams together. The connection never waits for credit and never queues for a slow client: a stream out of credit
// keeps only a position (the offset of the first line it skipped); when credit comes it asks the node for its latest
// lines and sends a StreamGap over what it skipped and the latest tail.
//
// Items of one transcript line share its offset. A client that lost its connection resumes with
// from_offset = (offset of the last item it got) + 1: the next line, not that one again.

// Streams is where a connection gets a Ticket's session output; *node.Node is one.
type Streams interface {
	Subscribe(ticketID string, from int64) (backfill []node.Line, live <-chan node.Line, cancel func(), err error)
}

// InitialStreamCredit is the STREAM credit every connection starts with; the client's grants add to it.
const InitialStreamCredit = 256

const (
	// maxStreams is how many Tickets one connection streams at once.
	maxStreams = 16
	// tailItems is how many of the latest items follow a gap, and how many a stream opened from the tail starts with.
	tailItems = 200
	// pendingItems and pendingBytes bound what a stream holds for a client without credit.
	pendingItems = 2000
	pendingBytes = 1 << 20
	// itemsPerFrame and frameBytes bound one StreamItems frame, well under link.MaxFrame.
	itemsPerFrame = 50
	frameBytes    = 256 << 10
	// backpressureRetry is the pause before a stream tries again after the link's STREAM queue was full.
	backpressureRetry = 100 * time.Millisecond
)

// streamer is one open stream of a connection.
type streamer struct {
	ticketID string
	from     int64
	cancel   context.CancelFunc
}

// openStream starts a stream (it replaces one of the same Ticket). It runs on the Session's reader and only hands
// work on.
func (c *conn) openStream(o *athenav1.StreamOpen) {
	id := o.GetTicketId()
	switch {
	case id == "":
		c.sendError(CodeInvalid, "stream_open needs a ticket id")
		return
	case c.d.streams == nil:
		c.streamFailed(id, CodeUnavailable, "session streams are not available")
		return
	}
	c.smu.Lock()
	old := c.streams[id]
	if old == nil && len(c.streams) >= maxStreams {
		c.smu.Unlock()
		c.streamFailed(id, CodeInvalid, fmt.Sprintf("at most %d streams are open at once", maxStreams))
		return
	}
	if c.streamCtx.Err() != nil { // the connection is ending
		c.smu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(c.streamCtx)
	st := &streamer{ticketID: id, from: o.GetFromOffset(), cancel: cancel}
	c.streams[id] = st
	c.streamWG.Add(1)
	c.smu.Unlock()
	if old != nil {
		old.cancel()
	}
	go func() {
		defer c.streamWG.Done()
		defer cancel()
		c.runStream(ctx, st)
		c.smu.Lock()
		if c.streams[id] == st {
			delete(c.streams, id)
		}
		c.smu.Unlock()
	}()
}

// closeStream ends the client's stream of a Ticket.
func (c *conn) closeStream(id string) {
	c.smu.Lock()
	st := c.streams[id]
	delete(c.streams, id)
	c.smu.Unlock()
	if st != nil {
		st.cancel()
	}
}

// streamFailed tells the client why a stream is not served: an Error, and the StreamClose that ends it.
func (c *conn) streamFailed(id, code, msg string) {
	c.sendError(code, msg)
	_ = c.sess.Send(athenav1.Channel_STREAM, false, &athenav1.Frame{Body: &athenav1.Frame_StreamClose{
		StreamClose: &athenav1.StreamClose{TicketId: id}}})
}

// onCredit wakes the streams: the client granted more.
func (c *conn) onCredit(ch athenav1.Channel) {
	if ch != athenav1.Channel_STREAM {
		return
	}
	c.smu.Lock()
	close(c.creditWake)
	c.creditWake = make(chan struct{})
	c.smu.Unlock()
}

// wakeChan is the channel that closes at the next grant. Read it before looking at the credit, and no grant is
// missed.
func (c *conn) wakeChan() <-chan struct{} {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.creditWake
}

// availableLocked is the credit left: the implicit start plus the client's grants, less what was sent.
func (c *conn) availableLocked() int {
	total := c.d.streamCredit + uint64(c.sess.Credit(athenav1.Channel_STREAM))
	if total <= c.itemsSent {
		return 0
	}
	return int(min(total-c.itemsSent, 1<<30))
}

func (c *conn) available() int {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.availableLocked()
}

// take reserves up to want items of credit, atomically across the connection's streams, and returns how many it
// got; refund gives them back.
func (c *conn) take(want int) int {
	c.smu.Lock()
	defer c.smu.Unlock()
	n := min(want, c.availableLocked())
	c.itemsSent += uint64(n)
	return n
}

func (c *conn) refund(n int) { c.smu.Lock(); c.itemsSent -= uint64(n); c.smu.Unlock() }

// pending is a stream's unsent items, oldest first, all newer than skipFrom when that is set. A stream out of credit
// holds none: it only keeps skipFrom, and catches up from the node when credit comes.
type pending struct {
	items []*athenav1.StreamItem
	bytes int
	// skipFrom is the offset of the first item lost without being sent, or -1.
	skipFrom int64
	starved  bool
	lastAt   time.Time
}

// size is what an item counts for in every bound: its encoded size.
func size(it *athenav1.StreamItem) int { return proto.Size(it) + 8 }

func (p *pending) markSkip(off int64) {
	if p.skipFrom < 0 || off < p.skipFrom {
		p.skipFrom = off
	}
}

func (p *pending) dropFront(n int) {
	if n <= 0 {
		return
	}
	for _, it := range p.items[:n] {
		p.bytes -= size(it)
	}
	clear(p.items[:n])
	p.items = p.items[n:]
}

// starve drops what is pending: with no credit only a position is kept.
func (p *pending) starve() {
	if len(p.items) > 0 {
		p.markSkip(p.items[0].GetOffset())
		p.dropFront(len(p.items))
	}
	p.items, p.bytes, p.starved = nil, 0, true
}

// absorb takes a line from the node. A starved stream renders nothing: it notes where the loss began.
func (p *pending) absorb(l node.Line) {
	if p.starved {
		p.markSkip(l.Offset)
		return
	}
	if l.GapTo != 0 {
		// The node dropped lines for this stream: everything held is older than the loss, and goes with it.
		if len(p.items) > 0 {
			p.markSkip(p.items[0].GetOffset())
			p.dropFront(len(p.items))
		}
		p.markSkip(l.Offset)
		return
	}
	p.add(l)
	over := 0
	for bytes := p.bytes; over < len(p.items)-1 && (len(p.items)-over > pendingItems || bytes > pendingBytes); over++ {
		bytes -= size(p.items[over])
	}
	if over > 0 {
		p.markSkip(p.items[0].GetOffset())
		p.dropFront(over)
	}
}

// add renders a line onto the end, dated when it has no time of its own.
func (p *pending) add(l node.Line) {
	for _, it := range render.Items(l.Data, l.Offset) {
		if it.At == nil {
			if p.lastAt.IsZero() {
				p.lastAt = time.Now()
			}
			it.At = timestamppb.New(p.lastAt)
		} else {
			p.lastAt = it.At.AsTime()
		}
		p.items = append(p.items, it)
		p.bytes += size(it)
	}
}

// streamError is what a stream that could not open says: "<name> is offline" for a node that is not connected.
func streamError(err error) string {
	if msg, ok := nodeOffline(err); ok {
		return msg
	}
	return "cannot stream this ticket"
}

// runStream serves one stream until its context ends.
func (c *conn) runStream(ctx context.Context, st *streamer) {
	if _, err := c.d.hub.Detail(st.ticketID); err != nil {
		c.streamFailed(st.ticketID, CodeNotFound, "no such ticket")
		return
	}
	backfill, live, cancel, err := c.d.streams.Subscribe(st.ticketID, st.from)
	if err != nil {
		c.d.log.Warn("dashboard: stream subscribe", "ticket", st.ticketID, "error", err)
		c.streamFailed(st.ticketID, CodeUnavailable, streamError(err))
		return
	}
	defer func() { cancel() }()

	p := &pending{skipFrom: -1}
	for _, l := range backfill {
		p.absorb(l)
	}
	if st.from < 0 {
		// From the tail: the last tailItems items, however many lines that took (many render to nothing). What is
		// left out is history the client did not ask for, not a loss: no gap.
		p.dropFront(len(p.items) - tailItems)
	}
	var retry <-chan time.Time
	for {
		wake := c.wakeChan()
		if p.starved && c.available() > 0 {
			// Credit came to a stream that fell behind: skip ahead to the node's latest lines.
			tail, newLive, newCancel, err := c.d.streams.Subscribe(st.ticketID, -1)
			if err != nil {
				c.d.log.Warn("dashboard: stream catch-up", "ticket", st.ticketID, "error", err)
				c.streamFailed(st.ticketID, CodeUnavailable, streamError(err))
				return
			}
			cancel()
			live, cancel = newLive, newCancel
			p.catchUp(tail, c.available())
		}
		backpressured, err := c.flush(st.ticketID, p)
		if err != nil {
			return
		}
		retry = nil
		if backpressured {
			retry = time.After(backpressureRetry)
		}
		select {
		case <-ctx.Done():
			return
		case l, ok := <-live:
			if !ok {
				return
			}
			p.absorb(l)
			// Take what else is waiting before sending: one pass for a burst.
			for more := true; more; {
				select {
				case l, ok := <-live:
					if !ok {
						return
					}
					p.absorb(l)
				default:
					more = false
				}
			}
		case <-wake:
		case <-retry:
		}
	}
}

// catchUp replaces the pending items with the latest of the node's lines from skipFrom on, at most tailItems and what
// avail covers; what is left out becomes part of the gap.
func (p *pending) catchUp(tail []node.Line, avail int) {
	p.items, p.bytes, p.starved = nil, 0, false
	for _, l := range tail {
		switch {
		case l.GapTo != 0:
			p.markSkip(l.Offset)
		case l.Offset >= p.skipFrom:
			p.add(l)
		}
	}
	if keep := min(avail, tailItems); len(p.items) > keep {
		p.markSkip(p.items[0].GetOffset())
		p.dropFront(len(p.items) - keep)
	}
}

// flush sends what credit and the link allow of p, in order: a StreamGap first when items were skipped. Items that
// find the credit gone are dropped (the stream is starved). It reports whether the link's STREAM queue was full,
// when the rest stays pending; the error is the link having closed.
func (c *conn) flush(ticketID string, p *pending) (backpressured bool, err error) {
	for len(p.items) > 0 {
		if p.skipFrom >= 0 {
			to := p.items[0].GetOffset()
			if to > p.skipFrom {
				if c.available() == 0 {
					p.starve()
					return false, nil
				}
				gap := &athenav1.StreamGap{TicketId: ticketID, From: p.skipFrom, To: to}
				if err := c.sess.Send(athenav1.Channel_STREAM, false, &athenav1.Frame{Body: &athenav1.Frame_StreamGap{StreamGap: gap}}); err != nil {
					return errors.Is(err, link.ErrBackpressure), streamErr(err)
				}
			}
			p.skipFrom = -1
		}
		n, bytes := 0, 0
		for n < len(p.items) && n < itemsPerFrame && (n == 0 || bytes+size(p.items[n]) <= frameBytes) {
			bytes += size(p.items[n])
			n++
		}
		k := c.take(n)
		if k == 0 {
			p.starve()
			return false, nil
		}
		frame := &athenav1.Frame{Body: &athenav1.Frame_StreamItems{StreamItems: &athenav1.StreamItems{
			TicketId: ticketID, Items: append([]*athenav1.StreamItem(nil), p.items[:k]...)}}}
		if err := c.sess.Send(athenav1.Channel_STREAM, false, frame); err != nil {
			c.refund(k)
			// The items the link could not take stay pending, tried again shortly.
			return errors.Is(err, link.ErrBackpressure), streamErr(err)
		}
		p.dropFront(k)
	}
	return false, nil
}

// streamErr is err unless it is a full queue, which the caller retries.
func streamErr(err error) error {
	if errors.Is(err, link.ErrBackpressure) {
		return nil
	}
	return err
}

// stopStreams ends every stream of the connection and waits for them.
func (c *conn) stopStreams() {
	c.streamCancel()
	c.streamWG.Wait()
}
