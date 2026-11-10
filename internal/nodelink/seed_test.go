package nodelink

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// seedTarget is a fakeTarget that records each Seed, with its bundle's bytes as Seed saw them, and when it came
// against the Starts.
type seedTarget struct {
	fakeTarget
	seeds   []node.Seed
	bundles [][]byte
	events  []string
}

func (s *seedTarget) Seed(sd node.Seed) error {
	var b []byte
	if sd.Bundle != "" {
		var err error
		if b, err = os.ReadFile(sd.Bundle); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seeds = append(s.seeds, sd)
	s.bundles = append(s.bundles, b)
	s.events = append(s.events, "seed")
	return nil
}

func (s *seedTarget) Start(t core.Ticket, a core.Assignment) {
	s.fakeTarget.Start(t, a)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "start")
}

func (fs *fakeServer) blob(t *testing.T, c *athenav1.BlobChunk) {
	t.Helper()
	if err := fs.sess.Send(athenav1.Channel_BLOB, true, &athenav1.Frame{Body: &athenav1.Frame_BlobChunk{BlobChunk: c}}); err != nil {
		t.Fatal(err)
	}
}

// TestSeedChunksThenStart: the server's seed comes as BlobChunks of kind "seed", then the seed call, then the Start;
// the node gets the whole bundle before the Start, and the transfer's file is gone after. A seed call naming no bundle
// that arrived whole seeds without one; chunks of another kind are not taken.
func TestSeedChunksThenStart(t *testing.T) {
	srv := newServers(t)
	c := newTestClient(t, srv)
	tgt := &seedTarget{}
	c.attach(tgt)
	fs := srv.add(0)
	run(t, c)
	eventually(t, "online", c.Online)

	bundle := bytes.Repeat([]byte("seed-bytes-0123456789"), (1<<20)/21) // two chunks
	sha := strings.Repeat("b", 40)
	at := time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)
	fs.blob(t, &athenav1.BlobChunk{TransferId: "x1", Kind: "seed", TicketId: "T-0001", Data: bundle[:blobChunk]})
	fs.blob(t, &athenav1.BlobChunk{TransferId: "x1", Kind: "seed", TicketId: "T-0001", Data: bundle[blobChunk:], Last: true})
	// Another kind is not a seed: dropped.
	fs.blob(t, &athenav1.BlobChunk{TransferId: "x2", Kind: "checkpoint", TicketId: "T-0001", Data: []byte("nope"), Last: true})
	// Wait for the writer to take the chunks, as the fleet does, so the calls come after them.
	eventually(t, "the chunks sent", func() bool { return fs.sess.Queued(athenav1.Channel_BLOB) == 0 })
	fs.call(t, 101, ServeSeed, ArgsSeed{TicketID: "T-0001", AssignmentID: "A-1", Transfer: "x1", SHA: sha, At: at, From: "studio", Offline: true})
	fs.call(t, 102, ServeStart, ArgsStart{Ticket: core.Ticket{ID: "T-0001"}, Assignment: core.Assignment{ID: "A-1"}})
	eventually(t, "the start", func() bool { return fs.result(102) != nil })
	if r := fs.result(101); r == nil || r.GetError() != "" {
		t.Fatalf("seed result = %v", r)
	}
	tgt.mu.Lock()
	events, seeds, bundles := append([]string(nil), tgt.events...), tgt.seeds, tgt.bundles
	tgt.mu.Unlock()
	if strings.Join(events, ",") != "seed,start" {
		t.Fatalf("events = %v, want the seed before the start", events)
	}
	sd := seeds[0]
	if sd.TicketID != "T-0001" || sd.AssignmentID != "A-1" || sd.SHA != sha || !sd.At.Equal(at) || sd.From != "studio" || !sd.Offline {
		t.Fatalf("seed = %+v", sd)
	}
	if !bytes.Equal(bundles[0], bundle) {
		t.Fatalf("the bundle arrived as %d bytes, want %d", len(bundles[0]), len(bundle))
	}
	if _, err := os.Stat(sd.Bundle); !os.IsNotExist(err) {
		t.Fatalf("the transfer's file is still there: %v", err)
	}

	// The checkpoint-kind transfer is no seed; a seed call naming it seeds without a bundle.
	fs.call(t, 103, ServeSeed, ArgsSeed{TicketID: "T-0001", AssignmentID: "A-2", Transfer: "x2", SHA: sha, From: "studio"})
	eventually(t, "the second seed", func() bool { return fs.result(103) != nil })
	tgt.mu.Lock()
	last := tgt.seeds[len(tgt.seeds)-1]
	tgt.mu.Unlock()
	if last.AssignmentID != "A-2" || last.Bundle != "" {
		t.Fatalf("seed = %+v, want no bundle", last)
	}
	entries, _ := os.ReadDir(c.tmpDir)
	if len(entries) != 0 {
		t.Fatalf("temp files left: %v", entries)
	}
}
