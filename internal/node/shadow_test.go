package node

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/store"
)

// shadowPolicy is a Policy whose repositories in shadow mode can change while the node runs; every tool policy is
// standard.
type shadowPolicy struct {
	fakeToolPolicy
	mu    sync.Mutex
	repos map[string]bool
}

func newShadowPolicy(repos ...string) *shadowPolicy {
	p := &shadowPolicy{repos: map[string]bool{}}
	for _, r := range repos {
		p.repos[r] = true
	}
	return p
}

func (p *shadowPolicy) Shadow(repo string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.repos[strings.ToLower(repo)]
}

func (p *shadowPolicy) set(repo string, on bool) {
	p.mu.Lock()
	p.repos[repo] = on
	p.mu.Unlock()
}

// tokenOf is the password of a git credential answer.
func tokenOf(answer string) string {
	_, tok, _ := strings.Cut(answer, "password=")
	return strings.TrimSpace(tok)
}

// proposalsOf returns a Ticket's Proposals in every state, oldest first.
func (h *ghHarness) proposalsOf(ticketID string) []*core.Proposal {
	h.t.Helper()
	var out []*core.Proposal
	if err := h.st.Tx(func(tx *store.Tx) error {
		var err error
		out, err = tx.TicketProposals(ticketID)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// commit makes an empty commit on the Ticket's worktree branch and returns it.
func (h *ghHarness) commit(tk core.Ticket, msg string) string {
	h.t.Helper()
	dir := TicketDir(h.cfg.WorkRoot, tk)
	git(h.t, dir, "commit", "-q", "--allow-empty", "-m", msg)
	return git(h.t, dir, "rev-parse", "HEAD")
}

// ghPrCreate runs `athena gh pr create args...` as the session of tk and a, with path as its PATH.
func (h *ghHarness) ghPrCreate(tk core.Ticket, a core.Assignment, path string, args ...string) (code int, stdout, stderr string) {
	h.t.Helper()
	env := []string{"ATHENA_SOCKET=" + h.cfg.SocketPath, "ATHENA_TICKET=" + tk.ID, "ATHENA_ASSIGNMENT=" + a.ID, "PATH=" + path}
	var out, errb bytes.Buffer
	code = RunGh(append([]string{"pr", "create"}, args...), strings.NewReader(""), &out, &errb, env)
	return code, out.String(), errb.String()
}

const ownRequest = "protocol=https\nhost=github.com\npath=o/r.git\n"

// A shadow Ticket's sessions get read-only tokens and a live one's write tokens; a repository switched to shadow has
// its held write tokens revoked, and its next request is read-only.
func TestShadowTicketGetsReadToken(t *testing.T) {
	pol := newShadowPolicy("o/r")
	h := newGHHarness(t, &fakeCreds{}, func(c *Config) { c.Policy = pol })
	tk, a := h.accept("github:issuecomment:1", "o/r", 12, "")
	if got := h.gitCredential(tk.ID, a.ID, ownRequest); tokenOf(got) == "" {
		t.Fatalf("shadow session got no token: %q", got)
	}
	asked := h.creds.asked()
	if len(asked) == 0 || slices.Contains(asked, true) {
		t.Fatalf("shadow ticket asked write = %v, want only read-only tokens", asked)
	}
	live, la := h.accept("github:issuecomment:2", "o/s", 3, "")
	liveTok := tokenOf(h.gitCredential(live.ID, la.ID, "protocol=https\nhost=github.com\npath=o/s.git\n"))
	if asked := h.creds.asked(); liveTok == "" || !asked[len(asked)-1] {
		t.Fatalf("live ticket: token %q, asked write = %v, want a write token", liveTok, asked)
	}

	pol.set("o/s", true)
	h.n.DropTokens("O/S")
	h.eventually("the held write token revoked", func() bool { return slices.Contains(h.creds.revokedTokens(), liveTok) })
	if slices.ContainsFunc(h.creds.revokedTokens(), func(v string) bool { return v != liveTok }) {
		t.Errorf("revoked %v, want only o/s's write token", h.creds.revokedTokens())
	}
	again := tokenOf(h.gitCredential(live.ID, la.ID, "protocol=https\nhost=github.com\npath=o/s.git\n"))
	if asked := h.creds.asked(); again == "" || again == liveTok || asked[len(asked)-1] {
		t.Fatalf("after the switch: token %q (was %q), asked write = %v, want a new read-only token", again, liveTok, asked)
	}
}

// A git push in a shadow Ticket is denied with a plain reason, never escalated, however it is spelled; live, the rule
// does not apply.
func TestShadowGitPushDenied(t *testing.T) {
	dir := t.TempDir()
	const reason = "This repository is in shadow mode: pushes are held for the owner. Your branch is proposed when you " +
		"report; carry on as if it will be pushed."
	bash := func(cmd string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"command": cmd})
		return b
	}
	for _, rc := range []RuleContext{{TicketDir: dir, Shadow: true}, {TicketDir: dir, Shadow: true, Restricted: true},
		{TicketDir: dir, Shadow: true, Blocked: []string{"git push"}}} {
		for _, cmd := range []string{"git push", "git push origin HEAD:refs/heads/athena/t-0001", "git -C . push --force",
			"cd sub && GIT_TRACE=1 git push -u origin x", `sh -c 'git push origin main'`, "make && git push"} {
			d := Evaluate(rc, "Bash", bash(cmd))
			if d.Verdict != VerdictDeny || d.Reason != reason || d.Rule != ruleShadow {
				t.Errorf("%+v %q = %+v, want the shadow deny", rc, cmd, d)
			}
		}
	}
	for _, cmd := range []string{"git status", "git commit -m 'push the button'", "git log --format=%s origin/push"} {
		if d := Evaluate(RuleContext{TicketDir: dir, Shadow: true}, "Bash", bash(cmd)); d.Rule == ruleShadow {
			t.Errorf("%q = %+v, not a push", cmd, d)
		}
	}
	if d := Evaluate(RuleContext{TicketDir: dir}, "Bash", bash("git push")); d.Rule == ruleShadow {
		t.Errorf("live git push = %+v, want no shadow rule", d)
	}

	// Through the hook of a shadow session: denied, and no Approval asks anyone.
	h := newGHHarness(t, &fakeCreds{}, func(c *Config) { c.Policy = newShadowPolicy("o/r") })
	tk, a := h.accept("github:issuecomment:1", "o/r", 12, "")
	resp := h.hook(EventPreToolUse, tk.ID, a.ID, map[string]any{"tool_name": "Bash",
		"tool_input": map[string]any{"command": "git push origin HEAD"}})
	if resp.Verdict != VerdictDeny || resp.Reason != reason {
		t.Fatalf("hook = %+v, want the shadow deny", resp)
	}
	if n := len(h.eventsOf(core.EventApprovalRequested)); n != 0 {
		t.Fatalf("%d approvals requested, want none", n)
	}
}

// `athena gh pr create` in a shadow Ticket records a pull_request Proposal, its body file read into --body, and does
// not run gh; in a live Ticket gh runs as before.
func TestShadowGhPrCreateProposes(t *testing.T) {
	h := newGHHarness(t, &fakeCreds{}, func(c *Config) { c.Policy = newShadowPolicy("o/r") })
	tk, a := h.accept("github:issuecomment:1", "o/r", 12, "")
	ghDir, record := writeFakeGh(t)
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("Fixes the typo.\n\nDetails here."), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := h.ghPrCreate(tk, a, ghDir, "--title", "T", "--body-file", body)
	if code != 0 || strings.TrimSpace(out) != "Held for the owner (shadow mode): the pull request is proposed and opens when they apply it." {
		t.Fatalf("gh pr create = exit %d, out %q, err %q", code, out, errOut)
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("the real gh ran for a held pull request")
	}
	ps := h.proposalsOf(tk.ID)
	if len(ps) != 1 || ps[0].Kind != core.ProposalPull || ps[0].State != core.ProposalPending || ps[0].Repo != "o/r" {
		t.Fatalf("proposals = %+v, want one pending pull_request", ps)
	}
	var pl pullPayload
	if err := json.Unmarshal(ps[0].Payload, &pl); err != nil {
		t.Fatal(err)
	}
	want := []string{"pr", "create", "--title", "T", "--body", "Fixes the typo.\n\nDetails here."}
	if !slices.Equal(pl.Args, want) || pl.Branch != branchName(tk) {
		t.Fatalf("payload = %+v, want args %q on %s", pl, want, branchName(tk))
	}
	if !strings.Contains(ps[0].Summary, `"T"`) || !strings.Contains(ps[0].Summary, "held for you") {
		t.Errorf("summary = %q", ps[0].Summary)
	}

	// Live: gh runs with the session's arguments, as it always did.
	live, la := h.accept("github:issuecomment:2", "o/s", 3, "")
	if code, _, _ := h.ghPrCreate(live, la, ghDir, "--fill"); code != 3 {
		t.Fatalf("live gh pr create: exit %d, want gh's own 3", code)
	}
	if rec, err := os.ReadFile(record); err != nil || !strings.Contains(string(rec), "args=pr create --fill") {
		t.Fatalf("live gh got %q, %v", rec, err)
	}
	if ps := h.proposalsOf(live.ID); len(ps) != 0 {
		t.Fatalf("live ticket proposals = %+v", ps)
	}
}

// A Report of a shadow session with a new commit proposes its branch as a push; a second commit and Report supersede
// it; a Report with nothing new proposes nothing more.
func TestProposeBranchOnReport(t *testing.T) {
	h := newGHHarness(t, &fakeCreds{}, func(c *Config) { c.Policy = newShadowPolicy("o/r") })
	tk, a := h.accept("github:issuecomment:1", "o/r", 12, "")
	if resp := h.report(tk.ID, a.ID, core.ReportProgress, "nothing yet"); !resp.OK {
		t.Fatalf("report = %+v", resp)
	}
	if ps := h.proposalsOf(tk.ID); len(ps) != 0 {
		t.Fatalf("a branch with no commit of its own was proposed: %+v", ps)
	}
	first := h.commit(tk, "fix the typo")
	if resp := h.report(tk.ID, a.ID, core.ReportProgress, "fixed"); !resp.OK {
		t.Fatalf("report = %+v", resp)
	}
	ps := h.proposalsOf(tk.ID)
	if len(ps) != 1 || ps[0].Kind != core.ProposalPush || ps[0].State != core.ProposalPending {
		t.Fatalf("proposals = %+v, want one pending push", ps)
	}
	var pl pushPayload
	if err := json.Unmarshal(ps[0].Payload, &pl); err != nil || pl.SHA != first || pl.Branch != branchName(tk) {
		t.Fatalf("payload = %+v %v, want %s at %s", pl, err, branchName(tk), first)
	}
	if resp := h.report(tk.ID, a.ID, core.ReportProgress, "still fixed"); !resp.OK {
		t.Fatalf("report = %+v", resp)
	}
	if ps := h.proposalsOf(tk.ID); len(ps) != 1 {
		t.Fatalf("the same commit proposed again: %+v", ps)
	}
	second := h.commit(tk, "and the other one")
	if resp := h.report(tk.ID, a.ID, core.ReportProgress, "fixed both"); !resp.OK {
		t.Fatalf("report = %+v", resp)
	}
	ps = h.proposalsOf(tk.ID)
	if len(ps) != 2 || ps[0].State != core.ProposalSuperseded || ps[1].State != core.ProposalPending {
		t.Fatalf("proposals = %+v, want the first superseded by a pending second", ps)
	}
	if err := json.Unmarshal(ps[1].Payload, &pl); err != nil || pl.SHA != second {
		t.Fatalf("second payload = %+v %v, want %s", pl, err, second)
	}
}

// writeApplyGh puts a gh on a new PATH directory that records its arguments, its token and repository, and the
// branch's commit in the bare repository when it runs, then prints a pull request URL.
func writeApplyGh(t *testing.T, bare, branch string) (dir, record string) {
	t.Helper()
	dir = t.TempDir()
	record = filepath.Join(t.TempDir(), "gh.rec")
	script := "#!/bin/sh\n{ echo \"args=$*\"; echo \"GH_TOKEN=$GH_TOKEN\"; echo \"GH_REPO=$GH_REPO\"; " +
		"echo \"ref=$(git --git-dir=" + bare + " rev-parse --verify -q refs/heads/" + branch + ")\"; } > " + record +
		"\necho https://github.com/o/r/pull/9\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, record
}

// Applying a pull request applies the Ticket's pending push first: the branch is on GitHub before gh opens the pull
// request, with write tokens minted for each and revoked after, and both Proposals end applied. A push that fails fails
// the pull request with its error, and no pull request is opened.
func TestApplyPullAppliesPushFirst(t *testing.T) {
	h := newGHHarness(t, &fakeCreds{}, func(c *Config) { c.Policy = newShadowPolicy("o/r") })
	bare := git(t, h.work, "remote", "get-url", "origin")
	tk, a := h.accept("github:issuecomment:1", "o/r", 12, "")
	sha := h.commit(tk, "fix the typo")
	if resp := h.report(tk.ID, a.ID, core.ReportProgress, "fixed"); !resp.OK {
		t.Fatalf("report = %+v", resp)
	}
	ghDir, record := writeApplyGh(t, bare, branchName(tk))
	if code, _, errOut := h.ghPrCreate(tk, a, ghDir, "--title", "Fix the typo", "--body", "Done."); code != 0 {
		t.Fatalf("gh pr create: exit %d %s", code, errOut)
	}
	ps := h.proposalsOf(tk.ID)
	if len(ps) != 2 || ps[0].Kind != core.ProposalPush || ps[1].Kind != core.ProposalPull {
		t.Fatalf("proposals = %+v, want a push and a pull request", ps)
	}
	pull := ps[1]
	t.Setenv("PATH", ghDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	before := len(h.creds.asked())
	err := h.n.ApplyProposal(context.Background(), *pull)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := h.eng.SettleProposal(pull.ID, err); err != nil {
		t.Fatal(err)
	}
	if got := git(t, bare, "rev-parse", "refs/heads/"+branchName(tk)); got != sha {
		t.Fatalf("GitHub's %s = %s, want %s", branchName(tk), got, sha)
	}
	rec, rerr := os.ReadFile(record)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, want := range []string{"args=pr create --title Fix the typo --body Done. --head " + branchName(tk), "GH_REPO=o/r",
		"ref=" + sha, "GH_TOKEN=" + fakeToken} {
		if !strings.Contains(string(rec), want) {
			t.Errorf("gh got:\n%s\nwant %q", rec, want)
		}
	}
	if asked := h.creds.asked()[before:]; len(asked) != 2 || !asked[0] || !asked[1] {
		t.Errorf("apply asked write = %v, want two write tokens (the push's, the pull request's)", asked)
	}
	h.eventually("the write tokens revoked", func() bool { return len(h.creds.revokedTokens()) >= 2 })
	for _, p := range h.proposalsOf(tk.ID) {
		if p.State != core.ProposalApplied {
			t.Errorf("proposal %s (%s) = %s %q, want applied", p.ID, p.Kind, p.State, p.Error)
		}
	}

	// A push GitHub refuses (the branch moved there meanwhile): the pull request fails with its error, and gh never runs.
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	t2, a2 := h.accept("github:issuecomment:2", "o/r", 13, "")
	h.commit(t2, "another fix")
	if resp := h.report(t2.ID, a2.ID, core.ReportProgress, "fixed"); !resp.OK {
		t.Fatalf("report = %+v", resp)
	}
	git(t, h.work, "checkout", "-q", "-b", "diverged")
	git(t, h.work, "commit", "-q", "--allow-empty", "-m", "someone else's")
	git(t, h.work, "push", "-q", "origin", "diverged:refs/heads/"+branchName(t2))
	if code, _, errOut := h.ghPrCreate(t2, a2, ghDir, "--title", "Another"); code != 0 {
		t.Fatalf("gh pr create: exit %d %s", code, errOut)
	}
	ps = h.proposalsOf(t2.ID)
	if len(ps) != 2 {
		t.Fatalf("proposals = %+v", ps)
	}
	err = h.n.ApplyProposal(context.Background(), *ps[1])
	if err == nil {
		t.Fatal("apply succeeded over a refused push")
	}
	if err := h.eng.SettleProposal(ps[1].ID, err); err != nil {
		t.Fatal(err)
	}
	ps = h.proposalsOf(t2.ID)
	if ps[0].State != core.ProposalFailed || ps[1].State != core.ProposalFailed || ps[1].Error != ps[0].Error || ps[0].Error == "" {
		t.Fatalf("after a refused push: push %s %q, pull %s %q; want both failed with the push's error",
			ps[0].State, ps[0].Error, ps[1].State, ps[1].Error)
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("gh ran after the push failed")
	}
}
