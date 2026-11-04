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

// FileEntry is a file in a Ticket's directory; Path is relative, with forward slashes.
type FileEntry struct {
	Path    string
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
	env := withGitConfig(nodeGitBaseEnv(), append(slices.Clone(noHooks), [][2]string{{"protocol.allow", "never"}, {"credential.helper", ""}, {"advice.graftFileDeprecated", "false"}}...))
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
	env := diffEnv()
	git := func(limit int, args ...string) ([]byte, bool, error) {
		return diffGit(ctx, p.repo, env, limit, args...)
	}
	text := func(args ...string) (string, error) {
		out, _, err := git(1<<10, args...)
		return strings.TrimSpace(string(out)), err
	}
	ref := "refs/heads/" + branch
	if _, err := text("rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		if ctx.Err() != nil {
			return Diff{}, ctx.Err()
		}
		return Diff{}, ErrNoWorkspace
	}
	base, err := text("merge-base", "refs/remotes/origin/HEAD", ref)
	if err != nil {
		if ctx.Err() != nil {
			return Diff{}, ctx.Err()
		}
		if base, err = text("merge-base", "HEAD", ref); err != nil {
			return Diff{}, err
		}
	}
	span := base + "..." + ref
	common := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--find-renames"}
	// --raw gives each file's status and --numstat its counts, in one run; -z keeps odd file names exact.
	stat, statCut, err := git(maxNumstatSize, append(common[:len(common):len(common)], "--raw", "--numstat", "-z", span)...)
	if err != nil {
		return Diff{}, err
	}
	d := Diff{Base: base, Branch: branch, Files: parseDiffStat(stat, statCut), Truncated: statCut}
	patch, cut, err := git(maxPatchBytes, append(common[:len(common):len(common)], span)...)
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

// ticketRoot is the directory of a Ticket placed without a repository.
func (n *Node) ticketRoot(ticketID string) (string, error) {
	p, _, err := n.ticketPlacement(ticketID)
	if err != nil {
		return "", err
	}
	if p.repo != "" {
		return "", ErrNoWorkspace
	}
	if st, err := os.Stat(p.workDir); err != nil || !st.IsDir() {
		return "", ErrNoWorkspace
	}
	return p.workDir, nil
}

// TicketFiles lists the regular files in the directory of a Ticket placed without a repository: breadth-first, at most
// 1000 files in at most 2000 directories within 10 s (truncated reports a cap was hit), sorted by path. The .athena
// directory, symlinks and files with several hard links are left out.
func (n *Node) TicketFiles(ticketID string) ([]FileEntry, bool, error) {
	dir, err := n.ticketRoot(ticketID)
	if err != nil {
		return nil, false, err
	}
	root, err := os.OpenRoot(dir)
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
				if ierr != nil || e.Type()&fs.ModeSymlink != 0 || (cur == "." && isAthena(root, e.Name(), info)) {
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

// OpenTicketFile opens a regular file of a Ticket placed without a repository, by its relative path. Nothing outside
// the Ticket's directory opens (os.Root), and neither does anything under .athena (in any letter case), through a
// symlink, or with several hard links: ErrNotFound.
func (n *Node) OpenTicketFile(ticketID, rel string) (*os.File, error) {
	dir, err := n.ticketRoot(ticketID)
	if err != nil {
		return nil, err
	}
	if rel == "" || strings.ContainsRune(rel, 0) || strings.HasPrefix(rel, "/") || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return nil, ErrNotFound
	}
	rel = path.Clean(rel)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// No component may be a symlink (one could lead into .athena, or to another file than the listing showed).
	parts := strings.Split(rel, "/")
	var last fs.FileInfo
	for i := range parts {
		st, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil || st.Mode()&fs.ModeSymlink != 0 || (i == 0 && isAthena(root, parts[0], st)) {
			return nil, ErrNotFound
		}
		last = st
	}
	if !last.Mode().IsRegular() || multiLinked(last) {
		return nil, ErrNotFound
	}
	f, err := root.Open(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	// It must be the file just checked, not something swapped in since.
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || !os.SameFile(st, last) || multiLinked(st) {
		f.Close()
		return nil, ErrNotFound
	}
	return f, nil
}
