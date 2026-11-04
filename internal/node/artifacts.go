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
	"sort"
	"strconv"
	"strings"
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
	// Patch is the unified diff, cut at 2 MiB (Truncated).
	Patch     string
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
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
		return nil, false, fmt.Errorf("git %s: %w: %s", args[0], werr, strings.TrimSpace(stderr.String()))
	}
	return out, false, nil
}

// diffEnv is git's environment for reading a Ticket's repository: the node's minimal one (no credentials, no prompt),
// with no hook or fsmonitor of the repository's own.
func diffEnv() []string {
	return withGitConfig(nodeGitBaseEnv(), noHooks)
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
	d := Diff{Base: base, Branch: branch, Files: parseDiffStat(stat), Truncated: statCut}
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
func parseDiffStat(out []byte) []DiffFile {
	toks := strings.Split(string(out), "\x00")
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
	byPath := make(map[string]*DiffFile, len(files))
	for j := range files {
		byPath[files[j].Path] = &files[j]
	}
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
		f := byPath[p]
		if f == nil {
			continue
		}
		if parts[0] == "-" {
			f.Binary = true
			continue
		}
		f.Added, _ = strconv.Atoi(parts[0])
		f.Deleted, _ = strconv.Atoi(parts[1])
	}
	return files
}

// athenaDir is the directory of the node's own files in a Ticket's directory; it is never listed or served.
const athenaDir = ".athena"

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
// 1000 (truncated reports more), sorted by path. The .athena directory and symlinks are left out.
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
	files := []FileEntry{}
	queue := []string{"."}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		d, err := root.Open(cur)
		if err != nil {
			continue // vanished or unreadable: leave it out
		}
		entries, err := d.ReadDir(-1)
		d.Close()
		if err != nil {
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			rel := path.Join(cur, e.Name())
			if cur == "." && e.Name() == athenaDir {
				continue
			}
			switch {
			case e.Type()&fs.ModeSymlink != 0:
			case e.IsDir():
				queue = append(queue, rel)
			case e.Type().IsRegular():
				if len(files) == maxFileEntries {
					sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
					return files, true, nil
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				files = append(files, FileEntry{Path: rel, Size: info.Size(), ModTime: info.ModTime()})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, false, nil
}

// OpenTicketFile opens a regular file of a Ticket placed without a repository, by its relative path. Nothing outside
// the Ticket's directory opens (os.Root), and neither does anything under .athena or through a symlink: ErrNotFound.
func (n *Node) OpenTicketFile(ticketID, rel string) (*os.File, error) {
	dir, err := n.ticketRoot(ticketID)
	if err != nil {
		return nil, err
	}
	if rel == "" || strings.ContainsRune(rel, 0) || strings.HasPrefix(rel, "/") || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return nil, ErrNotFound
	}
	rel = path.Clean(rel)
	if rel == athenaDir || strings.HasPrefix(rel, athenaDir+"/") {
		return nil, ErrNotFound
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// No component may be a symlink (one could lead into .athena, or to another file than the listing showed).
	parts := strings.Split(rel, "/")
	for i := range parts {
		st, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil || st.Mode()&fs.ModeSymlink != 0 {
			return nil, ErrNotFound
		}
		if i == len(parts)-1 && !st.Mode().IsRegular() {
			return nil, ErrNotFound
		}
	}
	f, err := root.Open(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, ErrNotFound
	}
	return f, nil
}
