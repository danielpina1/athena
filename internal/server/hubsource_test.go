package server

import (
	"context"
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

// probedRepos is d's Repos once the folders a first read asked about were probed, as the background pass does.
func probedRepos(t *testing.T, d *Dispatcher) []*athenav1.RepoView {
	t.Helper()
	if d.facts == nil {
		d.facts = newPlaceFacts()
	}
	src := newHubSource(d)
	if _, err := src.Repos(); err != nil {
		t.Fatal(err)
	}
	d.facts.refresh(false)
	got, err := src.Repos()
	if err != nil {
		t.Fatal(err)
	}
	return got
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
	got := probedRepos(t, &Dispatcher{st: st, gh: &gitHub{cfg: &GitHubConfig{Repos: []string{"z/last", "a/b", "c/d"}}}})
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

// Repos runs on the hub's loop: it reads what the background pass found about the placement folders (a .git entry,
// the path through links), never the filesystem, so a folder on a dead mount cannot stall every dashboard.
func TestReposReadCachedFolderFacts(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	gitDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(gitDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	putTickets(t, st, placedTicket("T-1", "", gitDir, time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)))
	facts := newPlaceFacts()
	var probed []string
	probe := facts.probe
	facts.probe = func(dir string) placeFact {
		probed = append(probed, dir)
		return probe(dir)
	}
	src := newHubSource(&Dispatcher{st: st, facts: facts})
	kinds := func() string {
		t.Helper()
		got, err := src.Repos()
		if err != nil {
			t.Fatal(err)
		}
		_, k := repoNames(got)
		return fmt.Sprint(k)
	}

	// Not probed yet: a plain folder until the pass the read asked for.
	if k := kinds(); k != "[dir]" || len(probed) != 0 {
		t.Fatalf("before the pass: kinds %s, probed %v", k, probed)
	}
	if c := facts.refresh(false); fmt.Sprint(c) != fmt.Sprint([]string{gitDir}) {
		t.Errorf("the pass found a repository but reported the change %v", c)
	}
	if k := kinds(); k != "[git]" || fmt.Sprint(probed) != fmt.Sprint([]string{gitDir}) {
		t.Fatalf("after the pass: kinds %s, probed %v", k, probed)
	}
	// Reads touch nothing; a periodic pass that finds the same reports no change.
	probed = nil
	kinds()
	if len(probed) != 0 {
		t.Errorf("a read probed %v", probed)
	}
	if c := facts.refresh(true); len(c) != 0 {
		t.Errorf("an unchanged pass reported a change %v", c)
	}
	// The summaries' branch reads the same facts, never the filesystem.
	probed = nil
	if !src.GitFolder(gitDir) || len(probed) != 0 {
		t.Errorf("GitFolder: %v, probed %v", src.GitFolder(gitDir), probed)
	}
}

// A folder nobody read over whole periods keeps its fact: a dashboard's Snapshot after a quiet spell lists it as the
// repository it is. Forgetting it made that Snapshot list it a plain folder, and no Delta ever corrected it, since the
// hub's last published list still held the fact.
func TestReposKeepFolderFactsOverQuietPeriods(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	gitDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(gitDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	putTickets(t, st, placedTicket("T-1", "", gitDir, time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)))
	facts := newPlaceFacts()
	src := newHubSource(&Dispatcher{st: st, facts: facts})
	if _, err := src.Repos(); err != nil {
		t.Fatal(err)
	}
	facts.refresh(false)
	// Two periodic passes with no read between them, as when no dashboard is open for a while.
	for range 2 {
		if c := facts.refresh(true); len(c) != 0 {
			t.Fatalf("a quiet pass reported a change %v", c)
		}
	}
	got, err := src.Repos()
	if err != nil {
		t.Fatal(err)
	}
	if names, kinds := repoNames(got); fmt.Sprint(kinds) != "[git]" {
		t.Errorf("after a quiet spell: %v %v", names, kinds)
	}
	if !src.GitFolder(gitDir) {
		t.Error("after a quiet spell the folder has no .git entry")
	}
}

// A placement folder the hub has not seen is probed off the loop at once, and a fact that differs from the guess
// reaches the dashboards through the change callback.
func TestPlaceFactsRunProbesANewFolder(t *testing.T) {
	gitDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(gitDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	facts := newPlaceFacts()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := make(chan []string, 1)
	go facts.run(ctx, time.Hour, func(dirs []string) { changed <- dirs })
	if facts.lookup(gitDir).git {
		t.Fatal("a folder not probed yet is a repository")
	}
	select {
	case dirs := <-changed:
		if fmt.Sprint(dirs) != fmt.Sprint([]string{gitDir}) {
			t.Errorf("changed folders %v", dirs)
		}
	case <-time.After(waitTimeout):
		t.Fatal("no change reported")
	}
	if f := facts.lookup(gitDir); !f.git || f.resolved != resolvePath(gitDir) {
		t.Errorf("fact = %+v", f)
	}
}

func TestRepoViewCarriesBrake(t *testing.T) {
	st := openStore(t)
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	var tks []*core.Ticket
	for i := range 3 {
		tk := placedTicket(fmt.Sprintf("T-%d", i), "acme/web", "", t0.Add(time.Duration(i)*time.Minute))
		tk.Attention = &core.Attention{Question: "which branch?"}
		tks = append(tks, tk)
	}
	putTickets(t, st, tks...)
	if err := st.Tx(func(tx *store.Tx) error {
		return tx.PutBrake(core.Brake{TriggerID: "trg-1", Repo: "acme/web", Thread: "github:acme/web#1", At: t0})
	}); err != nil {
		t.Fatal(err)
	}
	pc, err := newPolicyCache(st, nil, allScope{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got := probedRepos(t, &Dispatcher{st: st, policy: pc})
	if len(got) != 1 {
		t.Fatalf("repos = %v", got)
	}
	if r := got[0]; r.Owed != 3 || r.Ceiling != 3 || r.Braked != 1 || r.Mode != "live" {
		t.Errorf("repo view = %v, want owed 3, ceiling 3, braked 1, live", r)
	}
}
