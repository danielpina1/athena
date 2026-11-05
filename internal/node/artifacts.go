package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/core"
)

// What a Ticket leaves behind, read for the dashboard: its branch's diff (a Ticket in a git repository) or the files in
// its directory (one without).

var (
	// ErrNoWorkspace is a Ticket that is unknown, not placed, or placed the other way (a diff of a Ticket without a
	// repository, files of one with).
	ErrNoWorkspace = errors.New("node: the Ticket has no such workspace")
	// ErrNotFound is a file that does not exist, or may not be read.
	ErrNotFound = errors.New("node: not found")
)

const (
	diffTimeout    = 10 * time.Second
	maxPatchBytes  = 2 << 20
	maxNumstatSize = 4 << 20
	maxFileEntries = 1000
)

// Diff is a Ticket's branch against its base: the merge-base with the repository's default branch.
type Diff struct {
	// Base is the commit the Ticket's changes are measured from and Branch its branch (athena/<lowercased id>).
	Base, Branch string
	Files        []DiffFile
	// Patch is the unified diff, cut at 2 MiB.
	Patch string
	// Truncated is set when the patch was cut, or the file list was (its git output passed 4 MiB: the files listed are
	// complete ones, the rest is missing).
	Truncated bool
}

// DiffFile is one changed file. Status is A (added), M (modified), D (deleted), R (renamed), C (copied) or T (type
// changed); OldPath is set for R and C. Binary files have no line counts.
type DiffFile struct {
	Path, OldPath, Status string
	Added, Deleted        int
	Binary                bool
}

// FileEntry is a file in a Ticket's directory, or one its worktree added or changed; Path is relative, with forward
// slashes. Status is "" for a Ticket without a repository; in a worktree it is the branch's change (A, M, R, C or T,
// as in DiffFile) or "?" for a new file not committed yet.
type FileEntry struct {
	Path    string
	Status  string
	Size    int64
	ModTime time.Time
}

// diffGit runs git for the diff and returns at most limit bytes of its stdout (truncated reports more was written; git
// is then stopped). A variable so a test can see the argv and environment.
var diffGit = runDiffGit

func runDiffGit(ctx context.Context, dir string, env []string, limit int, args ...string) (out []byte, truncated bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = env
	stderr := &limitedBuffer{max: 4 << 10}
	cmd.Stderr = stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	out, rerr := io.ReadAll(io.LimitReader(pipe, int64(limit)+1))
	if len(out) > limit {
		out, truncated = out[:limit], true
		cancel() // stop git; the rest is not wanted
	} else {
		_, _ = io.Copy(io.Discard, pipe)
	}
	werr := cmd.Wait()
	if truncated {
		return out, true, nil
	}
	if rerr != nil {
		return nil, false, rerr
	}
	if werr != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, fmt.Errorf("git %s: %w: %s", args[0], werr, strings.TrimSpace(stderr.buf.String()))
	}
	return out, false, nil
}

// diffEnv is git's environment for reading a Ticket's repository, which a worker can write to (its refs, objects and
// some of its config): the node's minimal one (no credentials, no prompt), with
//   - no hook or fsmonitor of the repository's own, no credential helper, and no transport (protocol.allow=never at
//     command scope; a repository-scope protocol.<name>.allow would override it, hence also:)
//   - GIT_NO_LAZY_FETCH, so a missing object is an error and never a fetch from a promisor remote (whose URL, an
//     ext:: one, would run a command outside the sandbox),
//   - GIT_NO_REPLACE_OBJECTS and an empty graft file, so refs/replace/* and info/grafts cannot change what the owner
//     reads in the diff or where its base is.
func diffEnv() []string {
	env := withGitConfig(nodeGitBaseEnv(), append(slices.Clone(noHooks), [][2]string{{"protocol.allow", "never"}, {"credential.helper", ""}, {"advice.graftFileDeprecated", "false"}, {"core.quotePath", "false"}}...))
	return append(env, "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_GRAFT_FILE="+os.DevNull)
}

// limitedBuffer keeps the first max bytes written to it.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// ticketPlacement is where an open Ticket is placed, or ErrNoWorkspace.
func (n *Node) ticketPlacement(ticketID string) (placement, core.Ticket, error) {
	eng := n.engine()
	if eng == nil || !validStreamID(ticketID) {
		return placement{}, core.Ticket{}, ErrNoWorkspace
	}
	t, _, err := eng.Ticket(ticketID)
	if err != nil {
		return placement{}, core.Ticket{}, ErrNoWorkspace
	}
	p, err := n.place(t)
	if err != nil {
		return placement{}, core.Ticket{}, ErrNoWorkspace
	}
	return p, t, nil
}

// TicketDiff is the diff of a Ticket placed in a git repository: its branch against the merge-base of the branch with
// origin's default branch, else with the repository's own HEAD. It touches the repository (a clone, for a GitHub
// Ticket) only, with no token, and never the mirror.
func (n *Node) TicketDiff(ctx context.Context, ticketID string) (Diff, error) {
	p, t, err := n.ticketPlacement(ticketID)
	if err != nil {
		return Diff{}, err
	}
	branch := branchName(t)
	if p.repo == "" {
		return Diff{}, ErrNoWorkspace
	}
	ctx, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()
	b, err := branchFiles(ctx, p.repo, branch)
	if err != nil {
		return Diff{}, err
	}
	d := Diff{Base: b.base, Branch: branch, Files: b.files, Truncated: b.cut}
	patch, cut, err := b.git(maxPatchBytes, append(diffCommon[:len(diffCommon):len(diffCommon)], b.span)...)
	if err != nil {
		return Diff{}, err
	}
	if cut {
		// Do not end in the middle of a character.
		for i := 0; i < utf8.UTFMax && len(patch) > 0; i++ {
			if r, w := utf8.DecodeLastRune(patch); r != utf8.RuneError || w != 1 {
				break
			}
			patch = patch[:len(patch)-1]
		}
		d.Truncated = true
	}
	d.Patch = string(bytes.ToValidUTF8(patch, []byte("\uFFFD")))
	return d, nil
}

var diffCommon = []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--find-renames"}

// branchDiff is a branch's span against its base and the files it changes, with git to read more of it.
type branchDiff struct {
	base, span string
	files      []DiffFile
	cut        bool
	git        func(limit int, args ...string) ([]byte, bool, error)
}

// branchFiles reads the files branch changes in repo against its base: the merge-base with origin's default branch,
// else with the repository's own HEAD. A branch that does not exist is ErrNoWorkspace.
func branchFiles(ctx context.Context, repo, branch string) (branchDiff, error) {
	env := diffEnv()
	git := func(limit int, args ...string) ([]byte, bool, error) {
		return diffGit(ctx, repo, env, limit, args...)
	}
	text := func(args ...string) (string, error) {
		out, _, err := git(1<<10, args...)
		return strings.TrimSpace(string(out)), err
	}
	ref := "refs/heads/" + branch
	if _, err := text("rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		if ctx.Err() != nil {
			return branchDiff{}, ctx.Err()
		}
		return branchDiff{}, ErrNoWorkspace
	}
	base, err := text("merge-base", "refs/remotes/origin/HEAD", ref)
	if err != nil {
		if ctx.Err() != nil {
			return branchDiff{}, ctx.Err()
		}
		if base, err = text("merge-base", "HEAD", ref); err != nil {
			return branchDiff{}, err
		}
	}
	span := base + "..." + ref
	// --raw gives each file's status and --numstat its counts, in one run; -z keeps odd file names exact.
	stat, cut, err := git(maxNumstatSize, append(diffCommon[:len(diffCommon):len(diffCommon)], "--raw", "--numstat", "-z", span)...)
	if err != nil {
		return branchDiff{}, err
	}
	return branchDiff{base: base, span: span, files: parseDiffStat(stat, cut), cut: cut, git: git}, nil
}

// parseDiffStat reads `git diff --raw --numstat -z`: the raw records (":modes shas status", then one path, or two for
// a rename or copy), then the numstat records ("added\tdeleted\tpath", or the same with an empty path and the old and
// new paths after it).
//
// When the output was cut (cut), its last record may be partial and is ignored, and a file whose counts did not make it
// is left out: no file shows counts that were never read.
func parseDiffStat(out []byte, cut bool) []DiffFile {
	toks := strings.Split(string(out), "\x00")
	if cut {
		toks = toks[:len(toks)-1]
	}
	var files []DiffFile
	i := 0
	for ; i < len(toks) && strings.HasPrefix(toks[i], ":"); i++ {
		fields := strings.Fields(toks[i])
		if len(fields) < 5 || len(toks)-i < 2 {
			break
		}
		f := DiffFile{Status: fields[4][:1]}
		if f.Status == "R" || f.Status == "C" {
			if len(toks)-i < 3 {
				break
			}
			f.OldPath, f.Path = toks[i+1], toks[i+2]
			i += 2
		} else {
			f.Path = toks[i+1]
			i++
		}
		files = append(files, f)
	}
	byPath := make(map[string]int, len(files))
	for j := range files {
		byPath[files[j].Path] = j
	}
	counted := make([]bool, len(files))
	for ; i < len(toks); i++ {
		parts := strings.SplitN(toks[i], "\t", 3)
		if len(parts) != 3 {
			continue
		}
		p := parts[2]
		if p == "" && i+2 < len(toks) {
			p = toks[i+2]
			i += 2
		}
		j, ok := byPath[p]
		if !ok {
			continue
		}
		f := &files[j]
		counted[j] = true
		if parts[0] == "-" {
			f.Binary = true
			continue
		}
		f.Added, _ = strconv.Atoi(parts[0])
		f.Deleted, _ = strconv.Atoi(parts[1])
	}
	if cut {
		kept := files[:0]
		for j, f := range files {
			if counted[j] {
				kept = append(kept, f)
			}
		}
		files = kept
	}
	return files
}

// athenaDir is the directory of the node's own files in a Ticket's directory; it is never listed or served.
const athenaDir = ".athena"

const (
	maxWalkDirs = 2000
	walkBatch   = 256
	walkTimeout = 10 * time.Second
)

// isAthena reports whether name, an entry of the Ticket's directory, is its .athena: by name in any letter case (the
// file system may fold case), or by being the same file.
func isAthena(root *os.Root, name string, info fs.FileInfo) bool {
	if strings.EqualFold(name, athenaDir) {
		return true
	}
	if info == nil {
		var err error
		if info, err = root.Lstat(name); err != nil {
			return false
		}
	}
	a, err := root.Lstat(athenaDir)
	return err == nil && os.SameFile(a, info)
}

// multiLinked reports a file with more than one hard link: it may be a second name of something outside the directory.
func multiLinked(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Nlink) > 1
}

// privateName reports a name (one component of a path) that is never listed or served, in either kind of Ticket:
// git's own directory, a directory of SSH keys, and names that look like secrets (.env*, keys and certificates, SSH
// identities, credential and password files, Terraform state, KeePass databases). A document about keys ("keys.md",
// "id_mapping.md") is not one.
func privateName(name string) bool {
	l := strings.ToLower(name)
	switch l {
	case ".git", ".ssh", ".gnupg", ".netrc", ".npmrc", ".pypirc", ".pgpass", ".git-credentials", ".htpasswd":
		return true
	}
	for _, id := range []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"} {
		if strings.HasPrefix(l, id) {
			return true
		}
	}
	for _, ext := range []string{".pem", ".key", ".p12", ".pfx", ".tfstate", ".tfstate.backup", ".kdbx"} {
		if strings.HasSuffix(l, ext) {
			return true
		}
	}
	return strings.HasPrefix(l, ".env")
}

// privateIn are the files that are secrets in a directory of that name: (.aws, credentials) is .aws/credentials.
var privateIn = map[[2]string]bool{{".aws", "credentials"}: true, {".docker", "config.json"}: true, {".kube", "config"}: true}

// privatePath reports a relative path that is never listed or served: one with a private name, or a secret file in
// the directory that holds it, at any depth.
func privatePath(rel string) bool {
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		if privateName(part) || (i > 0 && privateIn[[2]string{strings.ToLower(parts[i-1]), strings.ToLower(part)}]) {
			return true
		}
	}
	return false
}

// localPath is rel cleaned, when it names something inside a directory: relative, no NUL, no "..", nothing private.
func localPath(rel string) (string, bool) {
	if rel == "" || strings.ContainsRune(rel, 0) || strings.HasPrefix(rel, "/") || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", false
	}
	rel = path.Clean(rel)
	if privatePath(rel) {
		return "", false
	}
	return rel, true
}

// fileRoot is a Ticket's directory to list or serve, with the directories never to be reached through it: the node's
// state directory and its work root (every Ticket's worktree), matched by identity at any depth.
type fileRoot struct {
	dir  string
	deny []fs.FileInfo
}

func (r fileRoot) denied(info fs.FileInfo) bool {
	for _, d := range r.deny {
		if os.SameFile(info, d) {
			return true
		}
	}
	return false
}

// fileRoot is dir as a Ticket's file root, or ErrNoWorkspace: dir must be a directory, not the state directory or the
// work root, and not inside the state directory unless it is inside the work root (a worktree in ~/.athena/work).
func (n *Node) fileRoot(dir string) (fileRoot, error) {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return fileRoot{}, ErrNoWorkspace
	}
	r := fileRoot{dir: dir}
	var state, work fs.FileInfo
	if n.cfg.StateDir != "" {
		if state, err = os.Stat(n.cfg.StateDir); err == nil {
			r.deny = append(r.deny, state)
		}
	}
	if n.cfg.WorkRoot != "" {
		if work, err = os.Stat(n.cfg.WorkRoot); err == nil {
			r.deny = append(r.deny, work)
		}
	}
	if r.denied(st) || insideState(dir, state, work) {
		return fileRoot{}, ErrNoWorkspace
	}
	return r, nil
}

// insideState reports a directory under the state directory (state) but not under the work root (work), by the
// identity of each of its parents.
func insideState(dir string, state, work fs.FileInfo) bool {
	inState, inWork := false, false
	for d := filepath.Dir(dir); ; d = filepath.Dir(d) {
		if a, err := os.Stat(d); err == nil {
			inState = inState || (state != nil && os.SameFile(a, state))
			inWork = inWork || (work != nil && os.SameFile(a, work))
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	return inState && !inWork
}

// checkDirectory refuses a Ticket's own directory that is the node's state directory or inside it (outside the work
// root): Athena's database, keys and shims are no place for a Ticket to work in.
func (n *Node) checkDirectory(dir string) error {
	if n.cfg.StateDir == "" || dir == "" {
		return nil
	}
	state, err := os.Stat(n.cfg.StateDir)
	if err != nil {
		return nil
	}
	work, _ := os.Stat(n.cfg.WorkRoot)
	if st, err := os.Stat(dir); err == nil && os.SameFile(st, state) || insideState(dir, state, work) {
		return fmt.Errorf("directory %s is Athena's state directory or inside it: choose another directory", dir)
	}
	return nil
}

// statFile is the regular file at rel in root, when it may be shown: no component of rel is a symlink (one could lead
// into .athena, or to another file than a listing showed), its first is not .athena (in any letter case), none is a
// denied directory (r.deny: the state directory, the work root), and it has one hard link (a second name could be of
// something outside the directory).
func statFile(root *os.Root, r fileRoot, rel string) (fs.FileInfo, bool) {
	parts := strings.Split(rel, "/")
	var last fs.FileInfo
	for i := range parts {
		st, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil || st.Mode()&fs.ModeSymlink != 0 || (i == 0 && isAthena(root, parts[0], st)) || r.denied(st) {
			return nil, false
		}
		last = st
	}
	if !last.Mode().IsRegular() || multiLinked(last) {
		return nil, false
	}
	return last, true
}

// openFile opens rel in r through the checks of localPath and statFile, or fails with ErrNotFound. Nothing outside
// r.dir opens (os.Root), and the file opened must be the one checked, not something swapped in since.
func openFile(r fileRoot, rel string) (*os.File, error) {
	rel, ok := localPath(rel)
	if !ok {
		return nil, ErrNotFound
	}
	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	last, ok := statFile(root, r, rel)
	if !ok {
		return nil, ErrNotFound
	}
	return openChecked(root, rel, last)
}

// openChecked opens rel in root when it is still last, the regular file statFile checked. It opens without blocking
// (a FIFO renamed into its place would wait for a writer) and checks the handle itself (fstat).
func openChecked(root *os.Root, rel string, last fs.FileInfo) (*os.File, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || !os.SameFile(st, last) || multiLinked(st) {
		f.Close()
		return nil, ErrNotFound
	}
	return f, nil
}

// TicketFiles lists a Ticket's files. Without a repository: the regular files in its directory, breadth-first, at most
// 1000 files in at most 2000 directories within 10 s (truncated reports a cap was hit), sorted by path. With one: the
// files its worktree added or changed (worktreeFiles). Either way the .athena directory, private paths (privatePath),
// the state directory and the work root (fileRoot), symlinks and files with several hard links are left out.
func (n *Node) TicketFiles(ctx context.Context, ticketID string) ([]FileEntry, bool, error) {
	p, t, err := n.ticketPlacement(ticketID)
	if err != nil {
		return nil, false, err
	}
	if p.repo != "" {
		return n.worktreeFiles(ctx, p, branchName(t))
	}
	r, err := n.fileRoot(p.workDir)
	if err != nil {
		return nil, false, err
	}
	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	deadline := time.Now().Add(walkTimeout)
	files := []FileEntry{}
	capped := false
	finish := func(truncated bool) ([]FileEntry, bool, error) {
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		return files, truncated, nil
	}
	queue := []string{"."}
	queued := 1 // directories queued so far, visited or not
	for dirs := 0; len(queue) > 0; dirs++ {
		if dirs == maxWalkDirs || time.Now().After(deadline) {
			return finish(true)
		}
		cur := queue[0]
		queue = queue[1:]
		d, err := root.Open(cur)
		if err != nil {
			continue // vanished or unreadable: leave it out
		}
		for {
			entries, err := d.ReadDir(walkBatch)
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			for _, e := range entries {
				rel := path.Join(cur, e.Name())
				info, ierr := e.Info()
				if ierr != nil || e.Type()&fs.ModeSymlink != 0 || privatePath(rel) || (cur == "." && isAthena(root, e.Name(), info)) || r.denied(info) {
					continue
				}
				switch {
				case e.IsDir():
					if queued == maxWalkDirs {
						capped = true // this directory is never visited
						continue
					}
					queued++
					queue = append(queue, rel)
				case e.Type().IsRegular() && !multiLinked(info):
					if len(files) == maxFileEntries {
						d.Close()
						return finish(true)
					}
					files = append(files, FileEntry{Path: rel, Size: info.Size(), ModTime: info.ModTime()})
				}
			}
			if err != nil || len(entries) == 0 {
				break // io.EOF, or unreadable
			}
			if time.Now().After(deadline) {
				d.Close()
				return finish(true)
			}
		}
		d.Close()
	}
	return finish(capped)
}

// worktreeFiles lists the files a Ticket's worktree added or changed: those its branch changes against its base (as in
// TicketDiff; a deleted one is not there), and the new files not committed yet that git does not ignore, each as it
// is in the worktree now. At most 1000, sorted by path; truncated reports that the cap was hit or git's output was cut.
// git reads only the repository and the worktree's index and directories, with its git dir pinned (worktreeGitDir):
// no hook, no filter and no fsmonitor runs.
func (n *Node) worktreeFiles(ctx context.Context, p placement, branch string) ([]FileEntry, bool, error) {
	r, err := n.fileRoot(p.workDir)
	if err != nil {
		return nil, false, err
	}
	dir := r.dir
	ctx, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()
	b, err := branchFiles(ctx, p.repo, branch)
	if err != nil {
		return nil, false, err
	}
	status := map[string]string{}
	for _, f := range b.files {
		if f.Status != "D" {
			status[f.Path] = f.Status
		}
	}
	gitDir, err := worktreeGitDir(ctx, p.repo, dir)
	if err != nil {
		return nil, false, err
	}
	// The common dir is pinned too: the worktree's commondir file is writable by the worker, and would otherwise choose
	// the config git reads (gitDir is <common>/worktrees/<name>, worktreeGitDir checked it).
	env := append(diffEnv(), "GIT_DIR="+gitDir, "GIT_COMMON_DIR="+filepath.Dir(filepath.Dir(gitDir)), "GIT_WORK_TREE="+dir)
	others, cut, err := diffGit(ctx, dir, env, maxNumstatSize, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, false, err
	}
	names := strings.Split(string(others), "\x00")
	if cut {
		names = names[:len(names)-1] // the last name may be partial
	}
	for _, name := range names {
		if _, tracked := status[name]; name != "" && !tracked {
			status[name] = "?"
		}
	}
	paths := make([]string, 0, len(status))
	for rel := range status {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	files := []FileEntry{}
	for _, rel := range paths {
		clean, ok := localPath(rel)
		if !ok || clean != rel {
			continue
		}
		info, ok := statFile(root, r, rel)
		if !ok {
			continue // gone since, or not one that may be shown
		}
		if len(files) == maxFileEntries {
			return files, true, nil
		}
		files = append(files, FileEntry{Path: rel, Status: status[rel], Size: info.Size(), ModTime: info.ModTime()})
	}
	return files, b.cut || cut, nil
}

// worktreeGitDir is the git dir of the worktree at dir, found from the repository's side: the entry of
// <common dir>/worktrees whose gitdir file names dir's .git. The worktree's own .git file, which its worker can
// rewrite to point at a git dir of its own (with its own config, exclude file and index), is never read.
func worktreeGitDir(ctx context.Context, repo, dir string) (string, error) {
	common := filepath.Join(repo, ".git")
	if st, err := os.Lstat(common); err != nil || !st.IsDir() {
		// The repository is itself a worktree or uses a gitdir file: ask git where its common dir is.
		out, _, err := diffGit(ctx, repo, diffEnv(), 4<<10, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return "", err
		}
		common = strings.TrimSpace(string(out))
	}
	want, err := os.Stat(dir)
	if err != nil {
		return "", ErrNoWorkspace
	}
	names := []string{filepath.Base(dir)}
	if entries, err := os.ReadDir(filepath.Join(common, "worktrees")); err == nil {
		for _, e := range entries {
			names = append(names, e.Name())
		}
	}
	for _, name := range names {
		admin := filepath.Join(common, "worktrees", name)
		b, err := readSmall(filepath.Join(admin, "gitdir"))
		if err != nil {
			continue
		}
		at := strings.TrimSpace(string(b))
		if !filepath.IsAbs(at) {
			// worktree.useRelativePaths (git 2.48+) records it relative to the admin directory.
			at = filepath.Join(admin, at)
		}
		if st, err := os.Stat(filepath.Dir(at)); err == nil && filepath.Base(at) == ".git" && os.SameFile(st, want) {
			return admin, nil
		}
	}
	return "", fmt.Errorf("no worktree of %s is registered at %s", repo, dir)
}

// readSmall reads a file of git's own bookkeeping, at most 4 KiB of it.
func readSmall(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 4<<10))
}

// OpenTicketFile opens a regular file of a Ticket by its relative path: one in the directory of a Ticket without a
// repository, or one its worktree added or changed (worktreeFiles lists it) in a Ticket with one. Nothing outside that
// directory opens (os.Root), and neither does anything under .athena (in any letter case), a private name, anything
// through a symlink, or with several hard links: ErrNotFound.
func (n *Node) OpenTicketFile(ctx context.Context, ticketID, rel string) (*os.File, error) {
	p, t, err := n.ticketPlacement(ticketID)
	if err != nil {
		return nil, err
	}
	r, err := n.fileRoot(p.workDir)
	if err != nil {
		return nil, err
	}
	if p.repo == "" {
		return openFile(r, rel)
	}
	clean, ok := localPath(rel)
	if !ok {
		return nil, ErrNotFound
	}
	files, _, err := n.worktreeFiles(ctx, p, branchName(t))
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(files, func(f FileEntry) bool { return f.Path == clean }) {
		return nil, ErrNotFound
	}
	return openFile(r, clean)
}
