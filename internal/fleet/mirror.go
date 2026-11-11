package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/nodelink"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// A node checkpoints a Ticket's work in progress at every turn end (internal/node/checkpoint.go) and ships the
// checkpoint here, as a git bundle after the one it sent before: a remote node in BlobChunks on its link's BLOB channel
// then the CheckpointSent fact, the embedded node straight from its repository. The server keeps the latest of each
// Ticket in a bare mirror per repository, at the same ref the node keeps it, so "Retry elsewhere" can restore the work
// on another node (Bundle). A daily sweep drops the checkpoints of Tickets that ended more than MirrorKeep ago.

const (
	// MirrorKeep is how long the checkpoint of an ended Ticket stays in the mirror (there is no signal of its pull
	// request closing).
	MirrorKeep = 14 * 24 * time.Hour
	// MirrorSweepEvery is how often the sweep runs; the first runs mirrorSweepFirst after the Fleet starts.
	MirrorSweepEvery = 24 * time.Hour
	mirrorSweepFirst = time.Minute
	// mirrorTimeout bounds one git call on a mirror.
	mirrorTimeout = 5 * time.Minute
	// compactTimeout bounds a repack or prune of one mirror repository, which a large one needs.
	compactTimeout = 30 * time.Minute
	// blobKindCheckpoint is BlobChunk.kind of a checkpoint bundle.
	blobKindCheckpoint = "checkpoint"
	// maxTransfers bounds the checkpoint transfers one link holds at once (a Ticket's queue behind one another), so
	// with maxBundle it bounds the disk one link takes.
	maxTransfers = 4
	// blobWindow is the window of a node's blob budget (blobBudget).
	blobWindow = time.Hour
	// checkpointKey prefixes the store's record of each Ticket's checkpoint in the mirror.
	checkpointKey = "fleet:checkpoint:"
)

// maxBundle is the largest checkpoint bundle the mirror takes: node.MaxCheckpointBundle; a variable for tests.
var maxBundle int64 = node.MaxCheckpointBundle

// blobBudget is how many checkpoint bytes one node may send in a blobWindow; past it, its transfers are refused, so a
// node cannot fill the server's disk on purpose. A variable for tests.
var blobBudget int64 = 4 * node.MaxCheckpointBundle

// CheckpointDir is the checkpoint mirror's root under the server's state directory: a directory of its own, apart
// from the embedded node's GitHub mirrors (node.MirrorDir), which its worktrees fetch from.
func CheckpointDir(stateDir string) string { return filepath.Join(stateDir, "checkpoints") }

// ErrNoCheckpoint is a Ticket the mirror holds no checkpoint of.
var ErrNoCheckpoint = node.ErrNoCheckpoint

// mirrorTicketRe is a Ticket id, lowercased, the mirror names a ref and a record after: ids that differ only in case
// are one Ticket to the mirror.
var mirrorTicketRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// mirrorConfig is the whole config of a mirror repository, written before each git call on it: nothing anyone wrote
// there applies.
const mirrorConfig = "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n"

// mirrorPins are the command-scope settings of every git call on a mirror, besides node.RunGit's (no hook, no
// fsmonitor): only the bundle file is fetched from, every object is checked, and nothing runs in the background.
var mirrorPins = []string{"-c", "protocol.allow=never", "-c", "protocol.file.allow=always",
	"-c", "transfer.fsckObjects=true", "-c", "fetch.fsckObjects=true", "-c", "gc.auto=0", "-c", "maintenance.auto=false",
	"-c", "credential.helper="}

// Mirror is the server's bare repositories of the nodes' checkpoints: <dir>/<owner>/<name>.git, lower-case, each
// Ticket's at refs/athena/<lowercased id>/checkpoint. Its git runs with no shell, no global or system config, and the
// node's pins and timeouts.
type Mirror struct {
	dir string

	mu    sync.Mutex
	locks map[string]*sync.Mutex // by repository directory
}

// NewMirror returns the mirror rooted at dir (CheckpointDir); its repositories are made on first use. The bundles a
// previous run left in its temporary directory are removed.
func NewMirror(dir string) *Mirror {
	_ = os.RemoveAll(filepath.Join(dir, mirrorTmp))
	return &Mirror{dir: dir, locks: map[string]*sync.Mutex{}}
}

// mirrorTmp is the mirror's directory of bundles on their way in; no repository has its name (repoDir).
const mirrorTmp = ".tmp"

// tempFile makes a temporary file in the mirror's own temporary directory.
func (m *Mirror) tempFile(pattern string) (*os.File, error) {
	dir := filepath.Join(m.dir, mirrorTmp)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, pattern)
}

// repoDir is repo's (owner/name) bare repository; a name that is not owner/name is refused.
func (m *Mirror) repoDir(repo string) (string, error) {
	if err := node.CheckRepo(repo); err != nil {
		return "", err
	}
	owner, name, _ := strings.Cut(strings.ToLower(repo), "/")
	if strings.HasPrefix(owner, "-") || strings.HasPrefix(name, "-") || strings.HasPrefix(owner, ".") || strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("repository %q is not owner/name", repo)
	}
	return filepath.Join(m.dir, owner, name+".git"), nil
}

// mirrorRef is the Ticket's ref; an id that is not a plain name is refused.
func mirrorRef(ticketID string) (string, error) {
	id := strings.ToLower(ticketID)
	if !mirrorTicketRe.MatchString(id) {
		return "", fmt.Errorf("ticket id %q is not a plain name", ticketID)
	}
	return node.CheckpointRef(id), nil
}

// lock serializes the mirror's git calls on one repository.
func (m *Mirror) lock(dir string) func() {
	m.mu.Lock()
	l := m.locks[dir]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[dir] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// mirrorEnv is the environment of git on the repository dir: the process's path, home and temp directory and nothing
// else of it, no system or global config, and the repository named by GIT_DIR (no discovery).
func mirrorEnv(dir string) []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "LANG"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if dir != "" {
		env = append(env, "GIT_DIR="+dir)
	}
	return env
}

// mirrorGit runs git on the repository dir with the mirror's environment and pins, bounded by mirrorTimeout.
func mirrorGit(ctx context.Context, dir string, args ...string) (string, error) {
	return mirrorGitFor(ctx, dir, mirrorTimeout, args...)
}

// mirrorGitFor is mirrorGit bounded by timeout.
func mirrorGitFor(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := writeConfig(dir); err != nil {
		return "", err
	}
	return node.RunGit(ctx, dir, mirrorEnv(dir), append(append([]string{}, mirrorPins...), args...)...)
}

// writeConfig replaces the repository's config with mirrorConfig, atomically.
func writeConfig(dir string) error {
	f, err := os.CreateTemp(dir, "config.athena-*")
	if err != nil {
		return err
	}
	if _, err := f.WriteString(mirrorConfig); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, "config")); err != nil {
		os.Remove(f.Name())
		return err
	}
	return nil
}

// ensure makes the bare repository dir unless it has one.
func ensure(ctx context.Context, dir string) error {
	if st, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil && st.Mode().IsRegular() {
		return nil
	}
	_ = os.RemoveAll(dir) // a repository that died half-way starts over
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, mirrorTimeout)
	defer cancel()
	if _, err := node.RunGit(ctx, filepath.Dir(dir), mirrorEnv(""), "init", "--quiet", "--bare", "--template=", dir); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("make the mirror of %s: %w", filepath.Base(dir), err)
	}
	return os.Chmod(dir, 0o700)
}

// Receive fetches the Ticket's checkpoint from bundle (a git bundle of its ref, whole or after a commit the mirror
// has) into repo's mirror, made bare on first use. Only refs/athena/<id>/checkpoint is taken from the bundle, whatever
// else it holds; a bundle over 512 MiB, or one git will not take, is refused and changes nothing.
func (m *Mirror) Receive(repo, ticketID string, bundle io.Reader) error {
	return m.ReceiveContext(context.Background(), repo, ticketID, bundle)
}

// ReceiveContext is Receive bounded by ctx.
func (m *Mirror) ReceiveContext(ctx context.Context, repo, ticketID string, bundle io.Reader) error {
	dir, err := m.repoDir(repo)
	if err != nil {
		return err
	}
	ref, err := mirrorRef(ticketID)
	if err != nil {
		return err
	}
	f, err := m.tempFile("athena-checkpoint-*.bundle")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	n, err := io.Copy(f, io.LimitReader(bundle, maxBundle+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > maxBundle {
		return fmt.Errorf("the checkpoint bundle is over %d MiB", maxBundle>>20)
	}
	return m.receivePath(ctx, dir, ref, f.Name())
}

// receiveHook, when set (tests), runs before a node's bundle goes into the mirror.
var receiveHook func(ticketID string)

// receiveFile is ReceiveContext from a bundle file already held, at most maxBundle long.
func (m *Mirror) receiveFile(ctx context.Context, repo, ticketID, file string) error {
	dir, err := m.repoDir(repo)
	if err != nil {
		return err
	}
	ref, err := mirrorRef(ticketID)
	if err != nil {
		return err
	}
	return m.receivePath(ctx, dir, ref, file)
}

// receivePath fetches ref from the bundle file into the repository dir.
func (m *Mirror) receivePath(ctx context.Context, dir, ref, file string) error {
	defer m.lock(dir)()
	if err := ensure(ctx, dir); err != nil {
		return err
	}
	// Only the mirror writes this ref, under the lock: a lock file left there is a git call the timeout or the link's
	// end killed, which would refuse every later update.
	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(ref)+".lock")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// The ref is forced: each checkpoint is a sibling of the last, on the branch's head.
	_, err := mirrorGit(ctx, dir, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-auto-gc",
		"--no-recurse-submodules", file, "+"+ref+":"+ref)
	return err
}

// Head is the commit of the Ticket's checkpoint in repo's mirror; ok is false when there is none.
func (m *Mirror) Head(repo, ticketID string) (sha string, ok bool, err error) {
	dir, err := m.repoDir(repo)
	if err != nil {
		return "", false, err
	}
	ref, err := mirrorRef(ticketID)
	if err != nil {
		return "", false, err
	}
	defer m.lock(dir)()
	return head(dir, ref)
}

func head(dir, ref string) (string, bool, error) {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		return "", false, nil
	}
	out, err := mirrorGit(context.Background(), dir, "for-each-ref", "--format=%(objectname)", ref)
	if err != nil {
		return "", false, err
	}
	return out, out != "", nil
}

// shaRe is a full commit name, SHA-1 or SHA-256.
var shaRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// Has reports whether the commit sha is in repo's mirror (git cat-file -e): a child's head the server can ship, or the
// lead's commit a fork seed starts at. A mirror not made yet has none.
func (m *Mirror) Has(repo, sha string) (bool, error) {
	dir, err := m.repoDir(repo)
	if err != nil {
		return false, err
	}
	if !shaRe.MatchString(sha) {
		return false, fmt.Errorf("%q is not a commit name", sha)
	}
	defer m.lock(dir)()
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		return false, nil
	}
	if _, err := mirrorGit(context.Background(), dir, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return false, nil // git says so with its exit status: not there
	}
	return true, nil
}

// Bundle writes a git bundle of the Ticket's whole checkpoint ref to w and returns its commit: what a node fetches
// into its clone to restore the work. ErrNoCheckpoint: the mirror holds none. The bundle streams from git to w, with
// no copy of its own; on an error w may hold part of one. The repository stays locked while w takes it: a caller that
// sends it on (a node's link) writes it to a file first (tempFile).
func (m *Mirror) Bundle(repo, ticketID string, w io.Writer) (sha string, err error) {
	dir, err := m.repoDir(repo)
	if err != nil {
		return "", err
	}
	ref, err := mirrorRef(ticketID)
	if err != nil {
		return "", err
	}
	defer m.lock(dir)()
	if _, ok, err := head(dir, ref); err != nil {
		return "", err
	} else if !ok {
		return "", ErrNoCheckpoint
	}
	ctx, cancel := context.WithTimeout(context.Background(), mirrorTimeout)
	defer cancel()
	if err := writeConfig(dir); err != nil {
		return "", err
	}
	args := append(append([]string{}, mirrorPins...), "bundle", "create", "-", ref)
	return node.StreamBundle(ctx, dir, mirrorEnv(dir), ref, &capWriter{w: w, left: maxBundle}, args)
}

// capWriter passes at most left bytes to w; past them it fails with ErrCheckpointTooBig.
type capWriter struct {
	w    io.Writer
	left int64
}

func (c *capWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > c.left {
		return 0, node.ErrCheckpointTooBig
	}
	c.left -= int64(len(p))
	return c.w.Write(p)
}

// Delete removes the Ticket's checkpoint ref from repo's mirror; none is not an error.
func (m *Mirror) Delete(repo, ticketID string) error {
	dir, err := m.repoDir(repo)
	if err != nil {
		return err
	}
	ref, err := mirrorRef(ticketID)
	if err != nil {
		return err
	}
	return m.deleteRef(dir, ref)
}

func (m *Mirror) deleteRef(dir, ref string) error {
	defer m.lock(dir)()
	if _, ok, err := head(dir, ref); err != nil || !ok {
		return err
	}
	_, err := mirrorGit(context.Background(), dir, "update-ref", "-d", ref)
	return err
}

// repos lists the mirror's repositories.
func (m *Mirror) repos() ([]string, error) {
	dirs, err := filepath.Glob(filepath.Join(m.dir, "*", "*.git"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range dirs {
		if !strings.HasPrefix(filepath.Base(filepath.Dir(d)), ".") {
			out = append(out, d)
		}
	}
	return out, nil
}

// mirrorRefInfo is a checkpoint ref in a repository: its lowercased Ticket id and its commit's date.
type mirrorRefInfo struct {
	dir, ref, id string
	at           time.Time
}

// refs lists the checkpoint refs of the repository dir.
func (m *Mirror) refs(dir string) ([]mirrorRefInfo, error) {
	defer m.lock(dir)()
	out, err := mirrorGit(context.Background(), dir, "for-each-ref", "--format=%(refname) %(committerdate:unix)", "refs/athena/")
	if err != nil || out == "" {
		return nil, err
	}
	var refs []mirrorRefInfo
	for _, line := range strings.Split(out, "\n") {
		ref, unix, _ := strings.Cut(line, " ")
		id, ok := strings.CutPrefix(ref, "refs/athena/")
		if id, ok = strings.CutSuffix(id, "/checkpoint"); !ok || !mirrorTicketRe.MatchString(id) {
			continue
		}
		sec, err := strconv.ParseInt(unix, 10, 64)
		if err != nil {
			continue
		}
		refs = append(refs, mirrorRefInfo{dir: dir, ref: ref, id: id, at: time.Unix(sec, 0)})
	}
	return refs, nil
}

// Compact repacks each repository and prunes what no ref reaches any more (the checkpoints replaced or deleted), under
// its lock: the mirror's disk follows the checkpoints it keeps.
func (m *Mirror) Compact() error {
	dirs, err := m.repos()
	if err != nil {
		return err
	}
	var errs []error
	for _, dir := range dirs {
		errs = append(errs, func() error {
			defer m.lock(dir)()
			if _, err := mirrorGitFor(context.Background(), dir, compactTimeout, "repack", "-a", "-d", "-q"); err != nil {
				return err
			}
			_, err := mirrorGitFor(context.Background(), dir, compactTimeout, "prune", "--expire=now")
			return err
		}())
	}
	return errors.Join(errs...)
}

// Checkpoint is the server's record of a Ticket's checkpoint in the mirror: the commit, the node it came from, when.
type Checkpoint struct {
	TicketID string    `json:"ticketId"`
	Repo     string    `json:"repo"`
	SHA      string    `json:"sha"`
	Node     string    `json:"node"`
	At       time.Time `json:"at"`
}

// Checkpoint is the Ticket's latest checkpoint in the mirror; ok is false when the server has none.
func (f *Fleet) Checkpoint(ticketID string) (c Checkpoint, ok bool, err error) {
	v, ok, err := f.cfg.Store.KVGet(checkpointKey + strings.ToLower(ticketID))
	if err != nil || !ok {
		return Checkpoint{}, false, err
	}
	if err := json.Unmarshal([]byte(v), &c); err != nil {
		return Checkpoint{}, false, err
	}
	return c, true, nil
}

// recordCheckpoint records the Ticket's checkpoint, and wakes what waits for a commit to reach the mirror
// (branches.go).
func (f *Fleet) recordCheckpoint(c Checkpoint) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := f.cfg.Store.KVSet(checkpointKey+strings.ToLower(c.TicketID), string(b)); err != nil {
		return err
	}
	f.mirrorChanged()
	return nil
}

// checkpointSent applies a node's CheckpointSent: the Ticket is placed on the node and works in repo, and its bundle,
// which came before the fact, put the mirror's ref at sha. Anything else is refused (a UserError: the same fact would be
// refused again), and the node sends the whole ref next time.
//
// landed are the commits this link's bundles of the Ticket put in the mirror before the fact: a later bundle of the
// Ticket may already have moved the ref past sha, which still counts as received.
func (f *Fleet) checkpointSent(eng *core.Engine, nodeID, ticketID, repo, sha string, landed []string) error {
	t, _, err := eng.Ticket(ticketID)
	if err != nil {
		return err
	}
	if !f.latestOn(t, nodeID) {
		return errNotOnNode(ticketID)
	}
	if t.Repo == "" || !strings.EqualFold(t.Repo, repo) {
		return &core.UserError{Msg: "ticket " + ticketID + " does not work in " + repo}
	}
	if f.cfg.Mirror == nil {
		return &core.UserError{Msg: "the server keeps no checkpoints"}
	}
	got, ok, err := f.cfg.Mirror.Head(t.Repo, t.ID)
	if err != nil {
		return &core.UserError{Msg: "read the mirror: " + err.Error(), Err: err}
	}
	if !ok || got != sha && !slices.Contains(landed, sha) {
		return &core.UserError{Msg: "the checkpoint's bundle did not reach the server"}
	}
	return f.recordCheckpoint(Checkpoint{TicketID: t.ID, Repo: t.Repo, SHA: sha, Node: nodeID, At: f.now()})
}

// latestOn reports whether the Ticket's latest Assignment is placed on the node: only that node's checkpoints count,
// so one that comes late from a node the Ticket left never moves the mirror's ref back.
func (f *Fleet) latestOn(t core.Ticket, nodeID string) bool {
	a := t.LatestAssignment()
	return a != nil && (a.Node == nodeID || a.Node == "" && nodeID == f.cfg.LocalID)
}

// LocalCheckpoint is the embedded node's node.Config.OnCheckpoint: the checkpoint goes into the mirror, after the one
// there, from a goroutine of the Fleet's. It never blocks; a burst keeps the Ticket's latest.
func (f *Fleet) LocalCheckpoint(ticketID, repo, ref, sha string) {
	if f.cfg.Mirror == nil {
		return
	}
	f.ckMu.Lock()
	f.ckLocal[ticketID] = repo
	f.ckMu.Unlock()
	signal(f.ckWake)
}

// localCheckpoints saves the embedded node's checkpoints into the mirror until the Fleet closes.
func (f *Fleet) localCheckpoints() {
	defer f.bg.Done()
	for {
		select {
		case <-f.ctx.Done():
			return
		case <-f.ckWake:
		}
		f.ckMu.Lock()
		todo := f.ckLocal
		f.ckLocal = map[string]string{}
		f.ckMu.Unlock()
		for id, repo := range todo {
			if err := f.saveLocal(f.ctx, id, repo); err != nil && f.ctx.Err() == nil {
				f.log.Warn("keep the local checkpoint", "ticket", id, "error", err)
			}
		}
	}
}

// saveLocal brings the mirror's checkpoint of the local node's Ticket up to the node's: a bundle after the mirror's
// commit, or the whole ref when that fails, and records it.
func (f *Fleet) saveLocal(ctx context.Context, ticketID, repo string) error {
	eng := f.engine()
	if f.localBundle == nil || eng == nil {
		return nil
	}
	t, _, err := eng.Ticket(ticketID)
	if err != nil {
		return err
	}
	if !f.latestOn(t, f.cfg.LocalID) {
		return nil // the Ticket moved to another node: its checkpoints count from there
	}
	if f.localTooBig[strings.ToLower(ticketID)] {
		return nil
	}
	m := f.cfg.Mirror
	base, _, err := m.Head(repo, ticketID)
	if err != nil {
		return err
	}
	sha, err := f.pipeLocal(ctx, ticketID, repo, base)
	if err != nil && base != "" && !errors.Is(err, node.ErrCheckpointUnchanged) && !errors.Is(err, node.ErrCheckpointTooBig) &&
		ctx.Err() == nil {
		sha, err = f.pipeLocal(ctx, ticketID, repo, "")
	}
	if errors.Is(err, node.ErrCheckpointUnchanged) {
		err = nil
	}
	if errors.Is(err, node.ErrCheckpointTooBig) {
		f.localTooBig[strings.ToLower(ticketID)] = true // localCheckpoints' alone
		f.log.Warn("the local checkpoint is too big for the mirror; no more are kept for this ticket", "ticket", ticketID)
		return nil
	}
	if err != nil {
		return err
	}
	got, ok, err := m.Head(repo, ticketID)
	if err != nil {
		return err
	}
	if !ok || got != sha {
		return fmt.Errorf("the mirror holds %q after the bundle of %s", got, sha)
	}
	if c, ok, err := f.Checkpoint(ticketID); err == nil && ok && c.SHA == sha {
		return nil
	}
	return f.recordCheckpoint(Checkpoint{TicketID: ticketID, Repo: repo, SHA: sha, Node: f.cfg.LocalID, At: f.now()})
}

// pipeLocal streams the local node's bundle after since into the mirror.
func (f *Fleet) pipeLocal(ctx context.Context, ticketID, repo, since string) (string, error) {
	pr, pw := io.Pipe()
	type result struct {
		sha string
		err error
	}
	done := make(chan result, 1)
	go func() {
		sha, err := f.localBundle(ctx, ticketID, since, pw)
		pw.CloseWithError(err)
		done <- result{sha, err}
	}()
	rerr := f.cfg.Mirror.ReceiveContext(ctx, repo, ticketID, pr)
	pr.CloseWithError(errors.New("the mirror stopped reading"))
	r := <-done
	if r.err != nil {
		return r.sha, r.err
	}
	return r.sha, rerr
}

// sweepLoop runs SweepMirror once soon after the Fleet starts, then daily, until it closes.
func (f *Fleet) sweepLoop() {
	defer f.bg.Done()
	t := time.NewTimer(mirrorSweepFirst)
	defer t.Stop()
	for {
		select {
		case <-f.ctx.Done():
			return
		case <-t.C:
		}
		if f.engine() != nil {
			if err := f.SweepMirror(); err != nil {
				f.log.Warn("sweep the checkpoint mirror", "error", err)
			}
		}
		t.Reset(MirrorSweepEvery)
	}
}

// SweepMirror deletes from the mirror, with their records, the checkpoints of Tickets that ended MirrorKeep ago or
// more, or that the Engine no longer knows; then the refs no record names (a bundle whose fact was refused or never
// applied) whose commit is MirrorKeep old and whose Ticket ended or is unknown; then compacts each repository. A
// child's checkpoint stays while its lead is open: it is the branch the server ships to the lead's node.
func (f *Fleet) SweepMirror() error {
	eng := f.engine()
	if eng == nil || f.cfg.Mirror == nil {
		return nil
	}
	recs, err := f.cfg.Store.KVList(checkpointKey)
	if err != nil {
		return err
	}
	now := f.now()
	var errs []error
	kept := map[string]bool{} // lowercased ids of the records that stay
	for key, v := range recs {
		var c Checkpoint
		if err := json.Unmarshal([]byte(v), &c); err != nil || c.TicketID == "" {
			errs = append(errs, f.cfg.Store.KVDelete(key))
			continue
		}
		t, _, err := eng.Ticket(c.TicketID)
		var ue *core.UserError
		switch {
		case errors.As(err, &ue): // a Ticket the Engine does not know
		case err != nil:
			errs = append(errs, err)
			kept[strings.ToLower(c.TicketID)] = true
			continue
		case t.Ended == nil || now.Sub(t.Ended.At) < MirrorKeep || leadOpen(eng, t):
			kept[strings.ToLower(c.TicketID)] = true
			continue
		}
		if err := f.cfg.Mirror.Delete(c.Repo, c.TicketID); err != nil {
			errs = append(errs, err)
			kept[strings.ToLower(c.TicketID)] = true
			continue
		}
		errs = append(errs, f.cfg.Store.KVDelete(key))
		f.log.Info("dropped an ended ticket's checkpoint from the mirror", "ticket", c.TicketID, "repo", c.Repo)
	}
	errs = append(errs, f.sweepOrphans(eng, kept, now), f.cfg.Mirror.Compact())
	return errors.Join(errs...)
}

// leadOpen reports whether t is a child whose lead has not ended.
func leadOpen(eng *core.Engine, t core.Ticket) bool {
	if t.Lead == "" {
		return false
	}
	lead, _, err := eng.Ticket(t.Lead)
	return err == nil && lead.Ended == nil
}

// sweepOrphans deletes the checkpoint refs no record names, MirrorKeep old by their commit, whose Ticket ended or is
// unknown.
func (f *Fleet) sweepOrphans(eng *core.Engine, kept map[string]bool, now time.Time) error {
	m := f.cfg.Mirror
	dirs, err := m.repos()
	if err != nil {
		return err
	}
	var tickets map[string]core.Ticket // by lowercased id, read once when an orphan is old enough
	var errs []error
	for _, dir := range dirs {
		refs, err := m.refs(dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, r := range refs {
			if kept[r.id] || now.Sub(r.at) < MirrorKeep {
				continue
			}
			if tickets == nil {
				all, err := eng.Tickets(true)
				if err != nil {
					return errors.Join(append(errs, err)...)
				}
				tickets = map[string]core.Ticket{}
				for _, t := range all {
					tickets[strings.ToLower(t.ID)] = t
				}
			}
			if t, ok := tickets[r.id]; ok && t.Ended == nil {
				continue
			} else if lead, open := tickets[strings.ToLower(t.Lead)]; ok && t.Lead != "" && open && lead.Ended == nil {
				continue // a child's branch, while its lead is open
			}
			if err := m.deleteRef(r.dir, r.ref); err != nil {
				errs = append(errs, err)
				continue
			}
			f.log.Info("dropped an unrecorded checkpoint from the mirror", "ref", r.ref, "repo", filepath.Base(dir))
		}
	}
	return errors.Join(errs...)
}

// transfer is one checkpoint bundle a node sends on its link: its chunks go into a temporary file, which goes into
// the mirror once the last one came.
type transfer struct {
	ticketID string
	file     *os.File // nil once refused
	size     int64
	refused  bool          // its chunks are dropped until its last
	running  bool          // the last chunk came: the bundle goes into the mirror
	done     chan struct{} // closed once it is in the mirror, refused or dropped
	// after is the done of the Ticket's transfer that came before it on the link: the fetches go in order.
	after chan struct{}
	// landed is the commit the mirror's ref was at once this bundle went in ("" if it did not); set before done.
	landed string
	seq    uint64 // order of arrival on the link
}

// onChunk takes one BlobChunk, on the link's reader goroutine: it only appends to the transfer's file. A transfer of a
// kind the server does not take, or over the bound, is dropped; the last chunk hands the file to receive.
func (lk *nodeLink) onChunk(c *athenav1.BlobChunk) {
	if !lk.welcomed.Load() {
		lk.end(link.CloseProtocol, "a blob before the welcome")
		return
	}
	id := c.GetTransferId()
	lk.mu.Lock()
	tr := lk.transfers[id]
	if tr == nil {
		if lk.ended || id == "" || len(lk.transfers) >= maxTransfers {
			lk.mu.Unlock()
			lk.f.log.Warn("node blob dropped", "node", lk.nodeID, "transfer", id, "ticket", c.GetTicketId())
			return
		}
		lk.tseq++
		tr = &transfer{ticketID: c.GetTicketId(), done: make(chan struct{}), seq: lk.tseq}
		// A Ticket's transfers queue behind one another: a turn end's checkpoint may come while the last is fetched.
		var prev *transfer
		for _, o := range lk.transfers {
			if strings.EqualFold(o.ticketID, tr.ticketID) && (prev == nil || o.seq > prev.seq) {
				prev = o
			}
		}
		if prev != nil {
			tr.after = prev.done
		}
		if c.GetKind() != blobKindCheckpoint || lk.f.cfg.Mirror == nil {
			tr.refused = true
			lk.f.log.Warn("node blob refused", "node", lk.nodeID, "kind", c.GetKind(), "ticket", tr.ticketID)
		} else if f, err := lk.f.cfg.Mirror.tempFile("athena-node-checkpoint-*.bundle"); err != nil {
			tr.refused = true
			lk.f.log.Error("hold a node's checkpoint", "node", lk.nodeID, "error", err)
		} else {
			tr.file = f
		}
		lk.transfers[id] = tr
	}
	lk.mu.Unlock()
	if !tr.refused {
		var why string
		switch {
		case c.GetTicketId() != tr.ticketID:
			why = "a chunk names another ticket"
		case tr.size+int64(len(c.GetData())) > maxBundle:
			why = fmt.Sprintf("over %d MiB", maxBundle>>20)
		case !lk.r.spend(int64(len(c.GetData())), lk.f.now()):
			why = fmt.Sprintf("the node sent over %d MiB of checkpoints within %s", blobBudget>>20, blobWindow)
		default:
			if _, err := tr.file.Write(c.GetData()); err != nil {
				why = err.Error()
			}
			tr.size += int64(len(c.GetData()))
		}
		if why != "" {
			lk.f.log.Warn("node checkpoint dropped", "node", lk.nodeID, "ticket", tr.ticketID, "why", why)
			lk.mu.Lock()
			tr.refused = true
			tr.discard()
			lk.mu.Unlock()
		}
	}
	if !c.GetLast() {
		return
	}
	lk.mu.Lock()
	if tr.refused {
		delete(lk.transfers, id)
		close(tr.done)
		lk.mu.Unlock()
		return
	}
	tr.running = true
	lk.wg.Add(1)
	lk.mu.Unlock()
	go lk.receive(id, tr)
}

// discard closes and removes the transfer's file; the caller holds lk.mu.
func (tr *transfer) discard() {
	if tr.file != nil {
		_ = tr.file.Close()
		_ = os.Remove(tr.file.Name())
		tr.file = nil
	}
}

// receive fetches a transfer's bundle into the mirror, for a Ticket with a Repo placed on the node only.
func (lk *nodeLink) receive(id string, tr *transfer) {
	defer lk.wg.Done()
	defer func() {
		lk.mu.Lock()
		tr.discard()
		delete(lk.transfers, id)
		close(tr.done)
		lk.mu.Unlock()
	}()
	log := lk.f.log.With("node", lk.nodeID, "ticket", tr.ticketID)
	if tr.after != nil {
		select {
		case <-tr.after:
		case <-lk.ctx.Done():
			return
		}
	}
	if receiveHook != nil {
		receiveHook(tr.ticketID)
	}
	t, _, err := lk.f.engine().Ticket(tr.ticketID)
	switch {
	case err != nil:
		log.Warn("node checkpoint refused", "error", err)
		return
	case !lk.f.latestOn(t, lk.nodeID):
		log.Warn("node checkpoint refused: the ticket is not on this node")
		return
	case t.Repo == "":
		log.Warn("node checkpoint refused: the ticket works in no repository")
		return
	}
	if err := tr.file.Sync(); err != nil {
		log.Error("hold a node's checkpoint", "error", err)
		return
	}
	if err := lk.f.cfg.Mirror.receiveFile(lk.ctx, t.Repo, t.ID, tr.file.Name()); err != nil {
		if lk.ctx.Err() == nil {
			log.Warn("node checkpoint not taken into the mirror", "error", err)
		}
		return
	}
	if sha, ok, err := lk.f.cfg.Mirror.Head(t.Repo, t.ID); err == nil && ok {
		lk.mu.Lock()
		tr.landed = sha
		lk.mu.Unlock()
	}
	log.Debug("node checkpoint in the mirror", "bytes", tr.size)
}

// dropTransfers drops the transfers whose last chunk never came, once the link's reader is done.
func (lk *nodeLink) dropTransfers() {
	lk.mu.Lock()
	defer lk.mu.Unlock()
	for id, tr := range lk.transfers {
		if tr.running {
			continue // receive ends with the link's context
		}
		tr.discard()
		delete(lk.transfers, id)
		close(tr.done)
	}
}

// factTransfers notes, as a CheckpointSent fact arrives on the reader goroutine, the Ticket's transfers that came
// before it on the link: its own bundle is among them.
func (lk *nodeLink) factTransfers(call *athenav1.NodeCall) {
	var a nodelink.ArgsCheckpointSent
	if decodeStrict(call.GetArgs(), &a) != nil {
		return
	}
	var trs []*transfer
	for _, tr := range lk.transfers { // the caller holds lk.mu
		if strings.EqualFold(tr.ticketID, a.TicketID) {
			trs = append(trs, tr)
		}
	}
	if len(trs) > 0 {
		lk.factWaits[call.GetFactSeq()] = trs
	}
}

// awaitFact waits for the transfers that came before the fact seq, and returns the commits they put in the mirror;
// ok is false when the link ended first.
func (lk *nodeLink) awaitFact(seq uint64) (landed []string, ok bool) {
	lk.mu.Lock()
	trs := lk.factWaits[seq]
	delete(lk.factWaits, seq)
	lk.mu.Unlock()
	for _, tr := range trs {
		select {
		case <-tr.done:
		case <-lk.ctx.Done():
			return nil, false
		}
		lk.mu.Lock()
		if tr.landed != "" {
			landed = append(landed, tr.landed)
		}
		lk.mu.Unlock()
	}
	return landed, true
}
