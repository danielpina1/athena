package node_test

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/fleet"
	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/nodelink"
	"github.com/danielpina1/athena/internal/store"
)

// Two nodes and the server's fleet in one process: each node has its own state directory under t.TempDir(), links to
// the fleet over an in-memory pipe, and runs fakeclaude; GitHub is a local bare repository.

const waitTimeout = 20 * time.Second

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pipeConn is a link.Conn over one end of a net.Pipe: each message is a 4-byte length and the bytes.
type pipeConn struct {
	c    net.Conn
	wmu  sync.Mutex
	once sync.Once
}

func (p *pipeConn) Read(ctx context.Context) ([]byte, error) {
	stop := context.AfterFunc(ctx, func() { _ = p.c.SetReadDeadline(time.Now()) })
	defer stop()
	var n [4]byte
	if _, err := io.ReadFull(p.c, n[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint32(n[:]))
	if _, err := io.ReadFull(p.c, b); err != nil {
		return nil, err
	}
	return b, nil
}

func (p *pipeConn) Write(ctx context.Context, b []byte) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = p.c.SetWriteDeadline(time.Now()) })
	defer stop()
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err := p.c.Write(buf)
	return err
}

func (p *pipeConn) Close(int, string) error {
	p.once.Do(func() { _ = p.c.Close() })
	return nil
}

// server is the control server's side: the store, the Engine and the fleet as its Executor.
type server struct {
	st  *store.Store
	eng *core.Engine
	f   *fleet.Fleet
	dir string
}

func newServer(t *testing.T, mirror bool) *server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := fleet.Config{Store: st, LocalID: fleet.LocalID, LocalName: "server", Creds: node.FakeCredsForTest(),
		OfflineAfter: 200 * time.Millisecond}
	if mirror {
		cfg.Mirror = fleet.NewMirror(fleet.CheckpointDir(dir))
	}
	f := fleet.New(cfg)
	eng := core.NewEngine(st.Repos(), f, nil, nil)
	f.SetEngine(eng)
	t.Cleanup(func() {
		f.Close()
		st.Close()
	})
	return &server{st: st, eng: eng, f: f, dir: dir}
}

// remote is a node linked to the server's fleet.
type remote struct {
	id, name string
	dir      string
	n        *node.Node
	stop     func()
}

func (s *server) startNode(t *testing.T, id, name string) *remote {
	t.Helper()
	return s.startNodeIn(t, id, name, t.TempDir())
}

// startNodeIn starts the node with its state in dir: a node started again in the same dir is the same node restarted.
func (s *server) startNodeIn(t *testing.T, id, name, dir string) *remote {
	t.Helper()
	if n, err := s.st.GetNode(id); err != nil {
		t.Fatal(err)
	} else if n == nil {
		if err := s.st.PutNode(store.Node{ID: id, Name: name, CertSerial: "01", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := nodelink.New(&nodelink.Identity{NodeID: id, Name: name}, nodelink.Options{
		StateDir: filepath.Join(dir, "link"), Version: "test", MinBackoff: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
		Dial: func(ctx context.Context) (link.Conn, error) {
			a, b := net.Pipe()
			go s.f.ServeNode(id, &pipeConn{c: b})
			return &pipeConn{c: a}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("/tmp", "athr") // Unix socket paths are short on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	n := node.New(node.Config{
		WorkRoot: filepath.Join(dir, "work"), StateDir: filepath.Join(dir, "state"), AthenaBin: "/usr/bin/true",
		SocketPath: filepath.Join(sockDir, "n.sock"), ClaudeBinary: node.FakeClaudeForTest(t), Name: name,
		ApprovalTimeout: 5 * time.Second, Credentials: c.Credentials(), Policy: c.Policy(),
		WorkerPrompt: c.WorkerPrompt, ReviewerPrompt: c.ReviewerPrompt, OnStatus: c.StatusChanged, OnCheckpoint: c.Checkpointed,
	}, c.Engine())
	ctx, cancel := context.WithCancel(context.Background())
	served, ran := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(served)
		_ = n.Serve(ctx)
	}()
	c.Attach(n)
	go func() {
		defer close(ran)
		_ = c.Run(ctx)
	}()
	r := &remote{id: id, name: name, dir: dir, n: n}
	stopped := false
	r.stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		<-ran
		n.Close()
		<-served
		c.Close()
	}
	t.Cleanup(r.stop)
	eventually(t, name+" online", func() bool {
		for _, v := range s.f.Nodes() {
			if v.ID == id {
				return v.State == fleet.StateOnline && v.Status.Slots > 0
			}
		}
		return false
	})
	return r
}

func (s *server) ticket(t *testing.T, id string) (core.Ticket, core.Stage) {
	t.Helper()
	tk, st, err := s.eng.Ticket(id)
	if err != nil {
		t.Fatal(err)
	}
	return tk, st
}

// runningOn waits until the Ticket's latest Assignment runs on the node with a session.
func (s *server) runningOn(t *testing.T, id, nodeID string) core.Assignment {
	t.Helper()
	var a core.Assignment
	eventually(t, id+" running on "+nodeID, func() bool {
		tk, _ := s.ticket(t, id)
		a = *tk.LatestAssignment()
		return a.Node == nodeID && a.State == core.AssignmentRunning && a.SessionID != ""
	})
	return a
}

func gitT(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// strand runs a Ticket on o/r on n1, edits a.txt in its worktree, uncommitted, and has another turn end after it (its
// checkpoint goes to the server).
func strand(t *testing.T, s *server, n1 *remote) core.Ticket {
	t.Helper()
	tk, err := s.eng.SubmitOwnerTask("Edit a.txt", core.TaskOptions{Repo: "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	s.runningOn(t, tk.ID, n1.id)
	wd, err := node.WorkingDir(filepath.Join(n1.dir, "work"), tk)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the worktree", func() bool {
		_, err := os.Stat(filepath.Join(wd, ".git"))
		return err == nil
	})
	if err := os.WriteFile(filepath.Join(wd, "a.txt"), []byte("half done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.eng.AddPersonInput(tk.ID, "keep going"); err != nil {
		t.Fatal(err)
	}
	return tk
}

// kill stops the node and waits for its Tickets to show it offline.
func kill(t *testing.T, s *server, n *remote, ticketID string) {
	t.Helper()
	n.stop()
	eventually(t, n.name+" offline on the Ticket", func() bool {
		_, st := s.ticket(t, ticketID)
		return st == core.Stage{Stage: "person", Action: "offline"}
	})
}

// TestRetryElsewhereRestoresWork: a turn on n1 leaves a.txt edited, uncommitted, and its checkpoint reaches the
// server; n1 dies; Retry elsewhere places the Ticket on n2, whose worktree has the edited a.txt on branch athena/<id>,
// and whose first prompt says the Ticket moved.
func TestRetryElsewhereRestoresWork(t *testing.T) {
	node.FakeGitHubForTest(t)
	stdin := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("ATHENA_FAKE_FIXTURE", node.ClaudeFixtureForTest(t, "simple-turn.jsonl"))
	t.Setenv("ATHENA_FAKE_LATER_FIXTURE", node.ClaudeFixtureForTest(t, "simple-turn.jsonl"))
	t.Setenv("ATHENA_FAKE_STDIN_LOG", stdin)
	s := newServer(t, true)
	n1 := s.startNode(t, "n1", "mini")
	tk := strand(t, s, n1)
	mirror := filepath.Join(fleet.CheckpointDir(s.dir), "o", "r.git")
	eventually(t, "the checkpoint with a.txt in the mirror", func() bool {
		c, ok, err := s.f.Checkpoint(tk.ID)
		if err != nil || !ok {
			return false
		}
		out, err := exec.Command("git", "--git-dir="+mirror, "show", c.SHA+":a.txt").Output()
		return err == nil && string(out) == "half done\n"
	})
	kill(t, s, n1, tk.ID)

	n2 := s.startNode(t, "n2", "studio")
	if err := s.eng.RetryElsewhere(tk.ID); err != nil {
		t.Fatal(err)
	}
	s.runningOn(t, tk.ID, n2.id)
	got, _ := s.ticket(t, tk.ID)
	wd, err := node.WorkingDir(filepath.Join(n2.dir, "work"), got)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(wd, "a.txt")); err != nil || string(b) != "half done\n" {
		t.Fatalf("a.txt on n2 = %q, %v", b, err)
	}
	if br := gitT(t, wd, nil, "rev-parse", "--abbrev-ref", "HEAD"); br != "athena/"+strings.ToLower(tk.ID) {
		t.Fatalf("branch on n2 = %s", br)
	}
	eventually(t, "the moved line in the first prompt", func() bool {
		b, _ := os.ReadFile(stdin)
		return strings.Contains(string(b), "This Ticket moved from mini, which went offline; the work below was restored from its last checkpoint at ")
	})
	if a := got.LatestAssignment(); len(a.Avoid) != 1 || a.Avoid[0] != n1.id {
		t.Fatalf("avoid = %v", a.Avoid)
	}
}

// TestRetryElsewhereWithoutCheckpoint: with no checkpoint on the server, the Ticket moved to n2 starts from the
// default branch (it was never pushed), and its first prompt says so.
func TestRetryElsewhereWithoutCheckpoint(t *testing.T) {
	node.FakeGitHubForTest(t)
	stdin := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("ATHENA_FAKE_FIXTURE", node.ClaudeFixtureForTest(t, "simple-turn.jsonl"))
	t.Setenv("ATHENA_FAKE_LATER_FIXTURE", node.ClaudeFixtureForTest(t, "simple-turn.jsonl"))
	t.Setenv("ATHENA_FAKE_STDIN_LOG", stdin)
	s := newServer(t, false) // the server keeps no checkpoints
	n1 := s.startNode(t, "n1", "mini")
	tk := strand(t, s, n1)
	time.Sleep(200 * time.Millisecond) // the second turn's checkpoint, refused
	kill(t, s, n1, tk.ID)

	n2 := s.startNode(t, "n2", "studio")
	if err := s.eng.RetryElsewhere(tk.ID); err != nil {
		t.Fatal(err)
	}
	s.runningOn(t, tk.ID, n2.id)
	got, _ := s.ticket(t, tk.ID)
	wd, err := node.WorkingDir(filepath.Join(n2.dir, "work"), got)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wd, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("a.txt on n2 without a checkpoint: %v", err)
	}
	eventually(t, "the moved line in the first prompt", func() bool {
		b, _ := os.ReadFile(stdin)
		return strings.Contains(string(b), "This Ticket moved from mini, which went offline; no checkpoint of its work reached the server and its branch was never pushed, so the work starts again from trunk.")
	})
}

// gitShow is the file at rev in the repository dir ("" when it has none).
func gitShow(dir, rev, file string) string {
	out, err := exec.Command("git", "-C", dir, "show", rev+":"+file).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// TestRetryElsewhereBackToKeptWorktree (Ruling R22): n1 → n2 → n1. n1 kept its worktree of the Ticket; when the Ticket
// comes back, n2's checkpoint wins there, and n1's old state is kept on a side ref the first prompt names.
func TestRetryElsewhereBackToKeptWorktree(t *testing.T) {
	node.FakeGitHubForTest(t)
	stdin := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("ATHENA_FAKE_FIXTURE", node.ClaudeFixtureForTest(t, "simple-turn.jsonl"))
	t.Setenv("ATHENA_FAKE_LATER_FIXTURE", node.ClaudeFixtureForTest(t, "simple-turn.jsonl"))
	t.Setenv("ATHENA_FAKE_STDIN_LOG", stdin)
	s := newServer(t, true)
	mirror := filepath.Join(fleet.CheckpointDir(s.dir), "o", "r.git")
	checkpointHas := func(id, body string) func() bool {
		return func() bool {
			c, ok, err := s.f.Checkpoint(id)
			return err == nil && ok && gitShow(mirror, c.SHA, "a.txt") == body
		}
	}
	n1dir := t.TempDir()
	n1 := s.startNodeIn(t, "n1", "mini", n1dir)
	tk := strand(t, s, n1) // a.txt "half done" on n1
	eventually(t, "n1's checkpoint", checkpointHas(tk.ID, "half done\n"))
	kill(t, s, n1, tk.ID)

	n2 := s.startNode(t, "n2", "studio")
	if err := s.eng.RetryElsewhere(tk.ID); err != nil {
		t.Fatal(err)
	}
	s.runningOn(t, tk.ID, n2.id)
	got, _ := s.ticket(t, tk.ID)
	wd2, _ := node.WorkingDir(filepath.Join(n2.dir, "work"), got)
	if err := os.WriteFile(filepath.Join(wd2, "a.txt"), []byte("done on n2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.eng.AddPersonInput(tk.ID, "keep going on n2"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "n2's checkpoint", checkpointHas(tk.ID, "done on n2\n"))
	kill(t, s, n2, tk.ID)

	n1 = s.startNodeIn(t, "n1", "mini", n1dir)
	if err := s.eng.RetryElsewhere(tk.ID); err != nil {
		t.Fatal(err)
	}
	s.runningOn(t, tk.ID, n1.id)
	got, _ = s.ticket(t, tk.ID)
	wd1, _ := node.WorkingDir(filepath.Join(n1dir, "work"), got)
	if b, err := os.ReadFile(filepath.Join(wd1, "a.txt")); err != nil || string(b) != "done on n2\n" {
		t.Fatalf("a.txt back on n1 = %q, %v; want n2's work", b, err)
	}
	clone := filepath.Join(n1dir, "state", "repos", "o", "r")
	side := gitT(t, clone, nil, "for-each-ref", "--format=%(refname)", "refs/athena/"+strings.ToLower(tk.ID)+"/")
	var superseded string
	for _, r := range strings.Fields(side) {
		if strings.Contains(r, "/superseded-") {
			superseded = r
		}
	}
	if superseded == "" || gitShow(clone, superseded, "a.txt") != "half done\n" {
		t.Fatalf("n1's old state is not on a side ref: refs %q", side)
	}
	eventually(t, "the moved line naming the side ref", func() bool {
		b, _ := os.ReadFile(stdin)
		return strings.Contains(string(b), "This Ticket moved from studio, which went offline; the work below was restored from its last checkpoint at ") &&
			strings.Contains(string(b), "This node's own earlier work on it is kept at "+superseded+".")
	})
}
