package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/redact"
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
	// maxPullBody bounds the body file `athena gh pr create` reads into a held pull request.
	maxPullBody = 256 << 10
	// applyTimeout bounds the git push or gh call of one applied Proposal.
	applyTimeout = 2 * time.Minute
)

// ruleShadow is the rule of a push denied in shadow mode.
const ruleShadow = "shadow"

// shadow reports whether repo's work is held for the owner: the Policy's shadow mode (no Policy: no repository is).
func (n *Node) shadow(repo string) bool {
	return repo != "" && n.cfg.Policy != nil && n.cfg.Policy.Shadow(repo)
}

// shadowPushRule denies a `git push` (also inside `sh -c`) of a session in a repository in shadow mode (ctx.Shadow):
// the read-only token would refuse it anyway, and the reason tells the worker its branch is proposed when it reports.
func shadowPushRule(ctx RuleContext, command string) (Decision, bool) {
	if !ctx.Shadow || !strings.Contains(command, "push") {
		return Decision{}, false
	}
	match := func(words []string) (string, string) {
		if words[0] == "git" && gitSubcommand(words[1:]) == "push" {
			return "git push", shadowPushReason
		}
		return "", ""
	}
	if hit, reason := findBlocked(command, match, 0); hit != "" {
		return deny(ruleShadow, reason), true
	}
	return Decision{}, false
}

// pushPayload and pullPayload are the payloads of a push and a pull_request Proposal.
type pushPayload struct {
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
}

type pullPayload struct {
	Args   []string `json:"args"`
	Branch string   `json:"branch"`
}

// proposeBranch proposes the session's worktree branch as a push, for a Ticket of a repository in shadow mode: when
// the branch has commits no remote branch has (the clone's remote-tracking branches, which the node moves when it
// pushes), and no pending push of the Ticket names its commit already. A newer push supersedes the older one (Propose).
func (n *Node) proposeBranch(s *session) error {
	p := s.place
	if p.github == "" || s.reviewing() || !n.shadow(s.t.Repo) {
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

// ProposeRequest carries the session's `gh pr create` arguments, a body file already read into --body.
type ProposeRequest struct {
	Op         string   `json:"op"`
	Ticket     string   `json:"ticket"`
	Assignment string   `json:"assignment"`
	Args       []string `json:"args"`
}

// ProposeResponse says whether the pull request was held (shadow mode) or should run as it is (Held false).
type ProposeResponse struct {
	OK    bool   `json:"ok"`
	Held  bool   `json:"held,omitempty"`
	Error string `json:"error,omitempty"`
}

// handlePropose holds a live session's pull request when its repository is in shadow mode: the worktree branch is
// proposed as a push first (proposeBranch), then the pull request as a pull_request Proposal. In a live repository it
// answers Held false and records nothing.
func (n *Node) handlePropose(req ProposeRequest) ProposeResponse {
	s := n.lookup(req.Ticket, req.Assignment)
	if s == nil || s.over() {
		return ProposeResponse{Error: denyNoSession}
	}
	if s.reviewing() {
		return ProposeResponse{Error: "a review session opens no pull request: review the local branch"}
	}
	if !n.shadow(s.t.Repo) {
		return ProposeResponse{OK: true}
	}
	if len(req.Args) < 2 || req.Args[0] != "pr" || req.Args[1] != "create" {
		return ProposeResponse{Error: "only gh pr create is held for the owner"}
	}
	if err := n.proposeBranch(s); err != nil {
		s.log.Warn("propose the branch with its pull request", "error", err)
	}
	branch := branchName(s.t)
	payload, err := json.Marshal(pullPayload{Args: slices.Clone(req.Args), Branch: branch})
	if err != nil {
		return ProposeResponse{Error: err.Error()}
	}
	summary := "Open a pull request from " + branch + " on " + s.t.Repo + ", held for you"
	if title := flagValue(req.Args, "-t", "--title"); title != "" {
		summary = "Open the pull request " + strconv.Quote(title) + " on " + s.t.Repo + ", held for you"
	}
	pr, err := n.engine().Propose(core.Proposal{TicketID: s.t.ID, Kind: core.ProposalPull, Summary: summary, Payload: payload})
	if err != nil {
		s.log.Warn("pull request not held", "error", err)
		return ProposeResponse{Error: err.Error()}
	}
	s.log.Info("pull request held for the owner", "proposal", pr.ID)
	return ProposeResponse{OK: true, Held: true}
}

// flagValue is the value of the last of gh's options named short or long (-t T, --title T, --title=T, -tT).
func flagValue(args []string, short, long string) string {
	v := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			return v
		case a == short || a == long:
			if i+1 < len(args) {
				i++
				v = args[i]
			}
		case strings.HasPrefix(a, long+"="):
			v = strings.TrimPrefix(a, long+"=")
		case len(a) > 2 && strings.HasPrefix(a, short):
			v = a[2:]
		}
	}
	return v
}

// prCreateArgs reports whether gh's arguments run `gh pr create` and returns them as the node holds them: "pr",
// "create" and the rest in order, the -R/--repo option dropped (the pull request opens on the Ticket's repository), and
// a -F/--body-file read (from stdin for -) into --body, at most maxPullBody.
func prCreateArgs(args []string, stdin io.Reader) (out []string, ok bool, err error) {
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
		return nil, false, nil
	}
	out = []string{"pr", "create"}
	readBody := func(name string) error {
		var r io.Reader = stdin
		if name != "-" && name != "/dev/stdin" {
			f, err := os.Open(name)
			if err != nil {
				return err
			}
			defer f.Close()
			r = f
		}
		b, err := io.ReadAll(io.LimitReader(r, maxPullBody+1))
		if err != nil {
			return err
		}
		if len(b) > maxPullBody {
			return fmt.Errorf("the body file %s is larger than %d KiB", name, maxPullBody>>10)
		}
		out = append(out, "--body", string(b))
		return nil
	}
	for i := 0; i < len(args); i++ {
		if i == pos[0] || i == pos[1] {
			continue
		}
		a := args[i]
		switch {
		case a == "--":
			out = append(out, args[i:]...)
			return out, true, nil
		case a == "-R" || a == "--repo":
			i++
		case strings.HasPrefix(a, "--repo="):
		case a == "-F" || a == "--body-file":
			if i+1 >= len(args) {
				return nil, true, fmt.Errorf("%s needs a file", a)
			}
			i++
			if err := readBody(args[i]); err != nil {
				return nil, true, err
			}
		case strings.HasPrefix(a, "--body-file="):
			if err := readBody(strings.TrimPrefix(a, "--body-file=")); err != nil {
				return nil, true, err
			}
		case len(a) > 2 && strings.HasPrefix(a, "-F"):
			if err := readBody(a[2:]); err != nil {
				return nil, true, err
			}
		default:
			out = append(out, a)
		}
	}
	return out, true, nil
}

// --- applying a Proposal ---

// ApplyProposal makes an applied push or pull_request Proposal happen, on this node and with a write token minted for
// it (and revoked after). A push runs `git push <the repository> <sha>:refs/heads/<branch>` from the Ticket's worktree
// (worktreeGitEnv, the node's credential helper); a pull request applies the Ticket's pending push first (settling it),
// then runs `gh pr create <args>` for the Ticket's repository and branch. A failed push fails the pull request with its
// error, and no pull request is opened. The caller settles p with the result (Engine.SettleProposal).
func (n *Node) ApplyProposal(ctx context.Context, p core.Proposal) error {
	switch p.Kind {
	case core.ProposalPush:
		return n.applyPush(ctx, p)
	case core.ProposalPull:
		return n.applyPull(ctx, p)
	}
	return fmt.Errorf("proposal %s: the node applies pushes and pull requests, not %q", p.ID, p.Kind)
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
	if _, err := os.Stat(pl.workDir); err != nil {
		return core.Ticket{}, placement{}, fmt.Errorf("the worktree of ticket %s is gone: %w", t.ID, err)
	}
	return t, pl, nil
}

// writeToken mints a write token for repo for one applied Proposal; done revokes it.
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
	admin, common, err := n.worktreeGitDirs(place, t)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	tok, done, err := n.writeToken(ctx, p)
	if err != nil {
		return err
	}
	defer done()
	// The node's own git, hardened as on its mirror (no global config, https only, no proxy or helper of anyone
	// else's), its credential helper answering once for this repository, git's directories pinned to the worktree's.
	mirror := MirrorDir(n.cfg.StateDir, place.github)
	if err := writeSystemConfig(mirror, cloneURL(place.github), githubTarget(place.github)); err != nil {
		return err
	}
	nonce, revoke := n.grantNode(place.github, tok)
	defer revoke()
	env := worktreeGitEnv(mirrorGitEnv(mirror, place.github, &nodeAuth{bin: n.cfg.AthenaBin, socket: n.cfg.SocketPath, nonce: nonce}),
		admin, common, place.workDir)
	defer lockRepo(place.repo)()
	if _, err := gitInCtx(ctx, place.workDir, env, "push", "--quiet", cloneURL(place.github), pl.SHA+":refs/heads/"+pl.Branch); err != nil {
		return err
	}
	// Pushed: the branch's commits are on the remote, and the next Report proposes only newer ones.
	if _, err := gitInCtx(ctx, place.workDir, env, "update-ref", "refs/remotes/origin/"+pl.Branch, pl.SHA); err != nil {
		n.log.Warn("record the pushed branch", "ticket", t.ID, "branch", pl.Branch, "error", err)
	}
	n.log.Info("applied a push held for the owner", "ticket", t.ID, "proposal", p.ID, "branch", pl.Branch, "commit", pl.SHA)
	return nil
}

func (n *Node) applyPull(ctx context.Context, p core.Proposal) error {
	var pl pullPayload
	if err := json.Unmarshal(p.Payload, &pl); err != nil {
		return fmt.Errorf("proposal %s: payload: %w", p.ID, err)
	}
	if len(pl.Args) < 2 || pl.Args[0] != "pr" || pl.Args[1] != "create" {
		return fmt.Errorf("proposal %s is not a gh pr create", p.ID)
	}
	pending, err := n.engine().PendingProposals()
	if err != nil {
		return err
	}
	for _, q := range pending {
		if q.TicketID != p.TicketID || q.Kind != core.ProposalPush {
			continue
		}
		perr := n.applyPush(ctx, q)
		if err := n.engine().SettleProposal(q.ID, perr); err != nil {
			n.log.Error("settle the push applied with its pull request", "proposal", q.ID, "error", err)
		}
		if perr != nil {
			return perr
		}
	}
	t, place, err := n.applyTarget(p)
	if err != nil {
		return err
	}
	admin, common, err := n.worktreeGitDirs(place, t)
	if err != nil {
		return err
	}
	real, err := realGh(os.Getenv("PATH"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	tok, done, err := n.writeToken(ctx, p)
	if err != nil {
		return err
	}
	defer done()
	// A fresh gh config of the node's own: nothing a worker wrote in its session's gh config applies.
	cfgDir, err := os.MkdirTemp("", "athena-gh-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(cfgDir)
	args := slices.Clone(pl.Args)
	if flagValue(args, "-H", "--head") == "" {
		args = append(args, "--head", branchName(t))
	}
	cmd := exec.CommandContext(ctx, real, args...)
	cmd.Dir = place.workDir
	cmd.Env = append(ghEnv(worktreeGitEnv(nodeGitBaseEnv(), admin, common, place.workDir), tok, cfgDir), "GH_REPO="+place.github)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh pr create: %v: %s", err, redact.String(strings.TrimSpace(stderr.String())))
	}
	n.log.Info("applied a pull request held for the owner", "ticket", t.ID, "proposal", p.ID,
		"url", strings.TrimSpace(stdout.String()))
	return nil
}
