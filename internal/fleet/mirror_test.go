package fleet

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/nodelink"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// gitT runs git in dir with the test's own environment and returns its trimmed output.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// nodeRepo is a node's repository: one commit on main.
func nodeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "README")
	gitT(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// checkpointIn commits the worktree, plus a file name with body, on HEAD to the Ticket's checkpoint ref as the node
// does: no branch moves. It returns the commit.
func checkpointIn(t *testing.T, repo, ticketID, name, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", name)
	tree := gitT(t, repo, "write-tree")
	sha := gitT(t, repo, "commit-tree", "-p", "HEAD", "-m", "checkpoint", tree)
	gitT(t, repo, "update-ref", node.CheckpointRef(ticketID), sha)
	return sha
}

// bundleOf is the bundle of the Ticket's checkpoint ref after since ("" for all of it).
func bundleOf(t *testing.T, repo, ticketID, since string) []byte {
	t.Helper()
	file := filepath.Join(t.TempDir(), "b.bundle")
	args := []string{"bundle", "create", file, node.CheckpointRef(ticketID)}
	if since != "" {
		args = append(args, "^"+since)
	}
	gitT(t, repo, args...)
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMirrorReceivesIncrementalBundles(t *testing.T) {
	repo := nodeRepo(t)
	m := NewMirror(filepath.Join(t.TempDir(), "mirrors"))
	first := checkpointIn(t, repo, "T-0001", "a.txt", "half\n")
	full := bundleOf(t, repo, "T-0001", "")
	if err := m.Receive("Acme/Web", "T-0001", bytes.NewReader(full)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(m.dir, "acme", "web.git")
	if gitT(t, dir, "rev-parse", "--is-bare-repository") != "true" {
		t.Fatal("the mirror is not bare")
	}
	if sha, ok, err := m.Head("acme/web", "T-0001"); err != nil || !ok || sha != first {
		t.Fatalf("Head = %s %v %v, want %s", sha, ok, err, first)
	}
	second := checkpointIn(t, repo, "T-0001", "b.txt", "more\n")
	incr := bundleOf(t, repo, "T-0001", first)
	if len(incr) >= len(full)+64 {
		t.Logf("incremental bundle %d bytes, full %d", len(incr), len(full))
	}
	// A lock file a killed fetch left behind does not hold the ref.
	if err := os.WriteFile(filepath.Join(dir, "refs", "athena", "t-0001", "checkpoint.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Receive("acme/web", "T-0001", bytes.NewReader(incr)); err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, dir, "rev-parse", "refs/athena/t-0001/checkpoint"); got != second {
		t.Fatalf("mirror ref = %s, want the node's %s", got, second)
	}
	// Only the checkpoint ref is in the mirror: no branch came with the bundles.
	if refs := gitT(t, dir, "for-each-ref", "--format=%(refname)"); refs != "refs/athena/t-0001/checkpoint" {
		t.Fatalf("mirror refs = %q", refs)
	}
	// Bundle gives it back whole: a fresh repository fetches the work in progress from it.
	var out bytes.Buffer
	sha, err := m.Bundle("acme/web", "T-0001", &out)
	if err != nil || sha != second {
		t.Fatalf("Bundle = %s, %v; want %s", sha, err, second)
	}
	other := t.TempDir()
	gitT(t, other, "init", "-q")
	file := filepath.Join(t.TempDir(), "seed.bundle")
	if err := os.WriteFile(file, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	gitT(t, other, "fetch", "-q", file, "refs/athena/t-0001/checkpoint:refs/seed")
	if got := gitT(t, other, "show", "refs/seed:b.txt"); got != "more" {
		t.Fatalf("b.txt from the mirror's bundle = %q", got)
	}
	if _, err := m.Bundle("acme/web", "T-0002", io.Discard); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("Bundle of a Ticket with none = %v, want ErrNoCheckpoint", err)
	}
	if _, ok, err := m.Head("other/repo", "T-0001"); err != nil || ok {
		t.Fatalf("Head of a repository with no mirror = %v %v", ok, err)
	}
}

// A bundle is a node's word: only the Ticket's own ref is fetched from it, bad names are refused, and an incremental
// bundle whose base the mirror lacks fails without moving the ref.
func TestMirrorRefusesBadInput(t *testing.T) {
	repo := nodeRepo(t)
	m := NewMirror(filepath.Join(t.TempDir(), "mirrors"))
	sha := checkpointIn(t, repo, "T-0001", "a.txt", "x\n")
	full := bundleOf(t, repo, "T-0001", "")
	for _, c := range []struct{ repo, id string }{
		{"../x", "T-0001"}, {"o/..", "T-0001"}, {"o", "T-0001"}, {"o/r/x", "T-0001"}, {"-o/r", "T-0001"},
		{"o/r", "../T-1"}, {"o/r", ""}, {"o/r", "T 1"}, {"o/r", "t/1"},
	} {
		if err := m.Receive(c.repo, c.id, bytes.NewReader(full)); err == nil {
			t.Errorf("Receive(%q, %q) succeeded", c.repo, c.id)
		}
	}
	if entries, _ := os.ReadDir(m.dir); len(entries) != 0 {
		t.Fatalf("refused receives left %v", entries)
	}
	// A bundle of another Ticket's ref, or of a branch, writes nothing.
	gitT(t, repo, "update-ref", "refs/heads/evil", sha)
	file := filepath.Join(t.TempDir(), "evil.bundle")
	gitT(t, repo, "bundle", "create", file, "refs/heads/evil", node.CheckpointRef("T-0001"))
	evil, _ := os.ReadFile(file)
	if err := m.Receive("o/r", "T-0002", bytes.NewReader(evil)); err == nil {
		t.Fatal("a bundle without the Ticket's ref was taken")
	}
	if err := m.Receive("o/r", "T-0001", bytes.NewReader(evil)); err != nil {
		t.Fatal(err)
	}
	if refs := gitT(t, filepath.Join(m.dir, "o", "r.git"), "for-each-ref", "--format=%(refname)"); refs != "refs/athena/t-0001/checkpoint" {
		t.Fatalf("mirror refs = %q", refs)
	}
	// Not a bundle at all.
	if err := m.Receive("o/r", "T-0001", strings.NewReader("# v2 git bundle\nnonsense\n")); err == nil {
		t.Fatal("garbage was taken")
	}
	// An incremental bundle on a base the mirror never got.
	other := nodeRepo(t)
	gitT(t, other, "commit", "-q", "--allow-empty", "-m", "a history the mirror never saw")
	base := checkpointIn(t, other, "T-0003", "a.txt", "1\n")
	checkpointIn(t, other, "T-0003", "b.txt", "2\n")
	if err := m.Receive("o/r", "T-0003", bytes.NewReader(bundleOf(t, other, "T-0003", base))); err == nil {
		t.Fatal("an incremental bundle without its base was taken")
	}
	if _, ok, _ := m.Head("o/r", "T-0003"); ok {
		t.Fatal("the failed receive left a ref")
	}
	if got, _, _ := m.Head("o/r", "T-0001"); got != sha {
		t.Fatalf("T-0001 = %s, want %s", got, sha)
	}
	// Over the size bound.
	defer func(old int64) { maxBundle = old }(maxBundle)
	maxBundle = int64(len(full)) + 16
	big := io.MultiReader(bytes.NewReader(full), &zeroReader{n: 64})
	if err := m.Receive("o/r", "T-0001", big); err == nil || !strings.Contains(err.Error(), "MiB") {
		t.Fatalf("an oversized bundle = %v", err)
	}
}

type zeroReader struct{ n int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), z.n)
	clear(p[:n])
	z.n -= n
	return int(n), nil
}

// clock is a Fleet's Now that a test moves.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) set(at time.Time) {
	c.mu.Lock()
	c.at = at
	c.mu.Unlock()
}

// The local node's checkpoints go straight into the mirror; the daily sweep drops an ended Ticket's 14 days after it
// ended, and keeps an open one's.
func TestMirrorSweepsEnded(t *testing.T) {
	e := newEnv(t)
	repo := nodeRepo(t)
	m := NewMirror(filepath.Join(e.dir, "mirrors"))
	clk := &clock{at: time.Now()}
	f := e.fleet(func(c *Config) { c.Mirror, c.Now = m, clk.now })
	var mu sync.Mutex
	var bases []string
	f.localBundle = func(ctx context.Context, ticketID, since string, w io.Writer) (string, error) {
		mu.Lock()
		bases = append(bases, since)
		mu.Unlock()
		sha := gitT(t, repo, "rev-parse", node.CheckpointRef(ticketID))
		if sha == since {
			return sha, node.ErrCheckpointUnchanged
		}
		_, err := w.Write(bundleOf(t, repo, ticketID, since))
		return sha, err
	}
	ended := e.placedRepo("local", "acme/web")
	open := e.placedRepo("local", "acme/web")
	shas := map[string]string{}
	for _, tk := range []string{ended.ID, open.ID} {
		shas[tk] = checkpointIn(t, repo, tk, "a.txt", tk+"\n")
		f.LocalCheckpoint(tk, "acme/web", node.CheckpointRef(tk), shas[tk])
	}
	for _, tk := range []string{ended.ID, open.ID} {
		eventually(t, "the local checkpoint of "+tk+" is recorded", func() bool {
			c, ok, err := f.Checkpoint(tk)
			return err == nil && ok && c.SHA == shas[tk] && c.Repo == "acme/web" && c.Node == "local"
		})
	}
	// A second checkpoint goes after the first.
	next := checkpointIn(t, repo, open.ID, "b.txt", "more\n")
	f.LocalCheckpoint(open.ID, "acme/web", node.CheckpointRef(open.ID), next)
	eventually(t, "the second checkpoint is recorded", func() bool {
		c, ok, _ := f.Checkpoint(open.ID)
		return ok && c.SHA == next
	})
	mu.Lock()
	if bases[len(bases)-1] != shas[open.ID] {
		t.Errorf("the second local bundle's base = %q, want %s", bases[len(bases)-1], shas[open.ID])
	}
	mu.Unlock()
	shas[open.ID] = next

	if err := e.eng.StopTicket(ended.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if e.ticket(ended.ID).Ended == nil {
		t.Fatal("the Ticket did not end")
	}
	// 13 days on, both are kept.
	clk.set(time.Now().Add(13 * 24 * time.Hour))
	if err := f.SweepMirror(); err != nil {
		t.Fatal(err)
	}
	for tk, sha := range shas {
		if got, ok, _ := m.Head("acme/web", tk); !ok || got != sha {
			t.Fatalf("%s after 13 days: %s %v", tk, got, ok)
		}
	}
	// 15 days on, the ended Ticket's ref and record are gone; the open one's stay.
	clk.set(time.Now().Add(15 * 24 * time.Hour))
	if err := f.SweepMirror(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := m.Head("acme/web", ended.ID); ok {
		t.Fatal("the ended Ticket's checkpoint survived the sweep")
	}
	if _, ok, _ := f.Checkpoint(ended.ID); ok {
		t.Fatal("the ended Ticket's record survived the sweep")
	}
	if got, ok, _ := m.Head("acme/web", open.ID); !ok || got != shas[open.ID] {
		t.Fatal("the open Ticket's checkpoint was swept")
	}
	if _, ok, _ := f.Checkpoint(open.ID); !ok {
		t.Fatal("the open Ticket's record was swept")
	}
}

// blob sends one BlobChunk as the node's link does.
func (w *wireNode) blob(c *athenav1.BlobChunk) {
	w.t.Helper()
	for {
		err := w.sess.Send(athenav1.Channel_BLOB, true, &athenav1.Frame{Body: &athenav1.Frame_BlobChunk{BlobChunk: c}})
		if err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sendBundle sends b in chunks of size under transfer id.
func (w *wireNode) sendBundle(id, ticketID string, b []byte, size int) {
	for len(b) > size {
		w.blob(&athenav1.BlobChunk{TransferId: id, Kind: "checkpoint", TicketId: ticketID, Data: b[:size]})
		b = b[size:]
	}
	w.blob(&athenav1.BlobChunk{TransferId: id, Kind: "checkpoint", TicketId: ticketID, Data: b, Last: true})
}

// A remote node's checkpoint: its chunks, then the fact. The fleet takes it into the mirror only for a Ticket placed
// on the node, records it, and refuses a fact whose bundle never came.
func TestCheckpointOverTheLink(t *testing.T) {
	e := newEnv(t)
	e.addNode("n1", "mini")
	e.addNode("n2", "other")
	m := NewMirror(filepath.Join(e.dir, "mirrors"))
	f := e.fleet(func(c *Config) { c.Mirror = m })
	mine := e.placedRepo("n1", "acme/web")
	theirs := e.placedRepo("n2", "acme/web")
	repo := nodeRepo(t)
	sha := checkpointIn(t, repo, mine.ID, "a.txt", "wip\n")
	w := dialWire(t, f, "n1", hello("n1"))
	w.waitWelcome()

	w.sendBundle("x1", mine.ID, bundleOf(t, repo, mine.ID, ""), 100)
	w.fact(1, 1, nodelink.MethodCheckpointSent, nodelink.ArgsCheckpointSent{TicketID: mine.ID, Repo: "acme/web", SHA: sha})
	if r := w.result(); r.GetError() != "" {
		t.Fatalf("CheckpointSent refused: %s", r.GetError())
	}
	c, ok, err := f.Checkpoint(mine.ID)
	if err != nil || !ok || c.SHA != sha || c.Node != "n1" || c.Repo != "acme/web" {
		t.Fatalf("Checkpoint = %+v %v %v", c, ok, err)
	}
	if got, _, _ := m.Head("acme/web", mine.ID); got != sha {
		t.Fatalf("mirror = %s, want %s", got, sha)
	}

	// Another node's Ticket: neither its bundle nor its fact counts.
	other := checkpointIn(t, repo, theirs.ID, "b.txt", "theirs\n")
	w.sendBundle("x2", theirs.ID, bundleOf(t, repo, theirs.ID, ""), 1<<20)
	w.fact(2, 2, nodelink.MethodCheckpointSent, nodelink.ArgsCheckpointSent{TicketID: theirs.ID, Repo: "acme/web", SHA: other})
	if r := w.result(); !strings.Contains(r.GetError(), "not on this node") {
		t.Fatalf("CheckpointSent of another node's Ticket = %q", r.GetError())
	}
	if _, ok, _ := m.Head("acme/web", theirs.ID); ok {
		t.Fatal("another node's Ticket's bundle reached the mirror")
	}
	// A fact whose bundle never came, or naming another repository, is refused and records nothing.
	later := checkpointIn(t, repo, mine.ID, "c.txt", "later\n")
	w.fact(3, 3, nodelink.MethodCheckpointSent, nodelink.ArgsCheckpointSent{TicketID: mine.ID, Repo: "acme/web", SHA: later})
	if r := w.result(); r.GetError() == "" || !r.GetUserError() {
		t.Fatal("a CheckpointSent without its bundle was taken")
	}
	w.fact(4, 4, nodelink.MethodCheckpointSent, nodelink.ArgsCheckpointSent{TicketID: mine.ID, Repo: "acme/other", SHA: sha})
	if r := w.result(); r.GetError() == "" {
		t.Fatal("a CheckpointSent of another repository was taken")
	}
	if c, _, _ := f.Checkpoint(mine.ID); c.SHA != sha {
		t.Fatalf("record moved to %s", c.SHA)
	}
	// A transfer cut off by the link's end leaves nothing behind.
	w.blob(&athenav1.BlobChunk{TransferId: "x3", Kind: "checkpoint", TicketId: mine.ID, Data: []byte("# v2 git bundle\n")})
	eventually(t, "the half transfer is held", func() bool { return len(f.Remote("n1").link().transferFiles()) == 1 })
	files := f.Remote("n1").link().transferFiles()
	w.close()
	for _, p := range files {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s outlived its link: %v", p, err)
		}
	}
}

// transferFiles are the files of the link's transfers under way.
func (lk *nodeLink) transferFiles() []string {
	lk.mu.Lock()
	defer lk.mu.Unlock()
	var out []string
	for _, tr := range lk.transfers {
		if tr.file != nil {
			out = append(out, tr.file.Name())
		}
	}
	return out
}
