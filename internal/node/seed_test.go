package node

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/store"
)

// seedingExec is the node as the Engine's Executor, with a hook that runs before each Start: where the fleet seeds a
// moved Ticket's worktree.
type seedingExec struct {
	n       *Node
	mu      sync.Mutex
	onStart func(t core.Ticket, a core.Assignment)
}

func (x *seedingExec) Start(t core.Ticket, a core.Assignment) {
	x.mu.Lock()
	hook := x.onStart
	x.mu.Unlock()
	if hook != nil {
		hook(t, a)
	}
	x.n.Start(t, a)
}
func (x *seedingExec) Deliver(t core.Ticket, a core.Assignment)        { x.n.Deliver(t, a) }
func (x *seedingExec) Stop(t core.Ticket, a core.Assignment, r string) { x.n.Stop(t, a, r) }

// newSeedHarness is newGHHarness with the seeding hook in front of the node.
func newSeedHarness(t *testing.T, onStart func(h *ghHarness, t core.Ticket, a core.Assignment)) *ghHarness {
	t.Helper()
	root, work := fakeGitHub(t, "o/r")
	logs := &syncBuffer{}
	pol := policy.New(policy.Defaults([]string{"alice"}), listenAll{}, time.Minute)
	creds := &fakeCreds{}
	x := &seedingExec{}
	var gh *ghHarness
	h := newHarnessWith(t, func(c *Config) {
		c.Credentials = creds
		c.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	}, func(st *store.Store, n *Node, emit func(core.Event)) *core.Engine {
		x.n = n
		return core.NewEngineWith(st.Repos(), x, core.Options{Emit: emit, Policy: func() core.Policy { return pol }, Scope: listenAll{}})
	})
	gh = &ghHarness{harness: h, creds: creds, work: work, logs: logs}
	_ = root
	x.mu.Lock()
	x.onStart = func(tk core.Ticket, a core.Assignment) { onStart(gh, tk, a) }
	x.mu.Unlock()
	return gh
}

// TestSeedRestoresCheckpoint: a Ticket whose Assignment moved here builds its worktree from the seed: branch
// athena/<id> at the checkpoint's parent (an unpushed commit included), the checkpoint's work in progress back as
// uncommitted changes, the seed ref gone, and the first message carries the moved line.
func TestSeedRestoresCheckpoint(t *testing.T) {
	at := time.Date(2026, 11, 10, 8, 45, 0, 0, time.UTC)
	var parent, cp string
	h := newSeedHarness(t, func(h *ghHarness, tk core.Ticket, a core.Assignment) {
		// The other node's work: one commit on the branch it never pushed, and a.txt edited on top, uncommitted.
		w := h.work
		git(h.t, w, "checkout", "-q", "-b", branchName(tk))
		if err := os.WriteFile(filepath.Join(w, "README"), []byte("hi there\n"), 0o644); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(w, "gone.txt"), []byte("to be deleted\n"), 0o644); err != nil {
			h.t.Fatal(err)
		}
		git(h.t, w, "add", "gone.txt")
		git(h.t, w, "commit", "-qam", "committed on the other node")
		parent = git(h.t, w, "rev-parse", "HEAD")
		if err := os.WriteFile(filepath.Join(w, "a.txt"), []byte("half done\n"), 0o644); err != nil {
			h.t.Fatal(err)
		}
		git(h.t, w, "add", "a.txt")
		git(h.t, w, "rm", "-q", "gone.txt")
		tree := git(h.t, w, "write-tree")
		cp = git(h.t, w, "commit-tree", "-p", "HEAD", "-m", "checkpoint", tree)
		git(h.t, w, "update-ref", checkpointRef(tk.ID), cp)
		bundle := filepath.Join(h.t.TempDir(), "seed.bundle")
		git(h.t, w, "bundle", "create", bundle, checkpointRef(tk.ID))
		if err := h.n.Seed(Seed{TicketID: tk.ID, AssignmentID: a.ID, Bundle: bundle, SHA: cp, At: at, From: "studio", Offline: true}); err != nil {
			h.t.Fatal(err)
		}
		if _, err := os.Stat(bundle); !os.IsNotExist(err) {
			h.t.Errorf("Seed left the bundle where it was: %v", err)
		}
	})
	tk, _ := h.accept("github:issuecomment:1", "o/r", 12, "")
	wd, err := WorkingDir(h.cfg.WorkRoot, tk)
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, wd, "rev-parse", "--abbrev-ref", "HEAD"); got != branchName(tk) {
		t.Fatalf("branch = %s, want %s", got, branchName(tk))
	}
	if got := git(t, wd, "rev-parse", "HEAD"); got != parent {
		t.Fatalf("HEAD = %s, want the checkpoint's parent %s", got, parent)
	}
	if b, err := os.ReadFile(filepath.Join(wd, "a.txt")); err != nil || string(b) != "half done\n" {
		t.Fatalf("a.txt = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(wd, "gone.txt")); !os.IsNotExist(err) {
		t.Fatalf("gone.txt, which the checkpoint deleted, is in the worktree: %v", err)
	}
	if st := git(t, wd, "status", "--porcelain"); !strings.Contains(st, "?? a.txt") || !strings.Contains(st, "D gone.txt") {
		t.Fatalf("the checkpoint's work is not back as uncommitted changes: %q", st)
	}
	git(t, wd, "diff", "--cached", "--quiet") // nothing staged
	clone := CloneDir(h.cfg.StateDir, "o/r")
	if out := git(t, clone, "for-each-ref", "refs/athena/"+strings.ToLower(tk.ID)+"/seed"); out != "" {
		t.Fatalf("the seed ref is left: %s", out)
	}
	want := "This Ticket moved from studio, which went offline; the work below was restored from its last checkpoint at 2026-11-10 08:45 UTC."
	h.eventually("the first message", func() bool { return strings.Contains(h.stdin(), want) })
	if !strings.Contains(h.stdin(), recoveryNote) {
		t.Fatalf("no recovery note in the first message: %s", h.stdin())
	}
	if _, err := os.Stat(h.n.seedPath(tk.ID, ".json")); !os.IsNotExist(err) {
		t.Fatalf("the seed is kept after its first message: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.cfg.StateDir, "seeds")); len(entries) != 0 {
		t.Fatalf("seed files left: %v", entries)
	}
}

// TestSeedWithoutCheckpoint: a moved Ticket with no checkpoint on the server starts from its branch as it was pushed,
// or, never pushed, from the default branch; the moved line says which.
func TestSeedWithoutCheckpoint(t *testing.T) {
	t.Run("pushed", func(t *testing.T) {
		var pushed string
		h := newSeedHarness(t, func(h *ghHarness, tk core.Ticket, a core.Assignment) {
			w := h.work
			git(h.t, w, "checkout", "-q", "-b", branchName(tk))
			git(h.t, w, "commit", "-q", "--allow-empty", "-m", "pushed work")
			git(h.t, w, "push", "-q", "origin", branchName(tk))
			pushed = git(h.t, w, "rev-parse", "HEAD")
			if err := h.n.Seed(Seed{TicketID: tk.ID, AssignmentID: a.ID, From: "studio", Offline: true}); err != nil {
				h.t.Fatal(err)
			}
		})
		tk, _ := h.accept("github:issuecomment:1", "o/r", 12, "")
		wd, _ := WorkingDir(h.cfg.WorkRoot, tk)
		if got := git(t, wd, "rev-parse", "HEAD"); got != pushed {
			t.Fatalf("HEAD = %s, want the pushed branch %s", got, pushed)
		}
		h.eventually("the moved line", func() bool {
			return strings.Contains(h.stdin(), "This Ticket moved from studio, which went offline; no checkpoint of its work reached the server, so the work below starts from the branch "+branchName(tk)+" as it was last pushed.")
		})
	})
	t.Run("base", func(t *testing.T) {
		h := newSeedHarness(t, func(h *ghHarness, tk core.Ticket, a core.Assignment) {
			if err := h.n.Seed(Seed{TicketID: tk.ID, AssignmentID: a.ID, From: "studio"}); err != nil {
				h.t.Fatal(err)
			}
		})
		tk, _ := h.accept("github:issuecomment:1", "o/r", 12, "")
		wd, _ := WorkingDir(h.cfg.WorkRoot, tk)
		if got, want := git(t, wd, "rev-parse", "HEAD"), git(t, h.work, "rev-parse", "origin/trunk"); got != want {
			t.Fatalf("HEAD = %s, want the default branch %s", got, want)
		}
		h.eventually("the moved line", func() bool {
			return strings.Contains(h.stdin(), "This Ticket moved from studio; no checkpoint of its work reached the server and its branch was never pushed, so the work starts again from trunk.")
		})
	})
}

// A seed whose bundle does not hold its checkpoint is refused by git or by its commit check: the worktree starts as
// with none, and nothing but the seed ref is ever written to the clone.
func TestSeedBadBundle(t *testing.T) {
	h := newSeedHarness(t, func(h *ghHarness, tk core.Ticket, a core.Assignment) {
		bundle := filepath.Join(h.t.TempDir(), "seed.bundle")
		if err := os.WriteFile(bundle, []byte("# v2 git bundle\ngarbage\n"), 0o600); err != nil {
			h.t.Fatal(err)
		}
		if err := h.n.Seed(Seed{TicketID: tk.ID, AssignmentID: a.ID, Bundle: bundle, SHA: strings.Repeat("c", 40), From: "studio"}); err != nil {
			h.t.Fatal(err)
		}
	})
	tk, _ := h.accept("github:issuecomment:1", "o/r", 12, "")
	wd, _ := WorkingDir(h.cfg.WorkRoot, tk)
	if got, want := git(t, wd, "rev-parse", "HEAD"), git(t, h.work, "rev-parse", "origin/trunk"); got != want {
		t.Fatalf("HEAD = %s, want the default branch %s", got, want)
	}
	h.eventually("the moved line", func() bool { return strings.Contains(h.stdin(), "starts again from trunk") })
	// Seed refuses what is not a seed.
	for _, sd := range []Seed{
		{TicketID: "../x", AssignmentID: "a"},
		{TicketID: "T-1", AssignmentID: ""},
		{TicketID: "T-1", AssignmentID: "a", Bundle: "/etc/hosts", SHA: "not-a-sha"},
	} {
		if err := h.n.Seed(sd); err == nil {
			t.Errorf("Seed(%+v) was taken", sd)
		}
	}
}

// The moved line says why a checkpoint the server had did not come.
func TestMovedLineReasons(t *testing.T) {
	rec := seedRecord{From: "studio", Offline: true, Unsent: SeedTooBig}
	if got, want := movedLine(rec, seedFromBase, "athena/t-1", "trunk"),
		"This Ticket moved from studio, which went offline; its last checkpoint was too large to send here and its branch was never pushed, so the work starts again from trunk. Nothing done on studio is here."; got != want {
		t.Errorf("too big, base:\n%s\nwant\n%s", got, want)
	}
	rec.Unsent = SeedSendFailed
	if got, want := movedLine(rec, seedFromPushed, "athena/t-1", "trunk"),
		"This Ticket moved from studio, which went offline; its last checkpoint could not be sent here, so the work below starts from the branch athena/t-1 as it was last pushed. Work in progress after that push is not here."; got != want {
		t.Errorf("failed, pushed:\n%s\nwant\n%s", got, want)
	}
}

// The sweep drops the seeds of ended Tickets, those past seedKeep and bundles with no seed; it keeps the others.
func TestSweepSeeds(t *testing.T) {
	h := newHarness(t, nil)
	ended := h.submit("Ends", t.TempDir())
	if err := h.eng.StopTicket(ended.ID, "done"); err != nil {
		t.Fatal(err)
	}
	open := h.submit("Stays", t.TempDir())
	for _, id := range []string{ended.ID, open.ID, "T-9999"} {
		if err := h.n.Seed(Seed{TicketID: id, AssignmentID: "a", From: "studio"}); err != nil {
			t.Fatal(err)
		}
	}
	old, _ := h.n.readSeed("T-9999")
	old.Created = time.Now().Add(-seedKeep - time.Hour)
	if err := h.n.writeSeed("T-9999", old); err != nil {
		t.Fatal(err)
	}
	orphan := h.n.seedPath("T-8888", ".bundle")
	if err := os.WriteFile(orphan, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-seedBundleGrace - time.Minute)
	if err := os.Chtimes(orphan, long, long); err != nil {
		t.Fatal(err)
	}
	young := h.n.seedPath("T-7777", ".bundle") // a Seed under way: its record comes next
	if err := os.WriteFile(young, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.n.sweepSeeds()
	if _, ok := h.n.readSeed(open.ID); !ok {
		t.Error("the open Ticket's seed was swept")
	}
	for _, id := range []string{ended.ID, "T-9999"} {
		if _, ok := h.n.readSeed(id); ok {
			t.Errorf("%s's seed was kept", id)
		}
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("an orphan bundle was kept: %v", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Errorf("a young bundle was swept: %v", err)
	}
}

// A seed whose restore failed is not lost: the Ticket's next Assignment on this node (the owner's retry of the
// failed one) takes it over, restores the checkpoint and gets the moved line.
func TestSeedCarriesOverAFailedRestore(t *testing.T) {
	var fails atomic.Int32
	fails.Store(1)
	restoreHook = func() error {
		if fails.Add(-1) >= 0 {
			return errors.New("injected restore failure")
		}
		return nil
	}
	t.Cleanup(func() { restoreHook = nil }) // after the node's own cleanup
	var once sync.Once
	h := newSeedHarness(t, func(h *ghHarness, tk core.Ticket, a core.Assignment) {
		once.Do(func() { // the server seeds the moved Assignment only, never the retry on the same node
			w := h.work
			git(h.t, w, "checkout", "-q", "-b", branchName(tk))
			if err := os.WriteFile(filepath.Join(w, "a.txt"), []byte("half done\n"), 0o644); err != nil {
				h.t.Fatal(err)
			}
			git(h.t, w, "add", "a.txt")
			cp := git(h.t, w, "commit-tree", "-p", "HEAD", "-m", "checkpoint", git(h.t, w, "write-tree"))
			git(h.t, w, "update-ref", checkpointRef(tk.ID), cp)
			bundle := filepath.Join(h.t.TempDir(), "seed.bundle")
			git(h.t, w, "bundle", "create", bundle, checkpointRef(tk.ID))
			if err := h.n.Seed(Seed{TicketID: tk.ID, AssignmentID: a.ID, Bundle: bundle, SHA: cp, From: "studio", Offline: true}); err != nil {
				h.t.Fatal(err)
			}
		})
	})
	id := h.acceptOnly("github:issuecomment:1", "o/r", 12)
	h.eventually("the first launch failed", func() bool {
		tk, _ := h.ticket(id)
		return core.CanRetry(&tk)
	})
	tk, _ := h.ticket(id)
	if wd, _ := WorkingDir(h.cfg.WorkRoot, tk); func() bool { _, err := os.Stat(wd); return err == nil }() {
		t.Fatal("the half-built worktree is left")
	}
	if err := h.eng.RetryTicket(id); err != nil {
		t.Fatal(err)
	}
	tk, _ = h.running(id)
	wd, _ := WorkingDir(h.cfg.WorkRoot, tk)
	if b, err := os.ReadFile(filepath.Join(wd, "a.txt")); err != nil || string(b) != "half done\n" {
		t.Fatalf("a.txt after the retry = %q, %v", b, err)
	}
	h.eventually("the moved line", func() bool {
		return strings.Contains(h.stdin(), "This Ticket moved from studio, which went offline; the work below was restored from its last checkpoint.")
	})
}
