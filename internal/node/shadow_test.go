package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/sources/github"
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
			want := reason
			if rc.Restricted { // held because it is restricted: its own words
				want = restrictedPushReason
			}
			d := Evaluate(rc, "Bash", bash(cmd))
			if d.Verdict != VerdictDeny || d.Reason != want || d.Rule != ruleShadow {
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
	code, out, errOut := h.ghPrCreate(tk, a, ghDir, "--title", "T", "--body-file", body, "-B", "main", "--draft")
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
	if got, want := string(ps[0].Payload), `{"title":"T","body":"Fixes the typo.\n\nDetails here.","base":"main","draft":true}`; got != want {
		t.Fatalf("payload = %s, want %s", got, want)
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

// The held pull request takes only its title, body (or body file), base and draft: anything else is refused by
// `athena gh`, and the node refuses a propose request with any other field or with fields it would not hold, whoever
// sends it. Nothing is proposed and gh never runs.
func TestHostileProposeRefused(t *testing.T) {
	h := newGHHarness(t, &fakeCreds{}, func(c *Config) { c.Policy = newShadowPolicy("o/r") })
	tk, a := h.accept("github:issuecomment:1", "o/r", 12, "")
	ghDir, record := writeFakeGh(t)
	for _, args := range [][]string{
		{"--title", "x", "--recover", "f"}, {"--title", "x", "-H", "evil"}, {"--title", "x", "--head=evil"},
		{"--title", "x", "-R", "o/x"}, {"--title", "x", "-Ro/x"}, {"--title", "x", "--repo=o/x"}, {"--fill"},
		{"--title", "x", "extra"}, {"--body", "no title"}, {"--title", "x", "--web"}, {"--title", "x", "-B", "../main"},
	} {
		code, _, errOut := h.ghPrCreate(tk, a, ghDir, args...)
		if code != 1 || errOut == "" {
			t.Errorf("gh pr create %q: exit %d %q, want a refusal", args, code, errOut)
		}
	}
	// Sent to the node directly, past the shim.
	pem := filepath.Join(h.cfg.StateDir, "app.pem")
	for _, raw := range []string{
		`{"op":"propose","ticket":%q,"assignment":%q,"args":["pr","create","--title","x","--body-file",` + strconvQuote(pem) + `]}`,
		`{"op":"propose","ticket":%q,"assignment":%q,"pull":{"title":"x","head":"evil"}}`,
		`{"op":"propose","ticket":%q,"assignment":%q,"pull":{"title":"x","bodyFile":"/etc/passwd"}}`,
		`{"op":"propose","ticket":%q,"assignment":%q,"pull":{"title":""}}`,
		`{"op":"propose","ticket":%q,"assignment":%q,"pull":{"title":"x\nmore"}}`,
		`{"op":"propose","ticket":%q,"assignment":%q,"pull":{"title":"x","base":"-main"}}`,
		`{"op":"propose","ticket":%q,"assignment":%q,"pull":{"title":"x","base":"a..b"}}`,
		`{"op":"propose","ticket":%q,"assignment":%q,"pull":{"title":"x","body":"` + strings.Repeat("y", maxPullBody+1) + `"}}`,
		`{"op":"propose","ticket":%q,"assignment":%q}`,
	} {
		var resp ProposeResponse
		if err := callNode(h.cfg.SocketPath, json.RawMessage(fmt.Sprintf(raw, tk.ID, a.ID)), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.OK || resp.Held || resp.Error == "" {
			t.Errorf("%.120s = %+v, want refused", raw, resp)
		}
	}
	if ps := h.proposalsOf(tk.ID); len(ps) != 0 {
		t.Fatalf("proposals = %+v, want none", ps)
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("gh ran")
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// Applying a pull request applies the Ticket's pending push first: the branch is on GitHub before the pull request
// opens, through the API with exactly the held fields and the Ticket's branch as head; the push's write token is minted
// for it and revoked after, and both Proposals end applied. A push that fails fails the pull request with its error,
// and no pull request is opened.
func TestApplyPullAppliesPushFirst(t *testing.T) {
	creds := &fakeCreds{}
	h := newGHHarness(t, creds, func(c *Config) { c.Policy = newShadowPolicy("o/r") })
	bare := git(t, h.work, "remote", "get-url", "origin")
	var refAtPull []string
	creds.onPull = func(repo, head string) {
		out, _ := exec.Command("git", "--git-dir="+bare, "rev-parse", "--verify", "-q", "refs/heads/"+head).Output()
		refAtPull = append(refAtPull, strings.TrimSpace(string(out)))
	}
	tk, a := h.accept("github:issuecomment:1", "o/r", 12, "")
	sha := h.commit(tk, "fix the typo")
	if resp := h.report(tk.ID, a.ID, core.ReportProgress, "fixed"); !resp.OK {
		t.Fatalf("report = %+v", resp)
	}
	ghDir, record := writeFakeGh(t)
	if code, _, errOut := h.ghPrCreate(tk, a, ghDir, "--title", "Fix the typo", "--body", "Done.", "--base", "trunk"); code != 0 {
		t.Fatalf("gh pr create: exit %d %s", code, errOut)
	}
	ps := h.proposalsOf(tk.ID)
	if len(ps) != 2 || ps[0].Kind != core.ProposalPush || ps[1].Kind != core.ProposalPull {
		t.Fatalf("proposals = %+v, want a push and a pull request", ps)
	}
	pull := ps[1]
	before := len(creds.asked())
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
	want := []openedPull{{Repo: "o/r", Head: branchName(tk), Pull: HeldPull{Title: "Fix the typo", Body: "Done.", Base: "trunk"}}}
	if got := creds.opened(); !slices.Equal(got, want) || !slices.Equal(refAtPull, []string{sha}) {
		t.Fatalf("opened %+v with the branch at %v; want %+v with it at %s", got, refAtPull, want, sha)
	}
	if asked := creds.asked()[before:]; len(asked) != 1 || !asked[0] {
		t.Errorf("apply asked write = %v, want the push's write token", asked)
	}
	th, err := h.eng.Thread(github.ThreadKey("o/r", 9))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(th.Subscriptions, func(s core.Subscription) bool { return s.TicketID == tk.ID && s.Own }) {
		t.Errorf("thread = %+v, want the Ticket to own the opened pull request's", th)
	}
	h.eventually("the push's token revoked", func() bool { return len(creds.revokedTokens()) >= 1 })
	for _, p := range h.proposalsOf(tk.ID) {
		if p.State != core.ProposalApplied {
			t.Errorf("proposal %s (%s) = %s %q, want applied", p.ID, p.Kind, p.State, p.Error)
		}
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("gh ran to apply a pull request")
	}

	// A push GitHub refuses (the branch moved there meanwhile): the pull request fails with its error, none opens.
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
	if n := len(creds.opened()); n != 1 {
		t.Fatalf("%d pull requests opened, want only the first", n)
	}
}

// The push the node applies never reads the config a worker can write: a pushInsteadOf, a case-variant URL's
// curloptResolve and sslVerify, a core.askPass and a pre-push hook planted in the clone's config and the worktree's
// config.worktree change nothing. The commit lands on the real remote, and neither the askPass nor the hook runs. A
// second apply of the same Proposal (a run after one that never settled) goes nowhere.
func TestApplyPushIgnoresHostileConfig(t *testing.T) {
	creds := &fakeCreds{}
	h := newGHHarness(t, creds, func(c *Config) { c.Policy = newShadowPolicy("o/r") })
	bare := git(t, h.work, "remote", "get-url", "origin")
	tk, a := h.accept("github:issuecomment:1", "o/r", 12, "")
	sha := h.commit(tk, "fix the typo")
	if resp := h.report(tk.ID, a.ID, core.ReportProgress, "fixed"); !resp.OK {
		t.Fatalf("report = %+v", resp)
	}
	ps := h.proposalsOf(tk.ID)
	if len(ps) != 1 {
		t.Fatalf("proposals = %+v", ps)
	}

	evil := t.TempDir()
	git(t, evil, "init", "-q", "--bare", filepath.Join(evil, "o", "r.git"))
	marks := t.TempDir()
	askPass := filepath.Join(marks, "askpass.sh")
	hooks := filepath.Join(marks, "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{askPass: "touch " + marks + "/askpass-ran\necho x\n",
		filepath.Join(hooks, "pre-push"): "touch " + marks + "/hook-ran\n"} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	clone := CloneDir(h.cfg.StateDir, "o/r")
	admin := worktreeAdmin(filepath.Join(clone, ".git"), TicketDir(h.cfg.WorkRoot, tk))
	hostile := [][2]string{
		{"url.file://" + evil + "/.pushInsteadOf", "https://github.com/"},
		{"url.file://" + evil + "/o/r.git.pushInsteadOf", "file://"},
		{"http.https://github.com/O/R.git.curloptResolve", "github.com:443:127.0.0.1"},
		{"http.https://github.com/O/R.git.sslVerify", "false"},
		{"core.askPass", askPass},
		{"core.hooksPath", hooks},
	}
	for _, file := range []string{filepath.Join(clone, ".git", "config"), filepath.Join(admin, "config.worktree")} {
		for _, kv := range hostile {
			git(t, clone, "config", "--file", file, "--add", kv[0], kv[1])
		}
	}
	git(t, clone, "config", "--file", filepath.Join(clone, ".git", "config"), "extensions.worktreeConfig", "true")

	if err := h.n.ApplyProposal(context.Background(), *ps[0]); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := git(t, bare, "rev-parse", "refs/heads/"+branchName(tk)); got != sha {
		t.Fatalf("the real remote's %s = %s, want %s", branchName(tk), got, sha)
	}
	if out, err := exec.Command("git", "--git-dir="+filepath.Join(evil, "o", "r.git"), "for-each-ref").Output(); err != nil || len(out) != 0 {
		t.Fatalf("the planted remote got refs %q (%v)", out, err)
	}
	for _, m := range []string{"askpass-ran", "hook-ran"} {
		if _, err := os.Stat(filepath.Join(marks, m)); err == nil {
			t.Errorf("%s: the planted command ran", m)
		}
	}

	// Applied again before it was settled, as after a crash: nothing goes out.
	asked := len(creds.asked())
	err := h.n.ApplyProposal(context.Background(), *ps[0])
	if err == nil || err.Error() != "Athena stopped while applying this; check GitHub before applying again." {
		t.Fatalf("second apply = %v, want the started-before error", err)
	}
	if n := len(creds.asked()); n != asked {
		t.Fatalf("a second apply minted a token")
	}
}

// restrictAll is a Policy under which every trust class's work is restricted and no repository is in shadow mode.
type restrictAll struct{ fakeToolPolicy }

func (restrictAll) ToolPolicy(string) string { return "restricted" }

// A restricted session is held for the owner in a live repository too (R15): its tokens are read-only, its git push is
// denied with a reason that says why, its pull request and its branch become Proposals.
func TestRestrictedLiveTicketIsHeld(t *testing.T) {
	if !SandboxSupported() {
		t.Skip("no sandbox on this platform")
	}
	h := newGHHarness(t, &fakeCreds{}, func(c *Config) { c.Policy = restrictAll{} })
	tk, a := h.accept("github:issuecomment:1", "o/r", 12, "")
	if got := h.gitCredential(tk.ID, a.ID, ownRequest); tokenOf(got) == "" {
		t.Fatalf("restricted session got no token: %q", got)
	}
	if asked := h.creds.asked(); len(asked) == 0 || slices.Contains(asked, true) {
		t.Fatalf("restricted ticket asked write = %v, want only read-only tokens", asked)
	}

	resp := h.hook(EventPreToolUse, tk.ID, a.ID, map[string]any{"tool_name": "Bash",
		"tool_input": map[string]any{"command": "git push origin HEAD:main"}})
	if resp.Verdict != VerdictDeny || resp.Reason != restrictedPushReason {
		t.Fatalf("push hook = %+v, want denied with %q", resp, restrictedPushReason)
	}

	ghDir, record := writeFakeGh(t)
	code, out, errOut := h.ghPrCreate(tk, a, ghDir, "--title", "T", "--body", "B")
	if code != 0 || strings.TrimSpace(out) != restrictedPullHeld {
		t.Fatalf("gh pr create = exit %d, out %q, err %q; want held with %q", code, out, errOut, restrictedPullHeld)
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("the real gh ran for a restricted session's pull request")
	}
	h.commit(tk, "a stranger's fix")
	if resp := h.report(tk.ID, a.ID, core.ReportProgress, "fixed"); !resp.OK {
		t.Fatalf("report = %+v", resp)
	}
	kinds := map[string]int{}
	for _, p := range h.proposalsOf(tk.ID) {
		if p.State == core.ProposalPending {
			kinds[p.Kind]++
		}
	}
	if kinds[core.ProposalPull] != 1 || kinds[core.ProposalPush] != 1 {
		t.Fatalf("pending proposals by kind = %v, want one pull request and one push", kinds)
	}
}
