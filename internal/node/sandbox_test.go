package node

import (
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/danielpina1/athena/internal/core"
)

type sandboxBlock struct {
	Enabled                  bool `json:"enabled"`
	FailIfUnavailable        bool `json:"failIfUnavailable"`
	AutoAllowBashIfSandboxed bool `json:"autoAllowBashIfSandboxed"`
	AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
	Weaker                   bool `json:"enableWeakerNetworkIsolation"`
	Filesystem               struct {
		AllowWrite []string `json:"allowWrite"`
		DenyWrite  []string `json:"denyWrite"`
		DenyRead   []string `json:"denyRead"`
	} `json:"filesystem"`
	Network struct {
		AllowedDomains      []string `json:"allowedDomains"`
		StrictAllowlist     bool     `json:"strictAllowlist"`
		AllowUnixSockets    []string `json:"allowUnixSockets"`
		AllowAllUnixSockets bool     `json:"allowAllUnixSockets"`
	} `json:"network"`
}

type sessionSettings struct {
	Env     map[string]string `json:"env"`
	Sandbox *sandboxBlock     `json:"sandbox"`
}

func readSessionSettings(t *testing.T, dir string) sessionSettings {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ".athena", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s sessionSettings
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("settings.json: %v\n%s", err, raw)
	}
	return s
}

// A worker in a worktree of a repository gets the sandbox block: its directory and the repository's git directory
// writable, the git config and hooks and Athena's own files not, GitHub and the configured hosts reachable, the node
// socket allowed, and no way to run a command outside the sandbox.
func TestWorkerSettingsCarrySandbox(t *testing.T) {
	repo := newRepo(t)
	h := newHarness(t, func(c *Config) {
		c.Sandbox = &SandboxConfig{Required: true, AllowedDomains: []string{"proxy.golang.org"}, AllowWrite: []string{"/cache/go"},
			DenyRead: []string{"/keys/app.pem"}, CAFile: "/etc/ca.pem", WeakerNetworkIsolation: true}
	})
	tk := h.submit("Sandboxed change", repo)
	tk, _ = h.running(tk.ID)
	dir := TicketDir(h.cfg.WorkRoot, tk)
	s := readSessionSettings(t, dir)
	sb := s.Sandbox
	if sb == nil {
		t.Fatal("settings.json has no sandbox block")
	}
	if !sb.Enabled || !sb.FailIfUnavailable || sb.AutoAllowBashIfSandboxed || sb.AllowUnsandboxedCommands || !sb.Weaker {
		t.Errorf("switches = %+v", sb)
	}
	common := filepath.Join(repo, ".git")
	if real, err := filepath.EvalSymlinks(common); err == nil {
		common = real // place resolves the repository's path
	}
	for _, p := range []string{dir, common, "/cache/go"} {
		if !slices.Contains(sb.Filesystem.AllowWrite, p) {
			t.Errorf("allowWrite %v lacks %s", sb.Filesystem.AllowWrite, p)
		}
	}
	for _, p := range []string{
		filepath.Join(common, "config"), filepath.Join(common, "config.worktree"), filepath.Join(common, "hooks"),
		filepath.Join(common, "worktrees", filepath.Base(dir), "config.worktree"),
		filepath.Join(dir, ".athena", "settings.json"), filepath.Join(dir, ".athena", "mcp.json"), filepath.Join(dir, ".athena", ".gitignore"),
	} {
		if !slices.Contains(sb.Filesystem.DenyWrite, p) {
			t.Errorf("denyWrite %v lacks %s", sb.Filesystem.DenyWrite, p)
		}
	}
	if !slices.Equal(sb.Filesystem.DenyRead, []string{"/keys/app.pem"}) {
		t.Errorf("denyRead = %v", sb.Filesystem.DenyRead)
	}
	if want := append(slices.Clone(GitHubDomains), "proxy.golang.org"); !slices.Equal(sb.Network.AllowedDomains, want) || !sb.Network.StrictAllowlist {
		t.Errorf("network = %+v, want domains %v and a strict allowlist", sb.Network, want)
	}
	if !slices.Equal(sb.Network.AllowUnixSockets, []string{h.cfg.SocketPath}) {
		t.Errorf("allowUnixSockets = %v, want the node socket %s", sb.Network.AllowUnixSockets, h.cfg.SocketPath)
	}
	if sb.Network.AllowAllUnixSockets != (runtime.GOOS == "linux") {
		t.Errorf("allowAllUnixSockets = %v on %s", sb.Network.AllowAllUnixSockets, runtime.GOOS)
	}
	for _, name := range []string{"SSL_CERT_FILE", "GIT_SSL_CAINFO", "CURL_CA_BUNDLE"} {
		if s.Env[name] != "/etc/ca.pem" {
			t.Errorf("env %s = %q, want the CA file", name, s.Env[name])
		}
	}
	if s.Env["ATHENA_SOCKET"] != h.cfg.SocketPath {
		t.Errorf("env ATHENA_SOCKET = %q", s.Env["ATHENA_SOCKET"])
	}
}

// Without Config.Sandbox the settings have no sandbox block (and a plain Ticket directory needs no git paths).
func TestWorkerSettingsWithoutSandbox(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Plain task", "")
	tk, _ = h.running(tk.ID)
	if s := readSessionSettings(t, TicketDir(h.cfg.WorkRoot, tk)); s.Sandbox != nil {
		t.Fatalf("sandbox block without Config.Sandbox: %+v", s.Sandbox)
	}

	h = newHarness(t, func(c *Config) { c.Sandbox = &SandboxConfig{} })
	tk = h.submit("Plain sandboxed task", "")
	tk, _ = h.running(tk.ID)
	dir := TicketDir(h.cfg.WorkRoot, tk)
	sb := readSessionSettings(t, dir).Sandbox
	if sb == nil || !slices.Equal(sb.Filesystem.AllowWrite, []string{dir}) || len(sb.Filesystem.DenyWrite) != 3 ||
		sb.FailIfUnavailable || sb.Weaker {
		t.Fatalf("sandbox of a plain directory = %+v", sb)
	}
}

func TestGitDirs(t *testing.T) {
	repo := newRepo(t)
	if g, c := gitDirs(repo); g != filepath.Join(repo, ".git") || c != g {
		t.Errorf("repository: %q %q", g, c)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", "-q", "-b", "b1", wt)
	g, c := gitDirs(wt)
	realRepo, _ := filepath.EvalSymlinks(repo)
	if want := filepath.Join(realRepo, ".git"); c != want && c != filepath.Join(repo, ".git") {
		t.Errorf("worktree common = %q, want %s", c, want)
	}
	if filepath.Dir(g) != filepath.Join(c, "worktrees") {
		t.Errorf("worktree gitdir = %q, want under %s/worktrees", g, c)
	}
	// A relative gitdir, as `git worktree add --relative-paths` writes it.
	rel := filepath.Join(t.TempDir(), "rel")
	if err := os.MkdirAll(filepath.Join(rel, "gd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, ".git"), []byte("gitdir: gd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, "gd", "commondir"), []byte("../common\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if g, c := gitDirs(rel); g != filepath.Join(rel, "gd") || c != filepath.Join(rel, "common") {
		t.Errorf("relative: %q %q", g, c)
	}
	if g, c := gitDirs(t.TempDir()); g != "" || c != "" {
		t.Errorf("no git: %q %q", g, c)
	}
}

// The kept prompt lives in the state directory, outside every path a sandboxed session may write.
func TestKeptPromptIsOutsideTheSandboxWriteSet(t *testing.T) {
	base := t.TempDir()
	n := New(Config{StateDir: filepath.Join(base, "state"), SocketPath: filepath.Join(base, "n.sock"), Sandbox: &SandboxConfig{}}, nil)
	sb, _ := n.sandboxFor(filepath.Join(base, "work", "t-1"))
	if sb == nil {
		t.Fatal("no sandbox")
	}
	kept := PromptPath(n.cfg.StateDir, "T-0001")
	for _, w := range sb.AllowWrite {
		if kept == w || strings.HasPrefix(kept, w+string(filepath.Separator)) {
			t.Fatalf("%s is inside the write set %s", kept, w)
		}
	}
}

func TestEndedTicketDropsItsKeptPrompt(t *testing.T) {
	n := New(Config{StateDir: t.TempDir()}, nil)
	n.sessionPrompt("T-0001", false)
	path := PromptPath(n.cfg.StateDir, "T-0001")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	n.dropPrompt("T-0001")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("kept prompt still there: %v", err)
	}
	n.dropPrompt("T-0001") // already gone: no error
}

// The node's own git (gitIn: worktree add, status, rev-list, worktree remove) runs no hook and no fsmonitor of the
// repository's, whose config or hooks a worker may have reached.
func TestNodeGitRunsNoHookOrFsmonitor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks")
	}
	repo := newRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	script := "#!/bin/sh\necho \"$0\" >> " + marker + "\n"
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	monitor := filepath.Join(t.TempDir(), "fsmonitor.sh")
	if err := os.WriteFile(monitor, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "core.fsmonitor", monitor)
	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := gitIn(repo, nil, "worktree", "add", "-q", "-b", "t1", wt); err != nil {
		t.Fatal(err)
	}
	if _, err := gitIn(wt, nil, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("node git ran the repository's hook or fsmonitor:\n%s", b)
	}
}

// The node's git in a worktree finds its directories where Athena made them: a worker that repoints the worktree's
// commondir, or its .git file, at a git directory of its own whose config names a filter driver runs nothing when the
// node reads the worktree's status.
func TestNodeGitIgnoresRepointedGitDirs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell filter")
	}
	for _, how := range []string{"commondir", ".git file"} {
		t.Run(how, func(t *testing.T) {
			repo := newRepo(t)
			if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.txt filter=evil\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, repo, "add", ".gitattributes", "a.txt")
			git(t, repo, "commit", "-q", "-m", "attrs")
			wt := filepath.Join(t.TempDir(), "wt-t-1")
			if err := ensureWorktree(repo, wt, "athena/t-1", "HEAD", nil); err != nil {
				t.Fatal(err)
			}
			admin := worktreeAdmin(filepath.Join(repo, ".git"), wt)
			// The worker's git directory: a copy of the repository's, whose config defines the filter.
			marker := filepath.Join(t.TempDir(), "ran")
			filter := filepath.Join(t.TempDir(), "evil.sh")
			if err := os.WriteFile(filter, []byte("#!/bin/sh\necho ran >> "+marker+"\ncat\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			fake := filepath.Join(t.TempDir(), "common")
			if out, err := exec.Command("cp", "-R", filepath.Join(repo, ".git"), fake).CombinedOutput(); err != nil {
				t.Fatalf("cp: %v %s", err, out)
			}
			git(t, fake, "--git-dir="+fake, "config", "filter.evil.clean", filter)
			switch how {
			case "commondir":
				if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte(fake+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case ".git file":
				fakeAdmin := filepath.Join(fake, "worktrees", filepath.Base(wt))
				if err := os.WriteFile(filepath.Join(fakeAdmin, "commondir"), []byte(fake+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+fakeAdmin+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// The same size: git must hash the file to tell, through the filter.
			if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("b\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cleanupWorktree(placement{workDir: wt, repo: repo}, core.Ticket{ID: "T-1"}, "", slog.New(slog.DiscardHandler))
			if b, err := os.ReadFile(marker); err == nil {
				t.Fatalf("the node's git ran the worker's filter driver:\n%s", b)
			}
			if _, err := os.Stat(wt); err != nil {
				t.Fatalf("the changed worktree was removed: %v", err)
			}
		})
	}
}

// A worktree an earlier node made under git's <name><number> (its own name was taken) is found by its gitdir file, for
// the file listing and the cleanup; one whose gitdir names another directory, or outside the work root, is not.
func TestLegacyWorktreeGitDir(t *testing.T) {
	repo := newRepo(t)
	common := filepath.Join(repo, ".git")
	workRoot := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	// Another worktree of the same name takes worktrees/fix-t-1; the Ticket's gets worktrees/fix-t-11, as git names it.
	other := filepath.Join(t.TempDir(), "fix-t-1")
	git(t, repo, "worktree", "add", "-q", "-b", "other", other)
	wt := filepath.Join(workRoot, "fix-t-1")
	git(t, repo, "worktree", "add", "-q", "-b", "athena/t-1", wt)
	legacy := filepath.Join(common, "worktrees", "fix-t-11")
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("git did not name the git dir fix-t-11: %v", err)
	}
	got, err := worktreeGitDir(t.Context(), repo, wt, workRoot, log)
	if err != nil || got != legacy {
		t.Fatalf("worktreeGitDir = %q, %v; want %s", got, err, legacy)
	}
	// Outside the work root, or with a gitdir naming another directory: refused.
	if _, err := findWorktreeAdmin(common, wt, t.TempDir(), log); err == nil || !strings.Contains(err.Error(), "unregistered or renamed git dir") {
		t.Fatalf("outside the work root: %v", err)
	}
	gd := filepath.Join(legacy, "gitdir")
	keep, err := os.ReadFile(gd)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gd, []byte(filepath.Join(other, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := findWorktreeAdmin(common, wt, workRoot, log); err == nil {
		t.Fatal("a legacy git dir naming another worktree was used")
	}
	cleanupWorktree(placement{workDir: wt, repo: repo, common: common}, core.Ticket{ID: "T-1"}, workRoot, log)
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("a worktree whose git dir is not found was removed: %v", err)
	}
	// Found again, the clean worktree's cleanup removes it.
	if err := os.WriteFile(gd, keep, 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupWorktree(placement{workDir: wt, repo: repo, common: common}, core.Ticket{ID: "T-1"}, workRoot, log)
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("the legacy worktree was not cleaned up: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("the other worktree was touched: %v", err)
	}
}

// A plain directory's Ticket stays plain: a worker that makes the directory a repository, with a smudge filter, does
// not get its next launch to make a worktree of it (whose checkout would run the filter on the host).
func TestPlainDirPlacementIsRecorded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell filter")
	}
	h := newHarness(t, nil)
	dir := t.TempDir()
	tk := h.submit("Work in a folder", dir)
	tk, a := h.running(tk.ID)
	rec := filepath.Join(h.cfg.StateDir, placementsDir, tk.ID+".json")
	if _, err := os.Stat(rec); err != nil {
		t.Fatalf("no placement record: %v", err)
	}
	// The worker: git init, an attributed file committed, and a smudge filter in the new repository's config.
	marker := filepath.Join(t.TempDir(), "ran")
	filter := filepath.Join(t.TempDir(), "evil.sh")
	if err := os.WriteFile(filter, []byte("#!/bin/sh\necho ran >> "+marker+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q", "-b", "main")
	for name, body := range map[string]string{".gitattributes": "*.txt filter=evil\n", "a.txt": "a\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", ".gitattributes", "a.txt")
	git(t, dir, "commit", "-q", "-m", "x")
	git(t, dir, "config", "filter.evil.smudge", filter)
	p, err := h.n.place(tk)
	if err != nil {
		t.Fatal(err)
	}
	if p.repo != "" || p.workDir != p.dirKey || p.dirKey == "" {
		t.Fatalf("placement = %+v, want the plain directory it was placed as", p)
	}
	// The next launch's preparation makes no worktree and runs nothing.
	if err := h.n.prepare(h.n.newSession(tk, a, p)); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("the worker's smudge filter ran on the host:\n%s", b)
	}
	if _, err := os.Stat(TicketDir(h.cfg.WorkRoot, tk)); !os.IsNotExist(err) {
		t.Fatalf("a worktree was made: %v", err)
	}
}

// Without a record (a Ticket placed before records existed), a Ticket that already ran and is now found in a
// repository is placed there only when its worktree exists: the probe's plain directory, made a repository by its
// worker, is refused and runs nothing; a repository Ticket with its worktree is placed and recorded; a new Ticket is
// derived and recorded as ever.
func TestUnrecordedPlacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell filter")
	}
	h := newHarness(t, nil)
	record := func(id string) string { return filepath.Join(h.cfg.StateDir, placementsDir, id+".json") }

	// The probe: a plain directory, a worker's git init and smudge filter, the record gone.
	dir := t.TempDir()
	tk := h.submit("Work in a folder", dir)
	tk, _ = h.running(tk.ID)
	marker := filepath.Join(t.TempDir(), "ran")
	filter := filepath.Join(t.TempDir(), "evil.sh")
	if err := os.WriteFile(filter, []byte("#!/bin/sh\necho ran >> "+marker+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q", "-b", "main")
	for name, body := range map[string]string{".gitattributes": "*.txt filter=evil\n", "a.txt": "a\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", ".gitattributes", "a.txt")
	git(t, dir, "commit", "-q", "-m", "x")
	git(t, dir, "config", "filter.evil.smudge", filter)
	if err := os.Remove(record(tk.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.n.place(tk); err == nil || err.Error() != "the directory became a git repository after the Ticket started, or its worktree was removed; resubmit" {
		t.Fatalf("place = %v, want the refusal", err)
	}
	if _, err := os.Stat(record(tk.ID)); !os.IsNotExist(err) {
		t.Fatalf("a refused placement was recorded: %v", err)
	}
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("the smudge filter ran:\n%s", b)
	}
	if _, err := os.Stat(TicketDir(h.cfg.WorkRoot, tk)); !os.IsNotExist(err) {
		t.Fatalf("a worktree was made: %v", err)
	}

	// A repository Ticket from before records: its worktree exists, so it is placed there and recorded.
	repo := newRepo(t)
	rt2 := h.submit("Work in a repo", repo)
	rt2, _ = h.running(rt2.ID)
	if err := os.Remove(record(rt2.ID)); err != nil {
		t.Fatal(err)
	}
	p, err := h.n.place(rt2)
	if err != nil || p.repo == "" || !sameFile(p.repo, repo) || p.workDir != TicketDir(h.cfg.WorkRoot, rt2) {
		t.Fatalf("place = %+v, %v; want the repository's worktree", p, err)
	}
	if _, err := os.Stat(record(rt2.ID)); err != nil {
		t.Fatalf("not recorded: %v", err)
	}

	// A new Ticket (one Assignment, no session yet): derived and recorded.
	fresh := core.Ticket{ID: "T-0099", Title: "Fresh", Execution: &core.Execution{Directory: repo},
		Assignments: []core.Assignment{{ID: "a1"}}}
	if p, err := h.n.place(fresh); err != nil || p.repo == "" {
		t.Fatalf("new ticket: %+v, %v", p, err)
	}
	if _, err := os.Stat(record(fresh.ID)); err != nil {
		t.Fatalf("new ticket not recorded: %v", err)
	}
}

// A repository Ticket from before records whose worktree was cleaned up after its work: its diff still shows (the
// views use what is derived, unrecorded), and a reopening is refused with why.
func TestUnrecordedCleanedWorktreeKeepsItsDiff(t *testing.T) {
	h := newHarness(t, nil)
	repo := newRepo(t)
	tk := h.submit("Work in a repo", repo)
	tk, _ = h.running(tk.ID)
	wt := TicketDir(h.cfg.WorkRoot, tk)
	if err := os.WriteFile(filepath.Join(wt, "done.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", "done.txt")
	git(t, wt, "commit", "-q", "-m", "the work")
	git(t, repo, "worktree", "remove", "--force", wt)
	record := filepath.Join(h.cfg.StateDir, placementsDir, tk.ID+".json")
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	d, err := h.n.TicketDiff(t.Context(), tk.ID)
	if err != nil || len(d.Files) != 1 || d.Files[0].Path != "done.txt" {
		t.Fatalf("diff = %+v, %v; want done.txt", d, err)
	}
	if _, err := h.n.place(tk); err == nil || err.Error() != "the directory became a git repository after the Ticket started, or its worktree was removed; resubmit" {
		t.Fatalf("reopen = %v, want the refusal", err)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("recorded: %v", err)
	}
}

// The read-only views of a plain directory its worker made a repository (the record gone) run nothing the repository
// configures: no filter, textconv, external diff, hook or fsmonitor.
func TestUnrecordedProbeRepoViewedRunsNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell programs")
	}
	h := newHarness(t, nil)
	dir := t.TempDir()
	tk := h.submit("Work in a folder", dir)
	tk, _ = h.running(tk.ID)
	marker := filepath.Join(t.TempDir(), "ran")
	prog := filepath.Join(t.TempDir(), "evil.sh")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\necho \"$0 $*\" >> "+marker+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The worker's repository: its branch for the Ticket changes an attributed file, and its config names a program
	// for everything git could run.
	git(t, dir, "init", "-q", "-b", "main")
	write(".gitattributes", "*.txt filter=evil diff=evil\n")
	write("a.txt", "a\n")
	git(t, dir, "add", ".gitattributes", "a.txt")
	git(t, dir, "commit", "-q", "-m", "base")
	git(t, dir, "checkout", "-q", "-b", branchName(tk))
	write("a.txt", "b\n")
	git(t, dir, "commit", "-q", "-am", "change")
	git(t, dir, "checkout", "-q", "main")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"post-checkout", "post-index-change", "reference-transaction"} {
		if err := os.Symlink(prog, filepath.Join(hooks, h)); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range map[string]string{"filter.evil.clean": prog, "filter.evil.smudge": prog, "filter.evil.process": prog,
		"diff.evil.textconv": prog, "diff.evil.command": prog, "diff.external": prog, "core.fsmonitor": prog,
		"core.hooksPath": hooks, "core.pager": prog} {
		git(t, dir, "config", k, v)
	}
	write("a.txt", "c\n") // a dirty working tree too
	if err := os.Remove(filepath.Join(h.cfg.StateDir, placementsDir, tk.ID+".json")); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	d, err := h.n.TicketDiff(ctx, tk.ID)
	if err != nil || len(d.Files) != 1 || d.Files[0].Path != "a.txt" {
		t.Fatalf("diff = %+v, %v", d, err)
	}
	_, _, _ = h.n.TicketFiles(ctx, tk.ID)
	if f, err := h.n.OpenTicketFile(ctx, tk.ID, "a.txt"); err == nil {
		f.Close()
	}
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("a view ran the repository's program:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(h.cfg.StateDir, placementsDir, tk.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("a view recorded the placement: %v", err)
	}
}

// A read-only session's sandbox keeps the owner's writable paths outside its working directory (a build cache, so
// its tests can run) and drops the rest: the working directory and what holds it or lies in it are denied.
func TestReadOnlySandboxKeepsOutsideCaches(t *testing.T) {
	work := t.TempDir()
	cache := filepath.Join(t.TempDir(), "gocache")
	n := &Node{cfg: Config{Sandbox: &SandboxConfig{AllowWrite: []string{cache, filepath.Join(work, "build"), filepath.Dir(work)}}}}
	sb, _ := n.sessionSandbox(work, true, false)
	if !slices.Equal(sb.AllowWrite, []string{cache}) {
		t.Fatalf("allowWrite = %v, want only the cache %s", sb.AllowWrite, cache)
	}
	if !slices.Contains(sb.DenyWrite, work) {
		t.Fatalf("denyWrite %v lacks the working directory", sb.DenyWrite)
	}
	if sb, _ := n.sessionSandbox(work, false, false); !slices.Contains(sb.AllowWrite, work) || !slices.Contains(sb.AllowWrite, cache) {
		t.Fatalf("worker allowWrite = %v", sb.AllowWrite)
	}
}

// A read-only session in a worktree keeps no owner path that holds or lies in the repository's git directories, and
// denies them: a review commits nothing.
func TestReadOnlySandboxDropsGitDirs(t *testing.T) {
	dir := t.TempDir()
	repo, work := filepath.Join(dir, "repo"), filepath.Join(dir, "wt")
	for _, args := range [][]string{{"init", "-q", repo}, {"-C", repo, "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", repo, "worktree", "add", "-q", work, "-b", "athena/t-1"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitDir, common := gitDirs(work)
	if common == "" || gitDir == common {
		t.Fatalf("gitDirs(%s) = %q, %q, want a worktree", work, gitDir, common)
	}
	cache := filepath.Join(dir, "gocache")
	n := &Node{cfg: Config{Sandbox: &SandboxConfig{AllowWrite: []string{cache, repo, filepath.Join(common, "objects"), gitDir}}}}
	sb, _ := n.sessionSandbox(work, true, false)
	if !slices.Equal(sb.AllowWrite, []string{cache}) {
		t.Fatalf("allowWrite = %v, want only the cache %s", sb.AllowWrite, cache)
	}
	for _, d := range []string{work, common, gitDir} {
		if !slices.Contains(sb.DenyWrite, d) {
			t.Fatalf("denyWrite %v lacks %s", sb.DenyWrite, d)
		}
	}
}

// A restricted session's sandbox is required, reaches GitHub alone and writes its own caches, not the owner's, with the
// owner's sandbox configured or not; a restricted review's stays read-only too.
func TestRestrictedSessionSandboxWithoutConfig(t *testing.T) {
	work := t.TempDir()
	for _, c := range []*SandboxConfig{nil, {AllowedDomains: []string{"pypi.org"}, AllowWrite: []string{"/cache/go"}}} {
		n := &Node{cfg: Config{Sandbox: c, SocketPath: "/tmp/n.sock"}}
		for _, readOnly := range []bool{false, true} {
			sb, _ := n.sessionSandbox(work, readOnly, true)
			if sb == nil || !sb.Required || !slices.Equal(sb.AllowedDomains, GitHubDomains) || !slices.Contains(sb.UnixSockets, "/tmp/n.sock") {
				t.Fatalf("config %+v readOnly %v: sandbox = %+v", c, readOnly, sb)
			}
			if readOnly == slices.Contains(sb.AllowWrite, work) || readOnly != slices.Contains(sb.DenyWrite, work) {
				t.Fatalf("config %+v readOnly %v: allowWrite %v denyWrite %v", c, readOnly, sb.AllowWrite, sb.DenyWrite)
			}
			// The owner's extra writable paths are dropped; the session's own caches take their place.
			if slices.Contains(sb.AllowWrite, "/cache/go") || !slices.Contains(sb.AllowWrite, restrictedCacheDir(work)) {
				t.Fatalf("config %+v readOnly %v: allowWrite %v, want its cache and not the owner's", c, readOnly, sb.AllowWrite)
			}
		}
		if c == nil {
			if sb, _ := n.sessionSandbox(work, false, false); sb != nil {
				t.Fatalf("a standard session without a sandbox got %+v", sb)
			}
		}
	}
}
