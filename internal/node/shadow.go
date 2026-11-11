package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/redact"
	"github.com/danielpina1/athena/internal/sources/github"
)

// Shadow mode: a repository whose work is held for the owner. Its Tickets' sessions get read-only GitHub tokens
// (tokenStore.writable), which is what holds the work back whatever a session runs. The hook rules and `athena gh` are
// the cooperative path that turns an attempt into a Proposal: a `git push` is denied with a plain explanation, and
// `athena gh pr create` records a pull_request Proposal instead of running. Each Report of a shadow Ticket proposes
// the worktree branch as a push. The owner applies a Proposal; the node then makes it happen (ApplyProposal), with a
// write token minted for that alone.

const (
	// shadowPushReason is why a session of a repository in shadow mode may not push.
	shadowPushReason = "This repository is in shadow mode: pushes are held for the owner. Your branch is proposed when you " +
		"report; carry on as if it will be pushed."
	// shadowPullHeld is what `athena gh pr create` prints in a repository in shadow mode.
	shadowPullHeld = "Held for the owner (shadow mode): the pull request is proposed and opens when they apply it."
	// restrictedPushReason is why a restricted session may not push, whatever its repository's mode (R15).
	restrictedPushReason = "Work started by someone outside your allow list is held for the owner: pushes wait for them. " +
		"Your branch is proposed when you report; carry on as if it will be pushed."
	// restrictedPullHeld is what `athena gh pr create` prints in a restricted session.
	restrictedPullHeld = "Held for the owner: work started by someone outside your allow list opens its pull request when they apply it."
	// maxPullTitle and maxPullBody bound a held pull request's title and body, in characters (GitHub's own limit for a
	// body); maxBodyFile bounds the body file `athena gh pr create` reads.
	maxPullTitle = 256
	maxPullBody  = 65536
	maxBodyFile  = 256 << 10
	// applyTimeout bounds the git push or the GitHub call of one applied Proposal.
	applyTimeout = 2 * time.Minute
)

// ruleShadow is the rule of a push denied in shadow mode.
const ruleShadow = "shadow"

// shadow reports whether repo's work is held for the owner: the Policy's shadow mode (no Policy: no repository is).
func (n *Node) shadow(repo string) bool {
	return repo != "" && n.cfg.Policy != nil && n.cfg.Policy.Shadow(repo)
}

// heldForOwner reports whether the session's work is held for the owner: its repository is in shadow mode, or it runs
// under the restricted tool policy (R15), whatever its repository's mode. A GitHub App token cannot be kept to one
// branch or thread, so a restricted session gets read-only tokens, and its push, pull request and replies wait for the
// owner as Proposals.
func (n *Node) heldForOwner(s *session) bool {
	return n.shadow(s.ticketRepo()) || (s.ticketRepo() != "" && s.restricted())
}

// shadowPushRule denies a `git push` (also inside `sh -c`) of a session whose work is held for the owner (ctx.Shadow:
// its repository is in shadow mode, or it is restricted): the read-only token would refuse it anyway, and the reason
// tells the worker its branch is proposed when it reports.
func shadowPushRule(ctx RuleContext, command string) (Decision, bool) {
	if !ctx.Shadow || !strings.Contains(command, "push") {
		return Decision{}, false
	}
	reason := shadowPushReason
	if ctx.Restricted {
		reason = restrictedPushReason
	}
	match := func(words []string) (string, string) {
		if words[0] == "git" && gitSubcommand(words[1:]) == "push" {
			return "git push", reason
		}
		return "", ""
	}
	if hit, reason := findBlocked(command, match, 0); hit != "" {
		return deny(ruleShadow, reason), true
	}
	return Decision{}, false
}

// ruleChild is the rule of a child Ticket's denied push.
const ruleChild = "child"

// childDelivers is why a child Ticket of lead may not push or open a pull request (R6).
func childDelivers(lead string) string {
	return "A child ticket delivers through its lead: commit on your branch and report ready; " + lead + " merges it."
}

// childPushRule denies a `git push` (also inside `sh -c`) of a child Ticket's session (ctx.Child, its lead): the lead
// merges the child's branch.
func childPushRule(ctx RuleContext, command string) (Decision, bool) {
	if ctx.Child == "" || !strings.Contains(command, "push") {
		return Decision{}, false
	}
	match := func(words []string) (string, string) {
		if words[0] == "git" && gitSubcommand(words[1:]) == "push" {
			return "git push", childDelivers(ctx.Child)
		}
		return "", ""
	}
	if hit, reason := findBlocked(command, match, 0); hit != "" {
		return deny(ruleChild, reason), true
	}
	return Decision{}, false
}

// pushPayload is the payload of a push Proposal; a pull_request Proposal's is a HeldPull.
type pushPayload struct {
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
}

// proposeBranch proposes the session's worktree branch as a push, for a session held for the owner (heldForOwner): when
// the branch has commits no remote branch has (the clone's remote-tracking branches, which the node moves when it
// pushes), and no pending push of the Ticket names its commit already. A newer push supersedes the older one (Propose).
func (n *Node) proposeBranch(s *session) error {
	p := s.place
	if p.github == "" || s.reviewing() || !n.heldForOwner(s) {
		return nil
	}
	if _, err := os.Stat(p.workDir); err != nil {
		return nil // nothing prepared yet
	}
	env, err := n.worktreeEnv(p, s.t)
	if err != nil {
		return err
	}
	branch := branchName(s.t)
	unlock := lockRepo(p.repo)
	sha, err := gitIn(p.workDir, env, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	var count string
	if err == nil {
		count, err = gitIn(p.workDir, env, "rev-list", "--count", sha, "--not", "--remotes")
	}
	unlock()
	if err != nil {
		return err
	}
	if count == "0" {
		return nil
	}
	pending, err := n.engine().PendingProposals()
	if err != nil {
		return err
	}
	for _, q := range pending {
		var pl pushPayload
		if q.TicketID == s.t.ID && q.Kind == core.ProposalPush && json.Unmarshal(q.Payload, &pl) == nil && pl.SHA == sha {
			return nil
		}
	}
	payload, err := json.Marshal(pushPayload{Branch: branch, SHA: sha})
	if err != nil {
		return err
	}
	commits := "commits"
	if count == "1" {
		commits = "commit"
	}
	pr, err := n.engine().Propose(core.Proposal{TicketID: s.t.ID, Kind: core.ProposalPush, Payload: payload,
		Summary: fmt.Sprintf("Push %s to %s: %s new %s, held for you", branch, s.t.Repo, count, commits)})
	if err != nil {
		return err
	}
	s.log.Info("branch proposed as a push", "proposal", pr.ID, "branch", branch, "commit", sha)
	return nil
}

// --- athena gh pr create, held ---

// OpPropose asks the node to hold a session's `gh pr create` for the owner; `athena gh` sends it.
const OpPropose = "propose"

// HeldPull is a pull request held for the owner: the only fields it can have. It opens from the Ticket's own branch on
// the Ticket's repository; an empty Base is the repository's default branch.
type HeldPull struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Base  string `json:"base,omitempty"`
	Draft bool   `json:"draft,omitempty"`
}

// PullOpener is the optional part of Credentials that opens a pull request through GitHub's API (POST
// /repos/{owner}/{name}/pulls) with a write token it mints for that call and revokes after; it returns its number and
// URL.
type PullOpener interface {
	OpenPull(ctx context.Context, repo, head string, p HeldPull) (number int, url string, err error)
}

// ProposeRequest carries the session's `gh pr create` as a HeldPull, parsed and its body file read by `athena gh`
// inside the session's sandbox; Pull is nil when the arguments are not ones a held pull request takes.
type ProposeRequest struct {
	Op         string    `json:"op"`
	Ticket     string    `json:"ticket"`
	Assignment string    `json:"assignment"`
	Pull       *HeldPull `json:"pull,omitempty"`
}

// ProposeResponse says whether the pull request was held for the owner or should run as it is (Held false), and
// whether it was held because the session is restricted (Restricted), which `athena gh` says in its own words.
type ProposeResponse struct {
	OK         bool   `json:"ok"`
	Held       bool   `json:"held,omitempty"`
	Restricted bool   `json:"restricted,omitempty"`
	Error      string `json:"error,omitempty"`
}

// pullOptions names what a held pull request takes, for the refusals.
const pullOptions = "--title, --body, --body-file, --base and --draft"

// branchNameRe is a base branch a held pull request may name: no leading - or /, no .., no spaces or git specials.
var branchNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]*$`)

// checkPull checks a held pull request again on the node: a title of one line, sane lengths, a plain base branch.
func checkPull(p HeldPull) error {
	switch {
	case strings.TrimSpace(p.Title) == "":
		return errors.New("a held pull request needs a title (--title)")
	case strings.ContainsAny(p.Title, "\r\n") || utf8.RuneCountInString(p.Title) > maxPullTitle:
		return fmt.Errorf("the pull request title must be one line of at most %d characters", maxPullTitle)
	case !utf8.ValidString(p.Body) || utf8.RuneCountInString(p.Body) > maxPullBody:
		return fmt.Errorf("the pull request body must be text of at most %d characters", maxPullBody)
	case p.Base != "" && (len(p.Base) > 255 || !branchNameRe.MatchString(p.Base) || strings.Contains(p.Base, "..") ||
		strings.HasSuffix(p.Base, "/") || strings.HasSuffix(p.Base, ".lock") || strings.Contains(p.Base, "//")):
		return fmt.Errorf("the base %q is not a branch name", p.Base)
	}
	return nil
}

// decodePropose reads a propose request strictly: a field it does not know (such as gh arguments) refuses it.
func decodePropose(line []byte) (ProposeRequest, error) {
	var req ProposeRequest
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	return req, err
}

// handlePropose holds a live session's pull request when its work is held for the owner (heldForOwner: its repository
// is in shadow mode, or it is restricted): the worktree branch is proposed as a push first (proposeBranch), then the
// pull request, checked again (checkPull), as a pull_request Proposal whose payload is the HeldPull alone. Otherwise it
// answers Held false and records nothing.
func (n *Node) handlePropose(req ProposeRequest) ProposeResponse {
	s := n.lookup(req.Ticket, req.Assignment)
	if s == nil || s.over() {
		return ProposeResponse{Error: denyNoSession}
	}
	if s.reviewing() {
		return ProposeResponse{Error: "a review session opens no pull request: review the local branch"}
	}
	if s.t.Lead != "" {
		return ProposeResponse{Error: childDelivers(s.t.Lead)}
	}
	if !n.heldForOwner(s) {
		return ProposeResponse{OK: true}
	}
	if req.Pull == nil {
		return ProposeResponse{Error: "a pull request held for the owner takes only " + pullOptions}
	}
	if err := checkPull(*req.Pull); err != nil {
		return ProposeResponse{Error: err.Error()}
	}
	if err := n.proposeBranch(s); err != nil {
		s.log.Warn("propose the branch with its pull request", "error", err)
	}
	payload, err := json.Marshal(*req.Pull)
	if err != nil {
		return ProposeResponse{Error: err.Error()}
	}
	summary := "Open the pull request " + strconv.Quote(req.Pull.Title) + " on " + s.t.Repo + ", held for you"
	pr, err := n.engine().Propose(core.Proposal{TicketID: s.t.ID, Kind: core.ProposalPull, Summary: summary, Payload: payload})
	if err != nil {
		s.log.Warn("pull request not held", "error", err)
		return ProposeResponse{Error: err.Error()}
	}
	s.log.Info("pull request held for the owner", "proposal", pr.ID)
	return ProposeResponse{OK: true, Held: true, Restricted: s.restricted()}
}

// parsePull reports whether gh's arguments run `gh pr create` (isCreate) and reads them into a HeldPull: --title/-t,
// --body/-b, --body-file/-F (a file, or stdin for -, read here, inside the session's sandbox), --base/-B and
// --draft/-d, in their spaced, = and joined forms. Any other argument is an error, and so is a missing title.
func parsePull(args []string, stdin io.Reader) (p HeldPull, isCreate bool, err error) {
	var pos []int
	for i := 0; i < len(args) && len(pos) < 2; i++ {
		switch a := args[i]; {
		case a == "--":
			i = len(args)
		case a == "-R" || a == "--repo":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			pos = append(pos, i)
		}
	}
	if len(pos) < 2 || args[pos[0]] != "pr" || args[pos[1]] != "create" {
		return HeldPull{}, false, nil
	}
	readBody := func(name string) (string, error) {
		var r io.Reader = stdin
		if name != "-" && name != "/dev/stdin" {
			f, err := os.Open(name)
			if err != nil {
				return "", err
			}
			defer f.Close()
			r = f
		}
		b, err := io.ReadAll(io.LimitReader(r, maxBodyFile+1))
		if err != nil {
			return "", err
		}
		if len(b) > maxBodyFile {
			return "", fmt.Errorf("the body file %s is larger than %d KiB", name, maxBodyFile>>10)
		}
		return string(b), nil
	}
	// value is the value of option a (short -x or long --name) at i: joined, after =, or the next argument.
	value := func(i *int, a, short, long string) (string, bool) {
		switch {
		case a == short || a == long:
			if *i+1 >= len(args) {
				return "", false
			}
			*i++
			return args[*i], true
		case strings.HasPrefix(a, long+"="):
			return strings.TrimPrefix(a, long+"="), true
		case len(a) > 2 && strings.HasPrefix(a, short) && !strings.HasPrefix(a, "--"):
			return a[2:], true
		}
		return "", false
	}
	for i := 0; i < len(args); i++ {
		if i == pos[0] || i == pos[1] {
			continue
		}
		a := args[i]
		if v, ok := value(&i, a, "-t", "--title"); ok {
			p.Title = v
		} else if v, ok := value(&i, a, "-b", "--body"); ok {
			p.Body = v
		} else if v, ok := value(&i, a, "-F", "--body-file"); ok {
			if p.Body, err = readBody(v); err != nil {
				return HeldPull{}, true, err
			}
		} else if v, ok := value(&i, a, "-B", "--base"); ok {
			p.Base = v
		} else if a == "-d" || a == "--draft" || a == "--draft=true" {
			p.Draft = true
		} else if a == "--draft=false" {
			p.Draft = false
		} else {
			return HeldPull{}, true, fmt.Errorf("a pull request held for the owner takes only %s: %q is not one of them", pullOptions, a)
		}
	}
	return p, true, checkPull(p)
}

// --- applying a Proposal ---

// errStartedBefore settles a Proposal an earlier run began to apply and never settled: whether it happened is unknown.
var errStartedBefore = errors.New("Athena stopped while applying this; check GitHub before applying again.")

// ApplyProposal makes an applied push or pull_request Proposal happen, with a write token minted for it. It first marks
// the Proposal started (Engine.StartProposal): one an earlier run started and never settled fails with
// errStartedBefore, and nothing goes out again. A push is pushed from the node's own mirror (applyPush); a pull request
// applies the Ticket's pending push first (settling it), then opens through GitHub's API (PullOpener) with exactly the
// held fields, from the Ticket's branch. A failed push fails the pull request with its error, and no pull request is
// opened. The caller settles p with the result (Engine.SettleProposal).
func (n *Node) ApplyProposal(ctx context.Context, p core.Proposal) error {
	if p.Kind != core.ProposalPush && p.Kind != core.ProposalPull {
		return fmt.Errorf("proposal %s: the node applies pushes and pull requests, not %q", p.ID, p.Kind)
	}
	if err := n.startApply(p); err != nil {
		return err
	}
	if p.Kind == core.ProposalPush {
		return n.applyPush(ctx, p)
	}
	return n.applyPull(ctx, p)
}

// startApply marks p started, or reports errStartedBefore when an earlier run did.
func (n *Node) startApply(p core.Proposal) error {
	first, err := n.engine().StartProposal(p.ID)
	if err != nil {
		return err
	}
	if !first {
		return errStartedBefore
	}
	return nil
}

// applyTarget is a Proposal's Ticket and its placement on this node: a GitHub worktree of the Proposal's repository.
func (n *Node) applyTarget(p core.Proposal) (core.Ticket, placement, error) {
	t, _, err := n.engine().Ticket(p.TicketID)
	if err != nil {
		return core.Ticket{}, placement{}, err
	}
	pl, ok, err := n.recordedPlacement(t)
	if err != nil {
		return core.Ticket{}, placement{}, err
	}
	if !ok || pl.github == "" || !strings.EqualFold(pl.github, p.Repo) {
		return core.Ticket{}, placement{}, fmt.Errorf("ticket %s has no worktree of %s on this node", t.ID, p.Repo)
	}
	return t, pl, nil
}

// mirrorHead is the branch the mirror's HEAD names (the default branch the node last saw), or "" when it names none
// the node would write back.
func mirrorHead(mirror string) string {
	b, err := readSmall(filepath.Join(mirror, "HEAD"))
	if err != nil {
		return ""
	}
	branch, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/heads/")
	if !ok || !branchNameRe.MatchString(branch) || strings.Contains(branch, "..") {
		return ""
	}
	return branch
}

// applyPush pushes the commit of a push Proposal to its branch on GitHub. The push runs from the node's own mirror, as
// every token-bearing git of the node does (mirrorGit: its config rewritten first, no global config, https only, the
// node's one-call credential helper), with the clone's objects as an alternate object directory, which git reads as
// data only. No git that holds the token reads the clone's or the worktree's config, which the worker can write.
func (n *Node) applyPush(ctx context.Context, p core.Proposal) error {
	var pl pushPayload
	if err := json.Unmarshal(p.Payload, &pl); err != nil {
		return fmt.Errorf("proposal %s: payload: %w", p.ID, err)
	}
	t, place, err := n.applyTarget(p)
	if err != nil {
		return err
	}
	if !shaRe.MatchString(pl.SHA) || pl.Branch != branchName(t) {
		return fmt.Errorf("proposal %s names no commit of the ticket's branch %s", p.ID, branchName(t))
	}
	repo, clone := place.github, place.repo
	mirror := MirrorDir(n.cfg.StateDir, repo)
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		return fmt.Errorf("this node has no mirror of %s to push from: %w", repo, err)
	}
	common := place.common
	if common == "" {
		common = filepath.Join(clone, ".git")
	}
	// Before any token exists. A worker can write the clone's objects, and the push reads them as an alternate.
	if err := checkObjectStore(filepath.Join(common, "objects")); err != nil {
		return fmt.Errorf("proposal %s: %w", p.ID, err)
	}
	ctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	tok, done, err := n.writeToken(ctx, p)
	if err != nil {
		return err
	}
	defer done()
	// The clone's lock covers its mirror too (EnsureClone holds it while it syncs the mirror).
	defer lockRepo(clone)()
	if err := writeSystemConfig(mirror, cloneURL(repo), githubTarget(repo)); err != nil {
		return err
	}
	nonce, revoke := n.grantNode(repo, tok)
	defer revoke()
	env := append(mirrorGitEnv(mirror, repo, &nodeAuth{bin: n.cfg.AthenaBin, socket: n.cfg.SocketPath, nonce: nonce}),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES="+filepath.Join(common, "objects"))
	if _, err := mirrorGit(ctx, mirror, mirrorHead(mirror), env, "push", "--quiet", cloneURL(repo),
		pl.SHA+":refs/heads/"+pl.Branch); err != nil {
		return err
	}
	// Pushed: the branch's commits are on the remote, and the next Report proposes only newer ones. A tokenless git on
	// the clone, as the node's other git there.
	cenv := repoGitEnv(cloneGitEnv(clone, repo), common, clone)
	if _, err := gitInCtx(ctx, clone, cenv, "update-ref", "refs/remotes/origin/"+pl.Branch, pl.SHA); err != nil {
		n.log.Warn("record the pushed branch", "ticket", t.ID, "branch", pl.Branch, "error", err)
	}
	n.log.Info("applied a push held for the owner", "ticket", t.ID, "proposal", p.ID, "branch", pl.Branch, "commit", pl.SHA)
	return nil
}

// checkObjectStore refuses a clone's object store that reaches outside itself: an objects/info/alternates file, or a
// symbolic link anywhere in it, either of which could make a push carry an object the node's user can read.
func checkObjectStore(objects string) error {
	if _, err := os.Lstat(filepath.Join(objects, "info", "alternates")); err == nil {
		return errors.New("the clone's object store has an alternates file, so this push was not made; look at the clone before applying again")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the clone's object store cannot be checked: %w", err)
	}
	return filepath.WalkDir(objects, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("the clone's object store cannot be checked: %w", err)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("the clone's object store has a symbolic link (%s), so this push was not made; look at the clone before applying again", d.Name())
		}
		return nil
	})
}

// writeToken mints a write token for a push Proposal's repository; done revokes it.
func (n *Node) writeToken(ctx context.Context, p core.Proposal) (tok string, done func(), err error) {
	if n.cfg.Credentials == nil {
		return "", nil, errors.New("this node has no GitHub credentials")
	}
	tok, _, err = n.cfg.Credentials.Token(ctx, p.Repo, true)
	if err != nil {
		return "", nil, fmt.Errorf("github token for %s: %s", p.Repo, redact.String(err.Error()))
	}
	if tok == "" {
		return "", nil, fmt.Errorf("github token for %s: empty token", p.Repo)
	}
	return tok, func() { n.tokens.revoke("proposal "+p.ID, tok) }, nil
}

// applyPull opens a held pull request: the Ticket's pending push first, then the API call with exactly the held fields
// and the Ticket's own branch as its head. Nothing runs in the worktree.
func (n *Node) applyPull(ctx context.Context, p core.Proposal) error {
	var pl HeldPull
	dec := json.NewDecoder(bytes.NewReader(p.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pl); err != nil {
		return fmt.Errorf("proposal %s: payload: %w", p.ID, err)
	}
	if err := checkPull(pl); err != nil {
		return fmt.Errorf("proposal %s: %w", p.ID, err)
	}
	t, _, err := n.engine().Ticket(p.TicketID)
	if err != nil {
		return err
	}
	if t.Repo == "" || !strings.EqualFold(t.Repo, p.Repo) {
		return fmt.Errorf("proposal %s is not for ticket %s's repository", p.ID, t.ID)
	}
	opener, ok := n.cfg.Credentials.(PullOpener)
	if !ok {
		return errors.New("this node cannot open pull requests on GitHub")
	}
	pending, err := n.engine().PendingProposals()
	if err != nil {
		return err
	}
	for _, q := range pending {
		if q.TicketID != p.TicketID || q.Kind != core.ProposalPush {
			continue
		}
		perr := n.startApply(q)
		if perr == nil {
			perr = n.applyPush(ctx, q)
		}
		if err := n.engine().SettleProposal(q.ID, perr); err != nil {
			n.log.Error("settle the push applied with its pull request", "proposal", q.ID, "error", err)
		}
		if perr != nil {
			return perr
		}
	}
	ctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	number, url, err := opener.OpenPull(ctx, p.Repo, branchName(t), pl)
	if err != nil {
		return fmt.Errorf("open the pull request: %s", redact.String(err.Error()))
	}
	n.log.Info("applied a pull request held for the owner", "ticket", t.ID, "proposal", p.ID, "url", url)
	// A Ticket that ended meanwhile has nothing to follow it with; the pull request stands either way, so no failure here
	// may fail the Proposal.
	if thread := github.ThreadKey(p.Repo, number); number > 0 {
		if err := n.engine().BindThread(t.ID, thread, true); err != nil {
			n.log.Warn("the applied pull request was not bound to its ticket", "ticket", t.ID, "thread", thread, "error", err)
		}
	}
	return nil
}
