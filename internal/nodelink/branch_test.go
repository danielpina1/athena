package nodelink

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// branchTarget is a seedTarget that records each ImportBranch, with its bundle's bytes as ImportBranch saw them.
type branchTarget struct {
	seedTarget
	imports []node.BranchImport
	data    [][]byte
}

func (b *branchTarget) ImportBranch(bi node.BranchImport) error {
	data, err := os.ReadFile(bi.Bundle)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.imports = append(b.imports, bi)
	b.data = append(b.data, data)
	return nil
}

func (b *branchTarget) got() ([]node.BranchImport, [][]byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]node.BranchImport(nil), b.imports...), append([][]byte(nil), b.data...)
}

// TestBranchChunksThenImport: a shipped branch comes as BlobChunks of kind "branch", then the importBranch call; the
// node imports the whole bundle and the transfer's file is gone after. A fork seed's Fork and Base reach the node.
func TestBranchChunksThenImport(t *testing.T) {
	srv := newServers(t)
	c := newTestClient(t, srv)
	tgt := &branchTarget{}
	c.attach(tgt)
	fs := srv.add(0)
	run(t, c)
	eventually(t, "online", c.Online)

	bundle := bytes.Repeat([]byte("branch-bytes-0123456789"), (1<<20)/23) // two chunks
	sha := strings.Repeat("c", 40)
	fs.blob(t, &athenav1.BlobChunk{TransferId: "b1", Kind: "branch", TicketId: "T-0002", Data: bundle[:blobChunk]})
	fs.blob(t, &athenav1.BlobChunk{TransferId: "b1", Kind: "branch", TicketId: "T-0002", Data: bundle[blobChunk:], Last: true})
	eventually(t, "the chunks sent", func() bool { return fs.sess.Queued(athenav1.Channel_BLOB) == 0 })
	fs.call(t, 201, ServeImportBranch, ArgsImportBranch{TicketID: "T-0001", Child: "T-0002", SHA: sha, Transfer: "b1"})
	eventually(t, "the import answered", func() bool { return fs.result(201) != nil })
	if r := fs.result(201); r.GetError() != "" {
		t.Fatalf("import = %s", r.GetError())
	}
	imports, data := tgt.got()
	if len(imports) != 1 {
		t.Fatalf("imports = %+v", imports)
	}
	if bi := imports[0]; bi.TicketID != "T-0001" || bi.Child != "T-0002" || bi.SHA != sha {
		t.Fatalf("import = %+v", bi)
	}
	if !bytes.Equal(data[0], bundle) {
		t.Fatalf("the bundle arrived as %d bytes, want %d", len(data[0]), len(bundle))
	}
	if _, err := os.Stat(imports[0].Bundle); !os.IsNotExist(err) {
		t.Fatalf("the transfer's file is still there: %v", err)
	}

	// A seed transfer is no branch, and a branch transfer no seed.
	fs.blob(t, &athenav1.BlobChunk{TransferId: "s1", Kind: "seed", TicketId: "T-0002", Data: []byte("seed"), Last: true})
	fs.blob(t, &athenav1.BlobChunk{TransferId: "b2", Kind: "branch", TicketId: "T-0002", Data: []byte("branch"), Last: true})
	eventually(t, "the chunks sent", func() bool { return fs.sess.Queued(athenav1.Channel_BLOB) == 0 })
	fs.call(t, 202, ServeImportBranch, ArgsImportBranch{TicketID: "T-0001", Child: "T-0002", SHA: sha, Transfer: "s1"})
	eventually(t, "the import answered", func() bool { return fs.result(202) != nil })
	if r := fs.result(202); !strings.Contains(r.GetError(), "did not arrive whole") {
		t.Fatalf("an import of a seed's transfer = %q", r.GetError())
	}
	base := strings.Repeat("d", 40)
	fs.call(t, 203, ServeSeed, ArgsSeed{TicketID: "T-0002", AssignmentID: "A-1", Transfer: "b2", SHA: sha, From: "studio",
		Fork: true, Base: base})
	eventually(t, "the seed answered", func() bool { return fs.result(203) != nil })
	tgt.mu.Lock()
	sd := tgt.seeds[len(tgt.seeds)-1]
	tgt.mu.Unlock()
	if sd.Bundle != "" || sd.Fork || sd.Base != "" || sd.Unsent != node.SeedForkLate {
		t.Fatalf("seed = %+v, want a fork whose bundle is not there (a branch transfer) downgraded to %q", sd, node.SeedForkLate)
	}
	// A fork seed with its bundle whole keeps Fork and Base.
	fs.blob(t, &athenav1.BlobChunk{TransferId: "s2", Kind: "seed", TicketId: "T-0002", Data: []byte("seed"), Last: true})
	eventually(t, "the chunks sent", func() bool { return fs.sess.Queued(athenav1.Channel_BLOB) == 0 })
	fs.call(t, 204, ServeSeed, ArgsSeed{TicketID: "T-0002", AssignmentID: "A-2", Transfer: "s2", SHA: sha, From: "studio",
		Fork: true, Base: base})
	eventually(t, "the seed answered", func() bool { return fs.result(204) != nil })
	tgt.mu.Lock()
	sd = tgt.seeds[len(tgt.seeds)-1]
	tgt.mu.Unlock()
	if !sd.Fork || sd.Base != base || sd.Unsent != "" {
		t.Fatalf("seed = %+v, want the fork and its base", sd)
	}
	eventually(t, "no temp files left", func() bool {
		entries, _ := os.ReadDir(c.tmpDir)
		return len(entries) == 0
	})
}

// TestBranchBlobChunksBounded: a branch transfer past maxSeeds concurrent ones (seeds and branches) on one link is
// dropped, as a seed is: its importBranch fails, and no file is kept for it.
func TestBranchBlobChunksBounded(t *testing.T) {
	srv := newServers(t)
	c := newTestClient(t, srv)
	tgt := &branchTarget{}
	c.attach(tgt)
	fs := srv.add(0)
	run(t, c)
	eventually(t, "online", c.Online)

	for i := range maxSeeds {
		kind := "seed"
		if i%2 == 1 {
			kind = "branch"
		}
		fs.blob(t, &athenav1.BlobChunk{TransferId: "open" + string(rune('a'+i)), Kind: kind, TicketId: "T-0002", Data: []byte("x")})
	}
	fs.blob(t, &athenav1.BlobChunk{TransferId: "late", Kind: "branch", TicketId: "T-0002", Data: []byte("late"), Last: true})
	eventually(t, "the chunks sent", func() bool { return fs.sess.Queued(athenav1.Channel_BLOB) == 0 })
	fs.call(t, 301, ServeImportBranch, ArgsImportBranch{TicketID: "T-0001", Child: "T-0002", SHA: strings.Repeat("c", 40), Transfer: "late"})
	eventually(t, "the import answered", func() bool { return fs.result(301) != nil })
	if r := fs.result(301); !strings.Contains(r.GetError(), "did not arrive whole") {
		t.Fatalf("an import past the bound = %q", r.GetError())
	}
	if imports, _ := tgt.got(); len(imports) != 0 {
		t.Fatalf("imported %+v", imports)
	}
	entries, _ := os.ReadDir(c.tmpDir)
	if len(entries) != maxSeeds {
		t.Fatalf("%d temp files, want the %d open transfers' only", len(entries), maxSeeds)
	}
}
