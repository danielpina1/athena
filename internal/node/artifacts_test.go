package node

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/danielpina1/athena/internal/core"
)

// ticketEngine answers Ticket from a map; every other callback is absent.
type ticketEngine struct {
	EngineCallbacks
	tickets map[string]core.Ticket
}

func (e ticketEngine) Ticket(id string) (core.Ticket, core.Stage, error) {
	t, ok := e.tickets[id]
	if !ok {
		return core.Ticket{}, core.Stage{}, errors.New("no such ticket")
	}
	return t, core.Stage{}, nil
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// diffNode is a node with Ticket T-0001 in a temp repository whose worktree is on athena/t-0001.
func diffNode(t *testing.T) (*Node, string) {
	t.Helper()
	repo := newRepo(t)
	work := t.TempDir()
	tk := core.Ticket{ID: "T-0001", Title: "Diff me", Execution: &core.Execution{Directory: repo}}
	n := New(Config{WorkRoot: work, StateDir: t.TempDir()}, ticketEngine{tickets: map[string]core.Ticket{"T-0001": tk}})
	wt := TicketDir(work, tk)
	git(t, repo, "worktree", "add", "-q", "-b", "athena/t-0001", wt)
	return n, wt
}

func TestTicketDiffAgainstMergeBase(t *testing.T) {
	n, wt := diffNode(t)
	repo := filepath.Dir(git(t, wt, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	// The base moves on after the branch left it: the diff must not show that.
	writeFile(t, filepath.Join(repo, "other"), "elsewhere\n")
	git(t, repo, "add", "other")
	git(t, repo, "commit", "-q", "-m", "main moves")

	writeFile(t, filepath.Join(wt, "README"), "hi\nnew line\n")
	writeFile(t, filepath.Join(wt, "sub", "file.txt"), "one\ntwo\nthree\n")
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "work")

	d, err := n.TicketDiff(context.Background(), "T-0001")
	if err != nil {
		t.Fatal(err)
	}
	if d.Branch != "athena/t-0001" || len(d.Base) != 40 || d.Truncated {
		t.Fatalf("diff header = %+v", d)
	}
	if want := git(t, repo, "rev-parse", "HEAD~1"); d.Base != want {
		t.Fatalf("base = %s, want the merge-base %s", d.Base, want)
	}
	want := []DiffFile{{Path: "README", Status: "M", Added: 1}, {Path: "sub/file.txt", Status: "A", Added: 3}}
	if !slices.Equal(d.Files, want) {
		t.Fatalf("files = %+v, want %+v", d.Files, want)
	}
	if !strings.Contains(d.Patch, "+new line") || strings.Contains(d.Patch, "elsewhere") {
		t.Fatalf("patch = %q", d.Patch)
	}
}

func TestTicketDiffRenameAndBinary(t *testing.T) {
	n, wt := diffNode(t)
	git(t, wt, "mv", "README", "READ ME\tnow")
	if err := os.WriteFile(filepath.Join(wt, "bin"), []byte{0, 1, 2, 0}, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "move")
	d, err := n.TicketDiff(context.Background(), "T-0001")
	if err != nil {
		t.Fatal(err)
	}
	want := []DiffFile{{Path: "READ ME\tnow", OldPath: "README", Status: "R"}, {Path: "bin", Status: "A", Binary: true}}
	slices.SortFunc(d.Files, func(a, b DiffFile) int { return strings.Compare(a.Path, b.Path) })
	if !slices.Equal(d.Files, want) {
		t.Fatalf("files = %+v, want %+v", d.Files, want)
	}
}

func TestTicketDiffTruncatesAt2MiB(t *testing.T) {
	n, wt := diffNode(t)
	writeFile(t, filepath.Join(wt, "huge"), strings.Repeat("0123456789abcdef\n", 200_000))
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "huge")
	d, err := n.TicketDiff(context.Background(), "T-0001")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Truncated || len(d.Patch) != 2<<20 {
		t.Fatalf("truncated=%v, patch %d bytes, want %d", d.Truncated, len(d.Patch), 2<<20)
	}
	if len(d.Files) != 1 || d.Files[0].Added != 200_000 {
		t.Fatalf("files = %+v", d.Files)
	}
}

func TestTicketDiffNoShellNoCredentialsEnv(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghs_secret")
	t.Setenv("GH_TOKEN", "ghs_secret")
	t.Setenv("GIT_ASKPASS", "/bin/evil")
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret")
	n, wt := diffNode(t)
	writeFile(t, filepath.Join(wt, "f"), "x\n")
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "f")

	type call struct {
		dir  string
		env  []string
		args []string
	}
	var calls []call
	prev := diffGit
	diffGit = func(ctx context.Context, dir string, env []string, limit int, args ...string) ([]byte, bool, error) {
		calls = append(calls, call{dir, env, args})
		if _, ok := ctx.Deadline(); !ok {
			t.Error("git runs without a deadline")
		}
		return prev(ctx, dir, env, limit, args...)
	}
	t.Cleanup(func() { diffGit = prev })
	if _, err := n.TicketDiff(context.Background(), "T-0001"); err != nil {
		t.Fatal(err)
	}
	var diffs int
	for _, c := range calls {
		for _, kv := range c.env {
			name, _, _ := strings.Cut(kv, "=")
			if strings.Contains(kv, "secret") || name == "GIT_ASKPASS" || name == "GITHUB_TOKEN" || name == "GH_TOKEN" || name == "ANTHROPIC_API_KEY" {
				t.Errorf("git inherited %s", name)
			}
		}
		for _, kv := range []string{"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_GRAFT_FILE=" + os.DevNull,
			"GIT_CONFIG_KEY_2=protocol.allow", "GIT_CONFIG_VALUE_2=never"} {
			if !slices.Contains(c.env, kv) {
				t.Errorf("env lacks %s: %v", kv, c.env)
			}
		}
		if slices.Contains(c.args, "-c") || c.args[0] == "sh" || c.args[0] == "-c" {
			t.Errorf("argv looks like a shell: %v", c.args)
		}
		if c.args[0] == "diff" {
			diffs++
			for _, flag := range []string{"--no-ext-diff", "--no-textconv", "--no-color", "--find-renames"} {
				if !slices.Contains(c.args, flag) {
					t.Errorf("diff lacks %s: %v", flag, c.args)
				}
			}
		}
	}
	if diffs != 2 {
		t.Fatalf("%d diff runs, want 2 (stat and patch)", diffs)
	}
	if !slices.Contains(calls[len(calls)-2].args, "--numstat") {
		t.Fatalf("first diff is not the numstat: %v", calls[len(calls)-2].args)
	}
}

func TestTicketDiffUnknownAndNoRepo(t *testing.T) {
	n, _ := diffNode(t)
	if _, err := n.TicketDiff(context.Background(), "T-9999"); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("unknown Ticket: %v", err)
	}
	if _, err := n.TicketDiff(context.Background(), "../etc"); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("bad id: %v", err)
	}
	m, _ := filesNode(t)
	if _, err := m.TicketDiff(context.Background(), "T-0002"); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("a Ticket without a repository: %v", err)
	}
	if _, _, err := n.TicketFiles(context.Background(), "T-9999"); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("files of an unknown Ticket: %v", err)
	}
}

// filesNode is a node with Ticket T-0002, placed without a repository: its directory is a plain folder.
func filesNode(t *testing.T) (*Node, string) {
	t.Helper()
	dir := t.TempDir()
	tk := core.Ticket{ID: "T-0002", Title: "Plain", Execution: &core.Execution{Directory: dir}}
	n := New(Config{WorkRoot: t.TempDir(), StateDir: t.TempDir()}, ticketEngine{tickets: map[string]core.Ticket{"T-0002": tk}})
	return n, dir
}

func TestTicketFilesSkipsAthenaDirAndSymlinks(t *testing.T) {
	n, dir := filesNode(t)
	writeFile(t, filepath.Join(dir, "b.txt"), "bb")
	writeFile(t, filepath.Join(dir, "a", "z.txt"), "z")
	writeFile(t, filepath.Join(dir, "a", "deep", "d.txt"), "d")
	writeFile(t, filepath.Join(dir, ".athena", "socket"), "s")
	writeFile(t, filepath.Join(dir, "sub", ".athena", "keep"), "k")
	outside := filepath.Join(t.TempDir(), "secret")
	writeFile(t, outside, "secret")
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	files, truncated, err := n.TicketFiles(context.Background(), "T-0002")
	if err != nil || truncated {
		t.Fatalf("err=%v truncated=%v", err, truncated)
	}
	var got []string
	for _, f := range files {
		got = append(got, f.Path)
	}
	want := []string{"a/deep/d.txt", "a/z.txt", "b.txt", "sub/.athena/keep"}
	if !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if files[2].Size != 2 || files[2].ModTime.IsZero() {
		t.Fatalf("entry = %+v", files[2])
	}
}

func TestTicketFilesCapsAt1000BreadthFirst(t *testing.T) {
	n, dir := filesNode(t)
	for i := range 1005 {
		writeFile(t, filepath.Join(dir, "top"+string(rune('a'+i%26))+strings.Repeat("x", i/26)), "")
	}
	writeFile(t, filepath.Join(dir, "d", "deep"), "")
	files, truncated, err := n.TicketFiles(context.Background(), "T-0002")
	if err != nil || !truncated || len(files) != 1000 {
		t.Fatalf("err=%v truncated=%v len=%d", err, truncated, len(files))
	}
	for _, f := range files {
		if f.Path == "d/deep" {
			t.Fatal("a deeper file made the cut before shallower ones")
		}
	}
}

func TestOpenTicketFileRefusesEscape(t *testing.T) {
	n, dir := filesNode(t)
	writeFile(t, filepath.Join(dir, "ok.txt"), "fine")
	writeFile(t, filepath.Join(dir, ".athena", "socket"), "s")
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "x"), "outside")
	if err := os.Symlink(filepath.Join(outside, "x"), filepath.Join(dir, "esc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, ".athena"), filepath.Join(dir, "viaathena")); err != nil {
		t.Fatal(err)
	}
	f, err := n.OpenTicketFile(context.Background(), "T-0002", "ok.txt")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(f); string(b) != "fine" {
		t.Fatalf("content %q", b)
	}
	f.Close()
	for _, rel := range []string{"../x", "/etc/passwd", ".athena/socket", ".ATHENA/socket", ".Athena/socket", "./.athena/socket", "a/../.athena/socket", "", ".", "..", "esc", "viaathena/socket", "missing", "ok.txt/x"} {
		if f, err := n.OpenTicketFile(context.Background(), "T-0002", rel); !errors.Is(err, ErrNotFound) {
			if f != nil {
				f.Close()
			}
			t.Errorf("OpenTicketFile(%q) = %v, want ErrNotFound", rel, err)
		}
	}
	if _, err := n.OpenTicketFile(context.Background(), "T-9999", "ok.txt"); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("unknown Ticket: %v", err)
	}
}

func TestTicketDiffIgnoresReplaceRefsAndGrafts(t *testing.T) {
	n, wt := diffNode(t)
	repo := filepath.Dir(git(t, wt, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	baseSHA := git(t, repo, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(wt, "f"), "real\n")
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "one")
	first := git(t, wt, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(wt, "g"), "more\n")
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "two")

	// A worker plants a replacement for the blob the owner would read, and a graft that cuts the branch off its base.
	realBlob := git(t, wt, "rev-parse", "HEAD:f")
	cmd := exec.Command("git", "-C", wt, "hash-object", "-w", "--stdin")
	cmd.Stdin = strings.NewReader("fake\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	git(t, wt, "replace", realBlob, strings.TrimSpace(string(out)))
	writeFile(t, filepath.Join(repo, ".git", "info", "grafts"), first+"\n")

	d, err := n.TicketDiff(context.Background(), "T-0001")
	if err != nil {
		t.Fatal(err)
	}
	if d.Base != baseSHA {
		t.Fatalf("base = %s, want %s (a graft moved it)", d.Base, baseSHA)
	}
	if !strings.Contains(d.Patch, "+real") || strings.Contains(d.Patch, "fake") {
		t.Fatalf("patch shows the replacement: %q", d.Patch)
	}
}

func TestTicketDiffNeverLazyFetches(t *testing.T) {
	n, wt := diffNode(t)
	repo := filepath.Dir(git(t, wt, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	// A bare repository holds a blob the Ticket's repository lacks; the repository names it as a promisor remote and
	// allows file transport at its own scope, which overrides any command-scope protocol.allow=never.
	bare := t.TempDir()
	git(t, bare, "init", "-q", "--bare")
	git(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	cmd := exec.Command("git", "-C", bare, "hash-object", "-w", "--stdin")
	cmd.Stdin = strings.NewReader("only in the promisor\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.TrimSpace(string(out))
	git(t, repo, "config", "remote.p.url", bare)
	git(t, repo, "config", "remote.p.promisor", "true")
	git(t, repo, "config", "protocol.file.allow", "always")

	// A commit on the Ticket's branch whose tree names the missing blob.
	mk := exec.Command("git", "-C", repo, "mktree", "--missing")
	mk.Stdin = strings.NewReader("100644 blob " + blob + "\tf\n")
	tree, err := mk.Output()
	if err != nil {
		t.Fatal(err)
	}
	commit := git(t, repo, "commit-tree", strings.TrimSpace(string(tree)), "-p", git(t, repo, "rev-parse", "athena/t-0001"), "-m", "lazy")
	git(t, repo, "update-ref", "refs/heads/athena/t-0001", commit)

	_, derr := n.TicketDiff(context.Background(), "T-0001")
	// Look without fetching ourselves.
	t.Setenv("GIT_NO_LAZY_FETCH", "1")
	present := exec.Command("git", "-C", repo, "cat-file", "-e", blob).Run() == nil
	if present {
		t.Fatal("the diff fetched the blob from the repository's promisor remote")
	}
	if derr == nil {
		t.Fatal("the diff of a missing object succeeded")
	}
}

func TestTicketDiffStatCutDropsUncountedFiles(t *testing.T) {
	raw := ":100644 100644 a b M\x00one\x00:100644 100644 a b M\x00two\x003\t1\tone\x004\t"
	files := parseDiffStat([]byte(raw), true)
	if len(files) != 1 || files[0].Path != "one" || files[0].Added != 3 || files[0].Deleted != 1 {
		t.Fatalf("files = %+v", files)
	}
}

func TestOpenTicketFileRefusesHardLinks(t *testing.T) {
	n, dir := filesNode(t)
	outside := filepath.Join(t.TempDir(), "o")
	writeFile(t, outside, "o")
	if err := os.Link(outside, filepath.Join(dir, "hard")); err != nil {
		t.Skip(err)
	}
	writeFile(t, filepath.Join(dir, "plain"), "p")
	if f, err := n.OpenTicketFile(context.Background(), "T-0002", "hard"); !errors.Is(err, ErrNotFound) {
		if f != nil {
			f.Close()
		}
		t.Fatalf("hard link opened: %v", err)
	}
	files, _, _ := n.TicketFiles(context.Background(), "T-0002")
	if len(files) != 1 || files[0].Path != "plain" {
		t.Fatalf("files = %+v", files)
	}
}

func TestTicketFilesDirectoryQueueCapped(t *testing.T) {
	n, dir := filesNode(t)
	for i := range maxWalkDirs + 50 {
		writeFile(t, filepath.Join(dir, "d"+strings.Repeat("0", 4-len(strconv.Itoa(i)))+strconv.Itoa(i), "f"), "")
	}
	files, truncated, err := n.TicketFiles(context.Background(), "T-0002")
	if err != nil || !truncated || len(files) >= maxWalkDirs+50 {
		t.Fatalf("err=%v truncated=%v files=%d", err, truncated, len(files))
	}
}

func TestTicketDiffNonASCIINamesAreNotQuoted(t *testing.T) {
	n, wt := diffNode(t)
	writeFile(t, filepath.Join(wt, "café.txt"), "x\n")
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "accent")
	d, err := n.TicketDiff(context.Background(), "T-0001")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Patch, "diff --git a/café.txt b/café.txt") || strings.Contains(d.Patch, `\303`) {
		t.Fatalf("patch = %q", d.Patch)
	}
}

// Names that look like secrets are never listed, and never served, in either kind of Ticket.
var secretNames = []string{".env", ".env.local", ".ENV.production", "server.pem", "tls.KEY", "id_rsa", "id_ed25519.pub", "conf/.env", ".env.d/inner.txt", "a/certs/ca.pem"}

func TestTicketFilesSkipSecretNames(t *testing.T) {
	n, dir := filesNode(t)
	writeFile(t, filepath.Join(dir, "notes.md"), "fine")
	writeFile(t, filepath.Join(dir, "keys.md"), "a document about keys")
	for _, name := range secretNames {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(name)), "SECRET=1")
	}
	files, _, err := n.TicketFiles(context.Background(), "T-0002")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, f.Path)
	}
	if want := []string{"keys.md", "notes.md"}; !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	for _, name := range secretNames {
		if f, err := n.OpenTicketFile(context.Background(), "T-0002", name); !errors.Is(err, ErrNotFound) {
			if f != nil {
				f.Close()
			}
			t.Errorf("OpenTicketFile(%q) = %v, want ErrNotFound", name, err)
		}
	}
}

// repoFilesNode is diffNode with work in the worktree: on the branch a changed README, a new docs/REVIEW.md and a
// deleted old.txt (from the base); uncommitted, a new REPORT.md, an ignored build/out.log, a new .env, a symlink and
// the node's .athena directory.
func repoFilesNode(t *testing.T) (*Node, string) {
	t.Helper()
	n, wt := diffNode(t)
	repo := filepath.Dir(git(t, wt, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	writeFile(t, filepath.Join(repo, "old.txt"), "old\n")
	writeFile(t, filepath.Join(repo, "untouched.txt"), "base\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "base files")
	git(t, wt, "merge", "-q", "--ff-only", "main")
	writeFile(t, filepath.Join(wt, "README"), "hi\nmore\n")
	writeFile(t, filepath.Join(wt, "docs", "REVIEW.md"), "# Review\n")
	writeFile(t, filepath.Join(wt, ".gitignore"), "build/\n.athena/\n")
	git(t, wt, "rm", "-q", "old.txt")
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "work")
	writeFile(t, filepath.Join(wt, "REPORT.md"), "# Report\n\nAll good.\n")
	writeFile(t, filepath.Join(wt, "build", "out.log"), "ignored\n")
	writeFile(t, filepath.Join(wt, ".env"), "TOKEN=x\n")
	writeFile(t, filepath.Join(wt, ".athena", "socket"), "s")
	writeFile(t, filepath.Join(wt, "ATHENA-notes", "keep.txt"), "kept\n")
	if err := os.Symlink(filepath.Join(repo, "untouched.txt"), filepath.Join(wt, "link.md")); err != nil {
		t.Fatal(err)
	}
	return n, wt
}

func TestTicketFilesOfRepositoryTicket(t *testing.T) {
	n, _ := repoFilesNode(t)
	files, truncated, err := n.TicketFiles(context.Background(), "T-0001")
	if err != nil || truncated {
		t.Fatalf("err=%v truncated=%v", err, truncated)
	}
	type entry struct{ path, status string }
	var got []entry
	for _, f := range files {
		got = append(got, entry{f.Path, f.Status})
		if f.ModTime.IsZero() {
			t.Errorf("%s has no time", f.Path)
		}
	}
	want := []entry{{".gitignore", "A"}, {"ATHENA-notes/keep.txt", "?"}, {"README", "M"}, {"REPORT.md", "?"}, {"docs/REVIEW.md", "A"}}
	if !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if files[3].Size != int64(len("# Report\n\nAll good.\n")) {
		t.Errorf("REPORT.md size = %d", files[3].Size)
	}
}

func TestOpenTicketFileInWorktree(t *testing.T) {
	n, wt := repoFilesNode(t)
	for rel, want := range map[string]string{"REPORT.md": "# Report\n\nAll good.\n", "docs/REVIEW.md": "# Review\n", "README": "hi\nmore\n"} {
		f, err := n.OpenTicketFile(context.Background(), "T-0001", rel)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		b, _ := io.ReadAll(f)
		f.Close()
		if string(b) != want {
			t.Errorf("%s = %q", rel, b)
		}
	}
	// Only what the Ticket added or changed opens: not an ignored file, a file of the base, git's own files, a secret
	// or a symlink.
	if err := os.Symlink("/etc/hosts", filepath.Join(wt, "hosts.txt")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"build/out.log", "untouched.txt", "old.txt", ".git", ".git/HEAD", ".env", ".athena/socket", "link.md", "hosts.txt", "../x", "/etc/passwd", "docs/../REPORT.md/x", ""} {
		if f, err := n.OpenTicketFile(context.Background(), "T-0001", rel); !errors.Is(err, ErrNotFound) {
			if f != nil {
				f.Close()
			}
			t.Errorf("OpenTicketFile(%q) = %v, want ErrNotFound", rel, err)
		}
	}
	// A worktree that is gone (an ended Ticket's, cleaned up) has no files.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := n.TicketFiles(context.Background(), "T-0001"); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("files of a removed worktree: %v", err)
	}
}

// Listing a worktree's files runs nothing of the repository's: no clean filter (named by an attribute the worker can
// write), no fsmonitor and no hook.
func TestWorktreeFilesRunNoFilterOrHook(t *testing.T) {
	n, wt := repoFilesNode(t)
	repo := filepath.Dir(git(t, wt, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	marker := filepath.Join(t.TempDir(), "ran")
	cmd := "touch " + marker
	git(t, repo, "config", "filter.evil.clean", cmd)
	git(t, repo, "config", "filter.evil.process", cmd)
	git(t, repo, "config", "core.fsmonitor", cmd)
	git(t, repo, "config", "core.hooksPath", filepath.Join(t.TempDir(), "hooks"))
	writeFile(t, filepath.Join(wt, ".gitattributes"), "* filter=evil\n")
	writeFile(t, filepath.Join(wt, "README"), "hi\nmore\nuncommitted\n")
	if _, _, err := n.TicketFiles(context.Background(), "T-0001"); err != nil {
		t.Fatal(err)
	}
	f, err := n.OpenTicketFile(context.Background(), "T-0001", "REPORT.md")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("listing the worktree ran a command of the repository's")
	}
}
