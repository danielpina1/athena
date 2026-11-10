package node

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		git(h.t, w, "commit", "-qam", "committed on the other node")
		parent = git(h.t, w, "rev-parse", "HEAD")
		if err := os.WriteFile(filepath.Join(w, "a.txt"), []byte("half done\n"), 0o644); err != nil {
			h.t.Fatal(err)
		}
		git(h.t, w, "add", "a.txt")
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
	if st := git(t, wd, "status", "--porcelain", "--untracked-files=no"); !strings.Contains(st, "a.txt") {
		t.Fatalf("a.txt is not an uncommitted change: %q", st)
	}
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
