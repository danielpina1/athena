package nodelink

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// receivedSince returns the blobs and calls the server has got since from (in fs.order).
func (fs *fakeServer) snapshot() (order []string, blobs []*athenav1.BlobChunk) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return slices.Clone(fs.order), slices.Clone(fs.blobs)
}

func (fs *fakeServer) checkpointFacts() []ArgsCheckpointSent {
	var out []ArgsCheckpointSent
	for _, c := range fs.facts() {
		if c.GetMethod() == MethodCheckpointSent {
			var a ArgsCheckpointSent
			_ = json.Unmarshal(c.GetArgs(), &a)
			out = append(out, a)
		}
	}
	return out
}

// A checkpoint goes as BlobChunks of at most 512 KiB on BLOB, then the CheckpointSent fact; the server's answer makes
// it the base of the next bundle, and a refusal makes the next one whole.
func TestCheckpointChunksThenFact(t *testing.T) {
	srv := newServers(t)
	c := newTestClient(t, srv)
	tgt := &fakeTarget{}
	c.attach(tgt)
	fs := srv.add(0)
	refuse := make(chan struct{})
	fs.answer = func(call *athenav1.NodeCall) *athenav1.NodeResult {
		if call.GetMethod() == MethodCheckpointSent {
			select {
			case <-refuse:
				return &athenav1.NodeResult{Error: "the checkpoint's bundle did not reach the server", UserError: true}
			default:
			}
		}
		return nil
	}
	run(t, c)
	eventually(t, "online", c.Online)

	bundle := bytes.Repeat([]byte("0123456789abcdef"), (1<<20+300<<10)/16) // 1.3 MiB: three chunks
	sha1 := strings.Repeat("a", 40)
	tgt.setCheckpoint(bundle, sha1)
	c.Checkpointed("T-0001", "o/r", "refs/athena/t-0001/checkpoint", sha1)
	eventually(t, "the fact", func() bool { return len(fs.checkpointFacts()) == 1 })
	order, blobs := fs.snapshot()
	if i := slices.Index(order, MethodCheckpointSent); i < 3 || slices.Contains(order[i:], "blob") {
		t.Fatalf("order %v: the fact came before its chunks", order)
	}
	if len(blobs) != 3 {
		t.Fatalf("%d chunks, want 3", len(blobs))
	}
	var got []byte
	for i, b := range blobs {
		if len(b.GetData()) > 512<<10 || b.GetKind() != "checkpoint" || b.GetTicketId() != "T-0001" ||
			b.GetTransferId() != blobs[0].GetTransferId() || b.GetTransferId() == "" || b.GetLast() != (i == len(blobs)-1) {
			t.Fatalf("chunk %d: %d bytes, kind %q, ticket %q, transfer %q, last %v", i, len(b.GetData()), b.GetKind(),
				b.GetTicketId(), b.GetTransferId(), b.GetLast())
		}
		got = append(got, b.GetData()...)
	}
	if !bytes.Equal(got, bundle) {
		t.Fatal("the chunks do not make the bundle")
	}
	if f := fs.checkpointFacts()[0]; f != (ArgsCheckpointSent{TicketID: "T-0001", Repo: "o/r", SHA: sha1}) {
		t.Fatalf("fact %+v", f)
	}
	eventually(t, "the base is recorded", func() bool { b, _ := c.ob.checkpointSent("T-0001"); return b == sha1 })

	// The next goes after it; the same checkpoint again sends nothing.
	sha2 := strings.Repeat("b", 40)
	tgt.setCheckpoint([]byte("small"), sha2)
	c.Checkpointed("T-0001", "o/r", "refs/athena/t-0001/checkpoint", sha2)
	eventually(t, "the second fact", func() bool { return len(fs.checkpointFacts()) == 2 })
	eventually(t, "the second base", func() bool { b, _ := c.ob.checkpointSent("T-0001"); return b == sha2 })
	c.Checkpointed("T-0001", "o/r", "refs/athena/t-0001/checkpoint", sha2)
	eventually(t, "the third bundle asked", func() bool { return len(tgt.bases()) == 3 })
	if b := tgt.bases(); b[0] != "" || b[1] != sha1 || b[2] != sha2 {
		t.Fatalf("bases %q", b)
	}
	// A refusal forgets the base: the next bundle is whole.
	close(refuse)
	sha3 := strings.Repeat("c", 40)
	tgt.setCheckpoint([]byte("refused"), sha3)
	c.Checkpointed("T-0001", "o/r", "refs/athena/t-0001/checkpoint", sha3)
	eventually(t, "the base is forgotten", func() bool {
		b, _ := c.ob.checkpointSent("T-0001")
		return len(fs.checkpointFacts()) == 3 && b == ""
	})
	if n := len(fs.checkpointFacts()); n != 3 {
		t.Fatalf("%d facts, want 3", n)
	}
}
