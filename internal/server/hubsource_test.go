package server

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/store"
	"github.com/danielpina1/athena/internal/web"
)

func placedTicket(id, repo, dir string, created time.Time) *core.Ticket {
	tk := &core.Ticket{ID: id, Title: id, Repo: repo, CreatedAt: created, UpdatedAt: created}
	if dir != "" {
		tk.Execution = &core.Execution{Directory: dir, Runtime: "claude"}
	}
	return tk
}

func putTickets(t *testing.T, st *store.Store, tks ...*core.Ticket) {
	t.Helper()
	if err := st.Tx(func(tx *store.Tx) error {
		for _, tk := range tks {
			if err := tx.PutTicket(tk); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func repoNames(rs []*athenav1.RepoView) (names, kinds []string) {
	for _, r := range rs {
		names, kinds = append(names, r.Name), append(kinds, r.Kind)
	}
	return
}

func TestSnapshotHasReposNewestFirst(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	gitDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(gitDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	putTickets(t, st,
		placedTicket("T-1", "c/d", "", t0),
		placedTicket("T-2", "", "/tmp/x", t0.Add(time.Hour)),
		placedTicket("T-3", "", gitDir, t0.Add(30*time.Minute)),
		placedTicket("T-4", "c/d", "", t0.Add(-time.Hour)), // an older use of c/d does not move it back
		placedTicket("T-5", "", "", t0.Add(2*time.Hour)),   // names nothing
	)
	src := newHubSource(&Dispatcher{st: st, gh: &gitHub{cfg: &GitHubConfig{Repos: []string{"z/last", "a/b", "c/d"}}}})
	got, err := src.Repos()
	if err != nil {
		t.Fatal(err)
	}
	names, kinds := repoNames(got)
	wantNames := []string{"/tmp/x", gitDir, "c/d", "a/b", "z/last"}
	wantKinds := []string{"dir", "git", "github", "github", "github"}
	if fmt.Sprint(names) != fmt.Sprint(wantNames) || fmt.Sprint(kinds) != fmt.Sprint(wantKinds) {
		t.Errorf("repos = %v %v, want %v %v", names, kinds, wantNames, wantKinds)
	}
	if got[0].LastUsedAt == nil || !got[0].LastUsedAt.AsTime().Equal(t0.Add(time.Hour)) || got[3].LastUsedAt != nil {
		t.Errorf("last used: used %v, never used %v", got[0].LastUsedAt, got[3].LastUsedAt)
	}
}

func TestReposAreBounded(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	var tks []*core.Ticket
	for i := range 230 {
		tks = append(tks, placedTicket(fmt.Sprintf("T-%03d", i), "", fmt.Sprintf("/work/%03d", i), t0.Add(time.Duration(i)*time.Minute)))
	}
	putTickets(t, st, tks...)
	got, err := newHubSource(&Dispatcher{st: st}).Repos()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 || got[0].Name != "/work/229" {
		t.Errorf("repos = %d, first %q; want 50, the newest first", len(got), got[0].Name)
	}
}

// A Ticket in the store reaches the dashboard's repos; a save of the notification preferences reaches its prefs.
func TestReposAndNotificationsReachTheHub(t *testing.T) {
	w := startWeb(t, newStateDir(t))
	snap, err := w.hub.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Repos) != 0 || len(snap.Notifications.GetKinds()) != len(web.NotificationKinds()) || len(snap.Notifications.GetMuted()) != 0 {
		t.Fatalf("snapshot repos %v notifications %v", snap.Repos, snap.Notifications)
	}
	sub, _ := w.hub.Subscribe(snap.Cursor, w.hub.Epoch())
	defer sub.Close()
	wait := func(what string, match func(*athenav1.Delta) bool) *athenav1.Delta {
		t.Helper()
		timeout := time.After(waitTimeout)
		for {
			select {
			case d, ok := <-sub.C():
				if !ok {
					t.Fatalf("feed closed waiting for %s", what)
				}
				if match(d) {
					return d
				}
			case <-timeout:
				t.Fatalf("no %s delta", what)
			}
		}
	}

	putTickets(t, w.d.st, placedTicket("T-0001", "", "/tmp/x", time.Now()))
	d := wait("repos", func(d *athenav1.Delta) bool { return d.GetRepos() != nil })
	if names, _ := repoNames(d.GetRepos().Repos); fmt.Sprint(names) != "[/tmp/x]" {
		t.Errorf("repos delta = %v", names)
	}

	if err := web.SaveMutedKinds(w.d.st, []string{"answer"}); err != nil {
		t.Fatal(err)
	}
	d = wait("notifications", func(d *athenav1.Delta) bool { return d.GetNotifications() != nil })
	if n := d.GetNotifications(); fmt.Sprint(n.Muted) != "[answer]" || len(n.Kinds) != len(web.NotificationKinds()) {
		t.Errorf("notifications delta = %v", n)
	}
	if snap, _ = w.hub.Snapshot(); fmt.Sprint(snap.Notifications.Muted) != "[answer]" || len(snap.Repos) != 1 {
		t.Errorf("snapshot after: repos %v notifications %v", snap.Repos, snap.Notifications)
	}
}
