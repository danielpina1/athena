package nodelink

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielpina1/athena/internal/node"
)

func TestServeCalls(t *testing.T) {
	srv := newServers(t)
	c := newTestClient(t, srv)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, maxOpenFile+1), 0o600); err != nil {
		t.Fatal(err)
	}
	tgt := &fakeTarget{dir: dir, backfill: []node.Line{{Offset: 0, Data: []byte("one")}}, live: make(chan node.Line, 4)}
	c.attach(tgt)
	fs := srv.add(0)
	run(t, c)
	eventually(t, "online", c.Online)

	fs.call(t, 1, ServeOpenFile, ArgsOpenFile{TicketID: "T1", Path: "small.txt"})
	fs.call(t, 2, ServeOpenFile, ArgsOpenFile{TicketID: "T1", Path: "big.bin"})
	fs.call(t, 3, ServeSeed, struct{}{})
	fs.call(t, 4, ServeInterrupt, ArgsInterrupt{TicketID: "T1"})
	fs.call(t, 5, ServeStart, map[string]any{"Ticket": map[string]any{}, "Extra": 1})
	for id := uint64(1); id <= 5; id++ {
		eventually(t, "a result", func() bool { return fs.result(id) != nil })
	}
	if r := fs.result(1); r.GetError() != "" || string(r.GetValue()) != "hello" {
		t.Fatalf("small file = %q %q", r.GetValue(), r.GetError())
	}
	if r := fs.result(2); r.GetError() != "too large to show from another node" || !r.GetUserError() {
		t.Fatalf("big file err = %q", r.GetError())
	}
	if r := fs.result(3); r.GetError() != "not supported by this node yet" {
		t.Fatalf("seed err = %q", r.GetError())
	}
	if r := fs.result(4); DecodeError(r.GetError(), r.GetUserError()) != node.ErrNoSession {
		t.Fatalf("interrupt err = %q", r.GetError())
	}
	if r := fs.result(5); !strings.Contains(r.GetError(), "unknown field") {
		t.Fatalf("strict args err = %q", r.GetError())
	}

	// The owner held a repository: the node drops its write tokens.
	fs.call(t, 7, ServeDropTokens, ArgsDropTokens{Repo: "o/r"})
	eventually(t, "the dropTokens result", func() bool { return fs.result(7) != nil })
	if r := fs.result(7); r.GetError() != "" {
		t.Fatalf("dropTokens err = %q", r.GetError())
	}
	tgt.mu.Lock()
	dropped := append([]string(nil), tgt.dropped...)
	tgt.mu.Unlock()
	if len(dropped) != 1 || dropped[0] != "o/r" {
		t.Fatalf("dropped = %v, want [o/r]", dropped)
	}

	// A subscription: its answer, then the backfill, then live lines, then the close.
	fs.call(t, 6, ServeSubscribe, ArgsSubscribe{TicketID: "T1", From: -1})
	eventually(t, "the subscribe result", func() bool { return fs.result(6) != nil })
	tgt.live <- node.Line{Offset: 4, Data: []byte("two")}
	close(tgt.live)
	var got []string
	closed := false
	eventually(t, "the lines", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		got, closed = nil, false
		for _, call := range fs.calls {
			if call.GetMethod() != MethodLines {
				continue
			}
			var a ArgsLines
			if err := json.Unmarshal(call.GetArgs(), &a); err != nil || a.Sub != 6 {
				t.Errorf("lines %s: %v", call.GetArgs(), err)
			}
			for _, l := range a.Lines {
				got = append(got, string(l.Data))
			}
			closed = closed || a.Closed
		}
		return closed
	})
	if strings.Join(got, ",") != "one,two" {
		t.Fatalf("lines = %v", got)
	}
}
