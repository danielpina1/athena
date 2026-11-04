package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/store"
)

// mkdirs makes each path under base; a path ending in ".git" is made a directory, "/.git=file" a .git file.
func mkdirs(t *testing.T, base string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if dir, ok := strings.CutSuffix(p, "/.git=file"); ok {
			if err := os.MkdirAll(filepath.Join(base, dir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(base, dir, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Join(base, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLocalRepoRootsDefault(t *testing.T) {
	home := t.TempDir()
	mkdirs(t, home, "code", "Projects", "Desktop", "Music")
	if err := os.WriteFile(filepath.Join(home, "src"), nil, 0o644); err != nil { // a file, not a folder
		t.Fatal(err)
	}
	got := FileConfig{}.localRepoRoots(home)
	want := []string{filepath.Join(home, "code"), filepath.Join(home, "Projects"), filepath.Join(home, "Desktop")}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("default roots = %v, want %v (the existing default folders, in order)", got, want)
	}
	if got := (FileConfig{}).localRepoRoots(""); len(got) != 0 {
		t.Errorf("without a home directory: %v", got)
	}
}

func TestLocalRepoRootsConfigured(t *testing.T) {
	dir := newStateDir(t)
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// An explicit empty list turns discovery off, and stays empty through a save.
	write(`{"localRepoRoots": []}`)
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.LocalRepoRoots == nil || len(c.localRepoRoots("/home/x")) != 0 {
		t.Fatalf("empty list: %v -> %v", c.LocalRepoRoots, c.localRepoRoots("/home/x"))
	}
	if newLocalRepos(c.localRepoRoots("/home/x"), nil, nil) != nil {
		t.Error("no roots must mean no finder")
	}
	if err := SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	if c, err = LoadConfig(dir); err != nil || c.LocalRepoRoots == nil || len(*c.LocalRepoRoots) != 0 {
		t.Fatalf("after a save: %v, %v", c.LocalRepoRoots, err)
	}

	// Left out, it is not written back: the defaults stay the defaults.
	write(`{}`)
	if c, err = LoadConfig(dir); err != nil || c.LocalRepoRoots != nil {
		t.Fatalf("left out: %v, %v", c.LocalRepoRoots, err)
	}

	// "~/" expands, an absolute path is cleaned, a missing folder is kept (a scan passes over it).
	write(`{"localRepoRoots": ["~/work", "/srv/git/", "/nope"]}`)
	if c, err = LoadConfig(dir); err != nil {
		t.Fatal(err)
	}
	if got := c.localRepoRoots("/home/x"); fmt.Sprint(got) != "[/home/x/work /srv/git /nope]" {
		t.Errorf("roots = %v", got)
	}
	if got := c.localRepoRoots(""); fmt.Sprint(got) != "[/srv/git /nope]" {
		t.Errorf("roots without a home = %v", got)
	}

	for body, want := range map[string]string{
		`{"localRepoRoots": ["code"]}`:      `"code" must be an absolute path or start with ~/`,
		`{"localRepoRoots": ["~bob/code"]}`: `"~bob/code" must be an absolute path`,
		`{"localRepoRoots": ["/a", "/a/"]}`: `"/a/" is listed twice`,
		`{"localRepoRoots": "/a"}`:          "cannot unmarshal",
		`{"localRepoRoots": [""]}`:          `"" must be an absolute path`,
	} {
		write(body)
		if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", body, err, want)
		}
	}
}

func TestScanLocalRepos(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	mkdirs(t, root,
		"top/.git", "top/inner/.git", // a repository, and one inside it that is not looked for
		"group/deep/.git",     // a grandchild
		"group/deeper/x/.git", // three levels down: too deep
		"group/wt/.git=file",  // a worktree's .git file
		"plain/notes",         // no repository
		".hidden/h/.git", "node_modules/n/.git", "vendor/v/.git", "group/.cache/.git",
		"state/s/.git", "group/state2/.git", // skipped paths
	)
	mkdirs(t, outside, ".git", "in/.git")
	for name, target := range map[string]string{"link": outside, "group/link2": filepath.Join(outside, "in")} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	skip := map[string]bool{filepath.Join(root, "state"): true, filepath.Join(root, "group", "state2"): true}

	got := scanLocalRepos(context.Background(), []string{root}, skip, 100, time.Minute)
	want := []string{filepath.Join(root, "group", "deep"), filepath.Join(root, "group", "wt"), filepath.Join(root, "top")}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("found %v\nwant  %v", got, want)
	}

	// Overlapping roots list a repository once; the depth counts from each root.
	got = scanLocalRepos(context.Background(), []string{root, filepath.Join(root, "group")}, skip, 100, time.Minute)
	want = []string{filepath.Join(root, "group", "deep"), filepath.Join(root, "group", "deeper", "x"),
		filepath.Join(root, "group", "wt"), filepath.Join(root, "top")}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("overlapping roots: found %v\nwant  %v", got, want)
	}

	// A root that is a repository is listed itself.
	if got := scanLocalRepos(context.Background(), []string{filepath.Join(root, "top")}, nil, 100, time.Minute); fmt.Sprint(got) != fmt.Sprint([]string{filepath.Join(root, "top")}) {
		t.Errorf("a repository root: %v", got)
	}

	// The cap, the time budget and the context bound a scan.
	many := t.TempDir()
	for i := range 10 {
		mkdirs(t, many, fmt.Sprintf("r%02d/.git", i))
	}
	if got := scanLocalRepos(context.Background(), []string{many}, nil, 3, time.Minute); len(got) != 3 {
		t.Errorf("cap 3: %v", got)
	}
	if got := scanLocalRepos(context.Background(), []string{many}, nil, 100, 0); len(got) != 0 {
		t.Errorf("no time: %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := scanLocalRepos(ctx, []string{many}, nil, 100, time.Minute); len(got) != 0 {
		t.Errorf("cancelled: %v", got)
	}
	// A missing root is passed over.
	if got := scanLocalRepos(context.Background(), []string{filepath.Join(many, "nope"), many}, nil, 100, time.Minute); len(got) != 10 {
		t.Errorf("with a missing root: %d", len(got))
	}
}

// A scan whose result differs calls changed; one that finds the same does not.
func TestLocalReposRunReportsChanges(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a/.git")
	l := newLocalRepos([]string{root}, nil, nil)
	l.every = 10 * time.Millisecond
	changed := make(chan []string, 10)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.run(ctx, func() { changed <- l.list() })
	}()
	defer func() { cancel(); <-done }()
	wait := func() []string {
		t.Helper()
		select {
		case got := <-changed:
			return got
		case <-time.After(waitTimeout):
			t.Fatal("no change reported")
			return nil
		}
	}
	if got := wait(); fmt.Sprint(got) != fmt.Sprint([]string{filepath.Join(root, "a")}) {
		t.Fatalf("first scan: %v", got)
	}
	time.Sleep(50 * time.Millisecond) // a few unchanged scans
	select {
	case got := <-changed:
		t.Fatalf("an unchanged scan reported %v", got)
	default:
	}
	mkdirs(t, root, "b/.git")
	if got := wait(); len(got) != 2 {
		t.Fatalf("after a new repository: %v", got)
	}
}

func TestReposAppendLocalRepositories(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	putTickets(t, st,
		placedTicket("T-1", "", "/src/used", t0),
		placedTicket("T-2", "o/used", "", t0.Add(time.Hour)),
	)
	local := &localRepos{found: []string{"/src/a", "/src/used", "/src/z"}}
	d := &Dispatcher{st: st, gh: &gitHub{cfg: &GitHubConfig{Repos: []string{"o/used", "o/new"}}}, local: local}
	got, err := newHubSource(d).Repos()
	if err != nil {
		t.Fatal(err)
	}
	names, kinds := repoNames(got)
	// Used first (newest first), then the configured GitHub ones, then the local ones not listed yet, by name.
	if want := "[o/used /src/used o/new /src/a /src/z]"; fmt.Sprint(names) != want {
		t.Errorf("names = %v, want %v", names, want)
	}
	if want := "[github dir github git git]"; fmt.Sprint(kinds) != want {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}
	if got[3].LastUsedAt != nil || got[4].LastUsedAt != nil {
		t.Error("a found repository was never used")
	}

	// Local repositories have their own cap: they never push out a used place or a configured repository.
	var tks []*core.Ticket
	for i := range 60 {
		tks = append(tks, placedTicket(fmt.Sprintf("U-%03d", i), "", fmt.Sprintf("/work/%03d", i), t0.Add(time.Duration(i+2)*time.Hour)))
	}
	putTickets(t, st, tks...)
	local.found = nil
	for i := range maxLocalRepos + 20 {
		local.found = append(local.found, fmt.Sprintf("/local/%03d", i))
	}
	got, err = newHubSource(d).Repos()
	if err != nil {
		t.Fatal(err)
	}
	var used, gh, found int
	for _, r := range got {
		switch {
		case r.LastUsedAt != nil:
			used++
		case r.Kind == "github":
			gh++
		case strings.HasPrefix(r.Name, "/local/"):
			found++
		}
	}
	if len(got) != maxRepos+maxLocalRepos || found != maxLocalRepos || used+gh != maxRepos || got[maxRepos].Name != "/local/000" {
		t.Errorf("%d repos: %d used, %d github, %d found, first found %q", len(got), used, gh, found, got[maxRepos].Name)
	}
}

// Local repositories found after the start reach a connected dashboard, through the hub's change path.
func TestLocalReposReachTheDashboard(t *testing.T) {
	dir := newStateDir(t)
	roots := t.TempDir()
	mkdirs(t, roots, "proj/.git")
	cfg := FileConfig{LocalRepoRoots: &[]string{roots}}
	if err := SaveConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	w := startWeb(t, dir)
	want := filepath.Join(roots, "proj")
	has := func(rs []*athenav1.RepoView) bool {
		for _, r := range rs {
			if r.Name == want && r.Kind == "git" && r.LastUsedAt == nil {
				return true
			}
		}
		return false
	}
	snap, err := w.hub.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !has(snap.Repos) {
		sub, _ := w.hub.Subscribe(snap.Cursor, w.hub.Epoch())
		defer sub.Close()
		timeout := time.After(waitTimeout)
		for found := false; !found; {
			select {
			case d, ok := <-sub.C():
				if !ok {
					t.Fatal("feed closed")
				}
				found = has(d.GetRepos().GetRepos())
			case <-timeout:
				t.Fatalf("no repos delta naming %s", want)
			}
		}
	}
}
