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
// total of STREAM items it has granted (link Session.Credit) is what the connection may have sent, in all its
// streams together. The connection never waits for credit and never queues for a slow client: each stream keeps the
// latest lines (bounded) and, when credit comes, sends a StreamGap over what it skipped and the latest tail.
//
// Items of one transcript line share its offset. A client that lost its connection resumes with
// from_offset = (offset of the last item it got) + 1: the next line, not that one again.

// Streams is where a connection gets a Ticket's session output; *node.Node is one.
type Streams interface {
	Subscribe(ticketID string, from int64) (backfill []node.Line, live <-chan node.Line, cancel func(), err error)
}

const (
	// maxStreams is how many Tickets one connection streams at once.
	maxStreams = 16
	// tailItems is how many of the latest items follow a gap.
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

// credit returns what the streams may send now and the channel that closes at the next grant. Read the channel
// first and the credit after, and no grant is missed.
func (c *conn) credit() (avail int, wake <-chan struct{}) {
	c.smu.Lock()
	wake = c.creditWake
	sent := c.itemsSent
	c.smu.Unlock()
	if granted := uint64(c.sess.Credit(athenav1.Channel_STREAM)); granted > sent {
		avail = int(min(granted-sent, 1<<30))
	}
	return avail, wake
}

// reserve takes n items of credit (n <= what credit returned) and refund gives them back.
func (c *conn) reserve(n int) { c.smu.Lock(); c.itemsSent += uint64(n); c.smu.Unlock() }
func (c *conn) refund(n int)  { c.smu.Lock(); c.itemsSent -= uint64(n); c.smu.Unlock() }

// pending is a stream's unsent items, oldest first, all newer than skipFrom when that is set.
type pending struct {
	items []*athenav1.StreamItem
	bytes int
	// skipFrom is the offset of the first item lost without being sent, or -1.
	skipFrom int64
	// starved is set when items were held back for lack of credit: a grant then finds them stale.
	starved bool
	lastAt  time.Time
}

func size(it *athenav1.StreamItem) int { return len(it.GetText()) + 64 }

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

// absorb takes a line from the node: rendered, bounded, and dated when it has no time of its own.
func (p *pending) absorb(l node.Line) {
	if l.GapTo != 0 {
		// The node dropped lines for this stream: everything held is older than the loss, and goes with it.
		if len(p.items) > 0 {
			p.markSkip(p.items[0].GetOffset())
			p.dropFront(len(p.items))
		}
		p.markSkip(l.Offset)
		return
	}
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
	over := 0
	for bytes := p.bytes; over < len(p.items)-1 && (len(p.items)-over > pendingItems || bytes > pendingBytes); over++ {
		bytes -= size(p.items[over])
	}
	if over > 0 {
		p.markSkip(p.items[0].GetOffset())
		p.dropFront(over)
	}
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
		c.streamFailed(st.ticketID, CodeUnavailable, "cannot stream this ticket")
		return
	}
	defer cancel()

	p := &pending{skipFrom: -1}
	for _, l := range backfill {
		p.absorb(l)
	}
	var retry <-chan time.Time
	granted := false
	for {
		avail, wake := c.credit()
		backpressured, err := c.flush(st.ticketID, p, avail, granted && p.starved)
		if err != nil {
			return
		}
		granted, retry = false, nil
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
			granted = true
		case <-retry:
		}
	}
}

// flush sends what credit and the link allow of p, in order. When skip is set (a grant came to a stream that was out
// of credit) and more is pending than the grant covers, it skips ahead instead: a StreamGap over what it leaves out,
// then the latest items. It reports whether the link's STREAM queue was full; the error is the
// link having closed.
func (c *conn) flush(ticketID string, p *pending, avail int, skip bool) (backpressured bool, err error) {
	if len(p.items) == 0 {
		return false, nil
	}
	if avail == 0 {
		p.starved = true
		return false, nil
	}
	if keep := min(avail, tailItems); skip && len(p.items) > avail && len(p.items) > keep {
		p.markSkip(p.items[0].GetOffset())
		p.dropFront(len(p.items) - keep)
	}
	if p.skipFrom >= 0 {
		gap := &athenav1.StreamGap{TicketId: ticketID, From: p.skipFrom, To: p.items[0].GetOffset()}
		if err := c.sess.Send(athenav1.Channel_STREAM, false, &athenav1.Frame{Body: &athenav1.Frame_StreamGap{StreamGap: gap}}); err != nil {
			return errors.Is(err, link.ErrBackpressure), streamErr(err)
		}
		p.skipFrom = -1
	}
	for len(p.items) > 0 {
		n, bytes := 0, 0
		for n < len(p.items) && n < itemsPerFrame && n < avail && (n == 0 || bytes+size(p.items[n]) <= frameBytes) {
			bytes += proto.Size(p.items[n]) + 8
			n++
		}
		if n == 0 {
			return false, nil // no credit left
		}
		c.reserve(n)
		frame := &athenav1.Frame{Body: &athenav1.Frame_StreamItems{StreamItems: &athenav1.StreamItems{
			TicketId: ticketID, Items: append([]*athenav1.StreamItem(nil), p.items[:n]...)}}}
		if err := c.sess.Send(athenav1.Channel_STREAM, false, frame); err != nil {
			c.refund(n)
			// The items the link could not take stay pending, tried again shortly.
			return errors.Is(err, link.ErrBackpressure), streamErr(err)
		}
		avail -= n
		p.dropFront(n)
		if avail == 0 {
			break
		}
	}
	p.starved = len(p.items) > 0 && avail == 0
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
