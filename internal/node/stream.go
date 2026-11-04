package node

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// The live session stream: every transcript line of a Ticket's sessions, by byte offset, to the dashboards watching
// it. The node keeps raw lines (rendering belongs to the dashboard layer) in a bounded ring per Ticket; older lines
// are read back from the transcript file on demand. A subscriber that cannot keep up loses lines, never the node's
// or anyone else's time: the loss shows as a gap line in its own channel.

const (
	// ringMaxLines and ringMaxBytes bound a Ticket's ring: 2000 lines or 1 MiB of them, whichever is less.
	ringMaxLines = 2000
	ringMaxBytes = 1 << 20
	// maxLineBytes is the longest line kept; a longer one is kept as its offset alone (Line.Data nil).
	maxLineBytes = 256 << 10
	// subscriberBuffer is a live channel's capacity.
	subscriberBuffer = 512
	// TailLines is how many lines from = -1 asks for.
	TailLines = 200
	// tailScanBytes bounds how much of a transcript's end is read to find the last TailLines lines.
	tailScanBytes = 4 << 20
)

// ringTTL is how long a Ticket's ring outlives its Ticket's end when nobody watches it.
var ringTTL = 10 * time.Minute

// Line is one transcript line, or a gap in the stream.
type Line struct {
	// Offset is the byte offset the line starts at in the Ticket's transcript file. Each line takes len(Data)+1 bytes
	// there.
	Offset int64
	// Data is the line without its newline. It is nil for a line too long to keep, and for a gap.
	Data []byte
	// GapTo, when not zero, makes this no line but a gap: the transcript from Offset up to GapTo was not delivered
	// (the subscriber fell behind, or the history asked for is longer than a backfill).
	GapTo int64
}

// streamHub holds the rings. The zero value is ready.
type streamHub struct {
	mu    sync.Mutex
	rings map[string]*ring
}

type ring struct {
	lines []Line // oldest first, contiguous
	bytes int
	subs  map[*subscriber]struct{}
	// ended is set when the Ticket ended: the ring is freed ringTTL after its last line or last subscriber.
	ended bool
	timer *time.Timer
}

type subscriber struct {
	ch chan Line
	// holding is set while Subscribe computes the backfill: lines go to held, and reach ch after it.
	holding bool
	held    []Line
	// dropFrom is the offset of the first line lost since the subscriber's channel last had room, or -1.
	dropFrom int64
	once     sync.Once
}

// validStreamID reports whether id can name a Ticket's stream: it ends up in a file name.
func validStreamID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00")
}

// add records a line of a Ticket's sessions and offers it to the subscribers. It never blocks.
func (h *streamHub) add(ticketID string, offset int64, data []byte) {
	if len(data) > maxLineBytes {
		data = nil
	}
	l := Line{Offset: offset, Data: data}
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.ringLocked(ticketID)
	r.lines = append(r.lines, l)
	r.bytes += len(data)
	for len(r.lines) > 1 && (len(r.lines) > ringMaxLines || r.bytes > ringMaxBytes) {
		r.bytes -= len(r.lines[0].Data)
		r.lines[0] = Line{}
		r.lines = r.lines[1:]
	}
	if len(r.lines) == 0 { // keep the backing array from growing with the slicing
		r.lines = nil
	}
	for s := range r.subs {
		s.deliver(l)
	}
	if r.ended {
		h.armLocked(ticketID, r)
	}
}

func (h *streamHub) ringLocked(id string) *ring {
	if h.rings == nil {
		h.rings = map[string]*ring{}
	}
	r := h.rings[id]
	if r == nil {
		r = &ring{subs: map[*subscriber]struct{}{}}
		h.rings[id] = r
	}
	return r
}

// deliver offers l to the subscriber without blocking. A subscriber whose channel is full loses the line and is
// told where the loss began, ahead of the next line it has room for.
func (s *subscriber) deliver(l Line) {
	switch {
	case s.holding:
		if len(s.held) < subscriberBuffer {
			s.held = append(s.held, l)
		} else if s.dropFrom < 0 {
			s.dropFrom = l.Offset
		}
	case s.dropFrom >= 0:
		if cap(s.ch)-len(s.ch) >= 2 { // only the publisher fills the channel, so the room stays
			s.ch <- Line{Offset: s.dropFrom, GapTo: l.Offset}
			s.ch <- l
			s.dropFrom = -1
		}
	default:
		select {
		case s.ch <- l:
		default:
			s.dropFrom = l.Offset
		}
	}
}

// end marks a Ticket's stream over: its ring is freed ringTTL later, once nobody watches it.
func (h *streamHub) end(ticketID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r := h.rings[ticketID]; r != nil {
		r.ended = true
		h.armLocked(ticketID, r)
	}
}

// armLocked (re)starts the free timer of an ended ring without subscribers.
func (h *streamHub) armLocked(id string, r *ring) {
	if len(r.subs) > 0 {
		return
	}
	if r.timer != nil {
		r.timer.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(ringTTL, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.rings[id] == r && r.timer == t && r.ended && len(r.subs) == 0 {
			delete(h.rings, id)
		}
	})
	r.timer = t
}

// Subscribe starts a feed of a Ticket's transcript lines.
//
// backfill is what was written from byte offset from on, oldest first; from = -1 asks for the last TailLines lines,
// and a from past the end gives none. A from that is not at the start of a line begins at the next line. A backfill
// longer than a ring holds is cut to its latest part and starts with a gap Line. live then carries every later line,
// none of them in backfill. When live falls behind (its capacity is 512) the node drops lines for this subscriber
// only; the next line it gets is preceded by a gap Line. cancel ends the feed and closes live; it is safe to call more
// than once.
func (n *Node) Subscribe(ticketID string, from int64) (backfill []Line, live <-chan Line, cancel func(), err error) {
	if !validStreamID(ticketID) {
		return nil, nil, nil, fmt.Errorf("node: bad ticket id %q", ticketID)
	}
	h := &n.streams
	s := &subscriber{ch: make(chan Line, subscriberBuffer), holding: true, dropFrom: -1}
	h.mu.Lock()
	r := h.ringLocked(ticketID)
	snap := slices.Clone(r.lines)
	r.subs[s] = struct{}{}
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	h.mu.Unlock()

	cancel = func() {
		s.once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			delete(r.subs, s)
			close(s.ch)
			if len(r.subs) == 0 {
				switch {
				case r.ended:
					h.armLocked(ticketID, r)
				case len(r.lines) == 0 && h.rings[ticketID] == r:
					delete(h.rings, ticketID) // a watch of a Ticket that never wrote anything
				}
			}
		})
	}

	backfill, err = n.backfill(ticketID, from, snap)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	last := int64(-1)
	for i := len(backfill) - 1; i >= 0; i-- {
		if backfill[i].GapTo == 0 {
			last = backfill[i].Offset
			break
		}
	}
	h.mu.Lock()
	s.holding = false
	for _, l := range s.held {
		if l.Offset > last { // a line the file read saw already is in the backfill
			s.ch <- l // at most subscriberBuffer held, an empty channel
		}
	}
	s.held = nil
	h.mu.Unlock()
	return backfill, s.ch, cancel, nil
}

// backfill returns the lines from asks for, given the ring's snapshot, reading the transcript file for what the ring
// no longer holds.
func (n *Node) backfill(ticketID string, from int64, ring []Line) ([]Line, error) {
	path := n.transcriptPath(ticketID)
	until := int64(-1) // the file is read up to here: where the ring starts, or its end
	if len(ring) > 0 {
		until = ring[0].Offset
	}
	if from < 0 {
		if len(ring) >= TailLines {
			return ring[len(ring)-TailLines:], nil
		}
		if path == "" {
			return ring, nil
		}
		start := int64(0)
		if size, err := fileSize(path, until); err != nil {
			return nil, err
		} else if size > tailScanBytes {
			start = size - tailScanBytes
		}
		older, _, err := readLines(path, start, until, TailLines-len(ring), ringMaxBytes)
		if err != nil {
			return nil, err
		}
		return append(older, ring...), nil
	}
	if len(ring) > 0 && from >= until {
		i, _ := slices.BinarySearchFunc(ring, from, func(l Line, off int64) int { return cmpInt64(l.Offset, off) })
		return ring[i:], nil
	}
	var older []Line
	if path != "" {
		var gap *Line
		var err error
		if older, gap, err = readLines(path, from, until, ringMaxLines, ringMaxBytes); err != nil {
			return nil, err
		}
		if gap != nil {
			older = append([]Line{*gap}, older...)
		}
	} else if until > from {
		older = []Line{{Offset: from, GapTo: until}} // no transcript to read the rest from
	}
	return append(older, ring...), nil
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// fileSize is the size of the file, or until when that is smaller and set; a missing file is size 0.
func fileSize(path string, until int64) (int64, error) {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("node: stat transcript: %w", err)
	}
	if until >= 0 && until < st.Size() {
		return until, nil
	}
	return st.Size(), nil
}

// readLines reads the complete lines of the transcript that start at or after from and before until (until < 0: to
// the end), keeping at most the last maxLines of them and maxBytes of their data. When it had to drop earlier ones,
// gap says what was left out, from the first line it read to the first it kept. A from inside a line begins at the
// next line; a missing file has no lines.
func readLines(path string, from, until int64, maxLines, maxBytes int) (lines []Line, gap *Line, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("node: open transcript: %w", err)
	}
	defer f.Close()
	pos := from
	if from > 0 {
		pos = from - 1 // from is at a line start when the byte before it ends a line
	}
	if _, err := f.Seek(pos, io.SeekStart); err != nil {
		return nil, nil, fmt.Errorf("node: seek transcript: %w", err)
	}
	r := bufio.NewReaderSize(f, 64<<10)
	if from > 0 {
		skipped, err := skipLine(r)
		if err != nil {
			return nil, nil, nil
		}
		pos += skipped
	}
	first := int64(-1)
	var size int
	for until < 0 || pos < until {
		data, n, complete, err := readLine(r)
		if err != nil || !complete {
			break // the end, or a line still being written
		}
		if first < 0 {
			first = pos
		}
		lines = append(lines, Line{Offset: pos, Data: data})
		size += len(data)
		pos += int64(n)
		for len(lines) > 1 && (len(lines) > maxLines || size > maxBytes) {
			size -= len(lines[0].Data)
			lines[0] = Line{}
			lines = lines[1:]
		}
	}
	if len(lines) > 0 && lines[0].Offset > first {
		gap = &Line{Offset: first, GapTo: lines[0].Offset}
	}
	return slices.Clone(lines), gap, nil
}

// skipLine reads through the next newline and returns how many bytes that took.
func skipLine(r *bufio.Reader) (int64, error) {
	var n int64
	for {
		b, err := r.ReadSlice('\n')
		n += int64(len(b))
		if err == bufio.ErrBufferFull {
			continue
		}
		return n, err
	}
}

// readLine reads one line: its data (nil when longer than maxLineBytes), its size in the file with the newline, and
// whether it ended in a newline.
func readLine(r *bufio.Reader) (data []byte, n int, complete bool, err error) {
	var buf []byte
	tooLong := false
	for {
		b, err := r.ReadSlice('\n')
		n += len(b)
		if !tooLong {
			if len(buf)+len(b) > maxLineBytes+1 {
				tooLong, buf = true, nil
			} else {
				buf = append(buf, b...)
			}
		}
		switch err {
		case bufio.ErrBufferFull:
			continue
		case nil:
			if tooLong {
				return nil, n, true, nil
			}
			return buf[:len(buf)-1], n, true, nil
		default:
			if err == io.EOF {
				err = nil
			}
			return nil, n, false, err
		}
	}
}
