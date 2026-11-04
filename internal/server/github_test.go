package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/effects"
	"github.com/danielpina1/athena/internal/metrics"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/sources/github"
	"github.com/danielpina1/athena/internal/store"
)

// fastEvaluate shortens the Evals' ticker for one test.
func fastEvaluate(t *testing.T) {
	prev := evaluateEvery
	evaluateEvery = 50 * time.Millisecond
	t.Cleanup(func() { evaluateEvery = prev })
}

// invocationEnvs returns the ATHENA_* environment of each fakeclaude start.
func (f fakeEnv) invocationEnvs(t *testing.T) []map[string]string {
	t.Helper()
	b, err := os.ReadFile(f.argsLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var inv struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal([]byte(line), &inv); err != nil {
			t.Fatalf("args log: %v", err)
		}
		out = append(out, inv.Env)
	}
	return out
}

// routerToken is the token of the latest router start, from its environment.
func routerToken(t *testing.T, f fakeEnv) string {
	t.Helper()
	var tok string
	eventually(t, "the router started", func() bool {
		for _, env := range f.invocationEnvs(t) {
			if env["ATHENA_ROLE"] == node.RoleRouter && env["ATHENA_ROUTER_TOKEN"] != "" {
				tok = env["ATHENA_ROUTER_TOKEN"]
			}
		}
		return tok != ""
	})
	return tok
}

func (s *testServer) threads() []ThreadView {
	s.t.Helper()
	var list []ThreadView
	s.ok(Request{Op: OpThreads}, &list)
	return list
}

func (s *testServer) health() HealthView {
	s.t.Helper()
	var h HealthView
	s.ok(Request{Op: OpHealth}, &h)
	return h
}

func metricCount(snap metrics.Snapshot, name string) int64 {
	var n int64
	for _, h := range snap.Histograms {
		if h.Name == name {
			n += h.Count
		}
	}
	return n
}

// The whole GitHub path with fakes (ruling R6): a fake GitHub, fakeclaude as both the router and the worker, and the
// router's and worker's tool calls made through the node socket as their MCP servers would. A mention from the
// allowed account becomes a candidate the router accepts with an acknowledgement; the acknowledgement is posted; the
// worker reports ready with its pull request; the pull request merges and the Ticket ends merged. Exactly one comment
// is posted, the acknowledgement.
func TestServerGitHubFlowWithFakes(t *testing.T) {
	fake := newFakeEnv(t, openTurnFixture(t))
	fastEvaluate(t)
	g := newFakeGitHub(t)
	dir := newStateDir(t)
	g.configure(t, dir, 600)
	// The node uses an existing worktree directory as it is: made ahead, no git ever reaches github.com.
	if err := os.MkdirAll(node.TicketDir(filepath.Join(dir, "work"), core.Ticket{ID: "T-0001", Title: "Fix the flaky test"}), 0o700); err != nil {
		t.Fatal(err)
	}
	s := startServer(t, dir)
	token := routerToken(t, fake)
	eventually(t, "router running and github healthy", func() bool {
		h := s.health()
		return h.Router.State == "running" && len(h.Sources) == 1 && h.Sources[0].Name == "github" && h.Sources[0].Error == ""
	})

	cid := g.addComment(3, "o", "@athena-bot the login test is flaky, please fix it")
	triggerID := fmt.Sprintf("github:issuecomment:%d", cid)
	thread := github.ThreadKey(fakeRepo, 3)
	eventually(t, "the candidate reached the router", func() bool {
		for _, th := range s.threads() {
			if th.Key == thread && th.Eval != nil && th.Eval.DeliveredAt != nil {
				return true
			}
		}
		return false
	})

	args, _ := json.Marshal(map[string]any{"decisions": []map[string]any{{"trigger_id": triggerID, "kind": "accept",
		"title": "Fix the flaky test", "ack": "On it: I will fix the flaky login test."}}})
	var rr node.RouterResponse
	nodeCall(t, dir, node.RouterRequest{Op: node.OpRouter, Token: token, Tool: "route", Args: args}, &rr)
	if !rr.OK || !strings.Contains(string(rr.Result), `"decision":"accept"`) || strings.Contains(string(rr.Result), "refused") {
		t.Fatalf("route = %+v %s", rr, rr.Result)
	}
	eventually(t, "the acknowledgement posted", func() bool { return len(g.postedComments()) == 1 })
	if c := g.postedComments()[0]; c.Number != 3 || !strings.HasPrefix(c.Body, "On it: I will fix the flaky login test.") {
		t.Fatalf("posted = %+v, want the acknowledgement on #3", c)
	}

	a := s.running("T-0001")
	if d := s.detail("T-0001"); d.Repo != fakeRepo {
		t.Fatalf("detail repo = %q", d.Repo)
	}
	envs := fake.invocationEnvs(t)
	worker := envs[len(envs)-1]
	if worker["ATHENA_TICKET"] != "T-0001" || worker["ATHENA_GITHUB_REPOSITORY"] != fakeRepo || worker["ATHENA_GITHUB_ACCOUNT"] != fakeBot ||
		worker["ATHENA_GITHUB_COMMIT_EMAIL"] != fmt.Sprintf("%d+%s@users.noreply.github.com", fakeBotID, fakeBot) || worker["ATHENA_GITHUB_NUMBER"] != "3" {
		t.Fatalf("worker env = %v", worker)
	}
	for k, v := range worker {
		if strings.Contains(v, "ghs_") {
			t.Fatalf("worker env %s holds a token", k)
		}
	}
	var resp node.Response
	nodeCall(t, dir, node.ReportRequest{Op: node.OpReport, Ticket: "T-0001", Assignment: a.ID, Status: "ready", Summary: "fixed the race", Pull: 7}, &resp)
	if !resp.OK {
		t.Fatalf("report = %+v", resp)
	}
	if d := s.detail("T-0001"); d.Ended != nil {
		t.Fatalf("ticket ended before its pull request merged: %+v", d.Ended)
	}
	g.mergePull(7, "athena/t-0001")
	eventually(t, "the ticket ended merged", func() bool { return s.detail("T-0001").Stage == "ended:merged" })

	// A few more polls: nothing else is posted, by any path.
	time.Sleep(1500 * time.Millisecond)
	if posted := g.postedComments(); len(posted) != 1 {
		t.Fatalf("posted %d comments, want only the acknowledgement: %+v", len(posted), posted)
	}
	var posts int
	for _, r := range g.requestLog() {
		if strings.HasPrefix(r, "POST /repos/") {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("%d POSTs to repositories, want 1: %v", posts, g.requestLog())
	}

	var snap metrics.Snapshot
	s.ok(Request{Op: OpMetrics, Window: "start"}, &snap)
	for _, name := range []string{"trigger_intake_seconds", "eval_seconds", "reply_send_seconds"} {
		if metricCount(snap, name) == 0 {
			t.Errorf("no %s sample", name)
		}
	}
	var requests, rate bool
	for _, c := range snap.Counters {
		requests = requests || (c.Name == "github_requests_total" && c.Labels["status"] == "200" && c.Value > 0)
	}
	for _, gv := range snap.Gauges {
		rate = rate || (gv.Name == "github_rate_remaining" && gv.Value == 4321)
	}
	if !requests || !rate {
		t.Errorf("github metrics missing: requests %v, rate %v", requests, rate)
	}
	if h := s.health(); h.GitHubRateRemaining == nil || *h.GitHubRateRemaining != 4321 || h.Router.Session == "" {
		t.Errorf("health = %+v", h)
	}

	// athena task --repo: an owner Ticket on a configured repository, in its configured spelling.
	if r, err := call(dir, Request{Op: OpSubmit, Text: "x", Repo: "o/elsewhere"}); err != nil || r.OK || !strings.Contains(r.Error, "not configured") {
		t.Fatalf("task on an unconfigured repo = %+v %v", r, err)
	}
	if err := os.MkdirAll(node.TicketDir(filepath.Join(dir, "work"), core.Ticket{ID: "T-0002", Title: "Bump the deps"}), 0o700); err != nil {
		t.Fatal(err)
	}
	var sub struct {
		ID string `json:"id"`
	}
	s.ok(Request{Op: OpSubmit, Text: "Bump the deps", Repo: "O/R"}, &sub)
	s.running(sub.ID)
	offline(t, dir, func(e *core.Engine) {
		tk, _, _ := e.Ticket(sub.ID)
		if tk.Repo != fakeRepo || tk.Mandate.Basis.Via != core.ViaOwner || len(tk.Channels) != 0 {
			t.Errorf("repo task = repo %q basis %+v channels %v", tk.Repo, tk.Mandate.Basis, tk.Channels)
		}
	})
	if posted := g.postedComments(); len(posted) != 1 {
		t.Fatalf("posted %d comments after the repo task, want still 1", len(posted))
	}
}

// A configured App that fails its check leaves the server running without the GitHub Source (the router runs, so
// relays queue); status says why.
func TestServerRunsWhenGitHubCheckFails(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	g := newFakeGitHub(t)
	g.repos = nil // the installation reaches nothing
	dir := newStateDir(t)
	g.configure(t, dir, 600)
	s := startServer(t, dir)
	h := s.health()
	if len(h.Sources) != 1 || !strings.Contains(h.Sources[0].Error, "does not reach o/r") || h.Router.State == "off" {
		t.Fatalf("health = %+v", h)
	}
	id := s.submit("Owner work still runs")
	s.running(id)
	if r, err := call(dir, Request{Op: OpSubmit, Text: "x", Repo: fakeRepo}); err != nil || r.OK || !strings.Contains(r.Error, "github is not running") {
		t.Fatalf("repo task without github = %+v, %v", r, err)
	}
}

func TestGithubCheckReportsBot(t *testing.T) {
	g := newFakeGitHub(t)
	dir := newStateDir(t)
	key := appKey(t)
	cfg := GitHubConfig{AppID: 1, Slug: "athena-bot", KeyFile: key, InstallationID: 777, Repos: []string{"O/R"}, APIURL: g.srv.URL}
	if err := SetupGitHub(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, filepath.Join(dir, ConfigFile)); m != 0o600 {
		t.Fatalf("config mode = %o", m)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, ConfigFile))
	if strings.Contains(string(raw), "PRIVATE KEY") || !strings.Contains(string(raw), key) {
		t.Fatalf("config holds the key or not its path:\n%s", raw)
	}
	got, err := CheckGitHub(context.Background(), dir, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bot != fakeBot || got.Account != "o" || len(got.Repos) != 1 || got.Repos[0] != fakeRepo {
		t.Fatalf("check = %+v", got)
	}
	if !strings.Contains(strings.Join(g.requestLog(), "\n"), "POST /app/installations/777/access_tokens") {
		t.Fatalf("no token minted: %v", g.requestLog())
	}

	// A configured repository the installation does not reach fails the check, with the reason.
	g.mu.Lock()
	g.repos = []string{"o/other"}
	g.mu.Unlock()
	if _, err := CheckGitHub(context.Background(), dir, "test", nil); err == nil || !strings.Contains(err.Error(), "does not reach O/R") {
		t.Fatalf("check with an unreachable repo = %v", err)
	}
	// So does a key file others may read, and setup refuses it.
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckGitHub(context.Background(), dir, "test", nil); err == nil || !strings.Contains(err.Error(), "0644") {
		t.Fatalf("check with a wide key = %v", err)
	}
	if err := SetupGitHub(dir, cfg); err == nil || !strings.Contains(err.Error(), "chmod 0600") {
		t.Fatalf("setup with a wide key = %v", err)
	}
	if _, err := CheckGitHub(context.Background(), newStateDir(t), "test", nil); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("check without config = %v", err)
	}
}

// The node's credentials come from the App: a token for the Ticket's repository alone with the workers' permissions,
// and the bot's identity, looked up once.
func TestCredentialsFromApp(t *testing.T) {
	g := newFakeGitHub(t)
	app, err := github.NewApp(github.AppConfig{AppID: 1, Slug: "athena-bot", KeyFile: appKey(t), InstallationID: 777, APIURL: g.srv.URL}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &appCredentials{app: app}
	tok, exp, err := c.Token(context.Background(), fakeRepo)
	if err != nil || !strings.HasPrefix(tok, "ghs_") || time.Until(exp) < 30*time.Minute {
		t.Fatalf("token = %q %v %v", tok, exp, err)
	}
	for i := 0; i < 3; i++ {
		login, email := c.Identity()
		if login != fakeBot || email != fmt.Sprintf("%d+%s@users.noreply.github.com", fakeBotID, fakeBot) {
			t.Fatalf("identity = %q %q", login, email)
		}
	}
	lookups := 0
	for _, r := range g.requestLog() {
		if r == "GET /users/athena-bot[bot]" {
			lookups++
		}
	}
	if lookups != 1 {
		t.Fatalf("bot looked up %d times, want once", lookups)
	}
}

// allScope listens to every Thread.
type allScope struct{}

func (allScope) Listens(string) bool { return true }

// offlineWith runs fn against the state dir's store with an Engine that has a Policy over the saved settings, a
// clock and a scope that listens everywhere, while no server runs.
func offlineWith(t *testing.T, stateDir string, now func() time.Time, fn func(*core.Engine, *store.Store)) {
	t.Helper()
	st, err := store.Open(filepath.Join(stateDir, "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ps := policy.NewStore(st, policy.Defaults(nil))
	cur, _, err := ps.Current()
	if err != nil {
		t.Fatal(err)
	}
	pol := policy.New(cur, allScope{}, 10*time.Minute)
	fn(core.NewEngineWith(st.Repos(), nopExec{}, core.Options{Now: now, Policy: func() core.Policy { return pol }, Scope: allScope{}}), st)
}

func saveSettings(t *testing.T, stateDir string, s policy.Settings) {
	t.Helper()
	st, err := store.Open(filepath.Join(stateDir, "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ps := policy.NewStore(st, policy.Defaults(nil))
	_, rev, err := ps.Current()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Save(s, rev, policy.OriginCLI); err != nil {
		t.Fatal(err)
	}
}

// mention is alice's comment that mentions the agent on issue n of o/r.
func mention(id string, n int, at time.Time) core.Trigger {
	thread := github.ThreadKey(fakeRepo, n)
	return core.Trigger{ID: id, Source: "github", Kind: core.KindMessage, Actor: core.ActorRef{Source: "github", ID: "alice"},
		Human: true, Scope: "comments", Addressed: core.AddressedMention, Threads: []string{thread}, At: at,
		Text:     "@athena-bot please look at #" + fmt.Sprint(n),
		Channels: []core.ChannelRef{{Source: "github", Thread: thread, Address: map[string]string{"repo": fakeRepo, "number": fmt.Sprint(n)}}}}
}

// Startup recovery of Evals: one delivered past its deadline expires at once (the restart did not reset its clock),
// one delivered within it keeps its deadline, an undelivered one keeps its waiting assessment effect (not queued
// again) and one whose effect went missing has it queued again.
func TestRecoveryKeepsDeliveredEvalDeadline(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	dir := newStateDir(t)
	allowAlice := policy.Defaults([]string{"alice"})
	saveSettings(t, dir, allowAlice)
	start := time.Now().Add(-20 * time.Minute)
	clock := start
	now := func() time.Time { return clock }
	evals := map[int]core.Evaluation{}
	var effectSeq map[string]int
	offlineWith(t, dir, now, func(e *core.Engine, st *store.Store) {
		// The settings saved above allow alice; offlineWith's Policy is over them.
		for n := 1; n <= 4; n++ {
			if _, err := e.Receive(mention(fmt.Sprintf("github:issuecomment:%d", n), n, clock)); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.EvaluateDue(); err != nil {
			t.Fatal(err)
		}
		for n := 1; n <= 4; n++ {
			th, err := e.Thread(github.ThreadKey(fakeRepo, n))
			if err != nil || th.Eval == nil {
				t.Fatalf("thread %d: %+v %v", n, th, err)
			}
			evals[n] = *th.Eval
		}
		// #1 reached the router 11 minutes ago (past the 10 minute timeout), #2 a minute ago; #3 and #4 never did.
		clock = time.Now().Add(-11 * time.Minute)
		if err := e.CandidateDelivered(evals[1].ID); err != nil {
			t.Fatal(err)
		}
		clock = time.Now().Add(-time.Minute)
		if err := e.CandidateDelivered(evals[2].ID); err != nil {
			t.Fatal(err)
		}
		fx := effects.New(st, nil)
		rows, err := fx.Pending(core.EffectAssessment)
		if err != nil {
			t.Fatal(err)
		}
		effectSeq = map[string]int{}
		for _, r := range rows {
			effectSeq[r.Key] = r.Seq
			if r.Key == evals[4].ID {
				if ok, err := fx.Complete(r.Kind, r.Key, r.Payload); err != nil || !ok {
					t.Fatalf("drop #4's effect: %v %v", ok, err)
				}
			}
		}
		if len(rows) != 4 {
			t.Fatalf("assessment effects = %d, want 4", len(rows))
		}
	})
	if err := SaveConfig(dir, FileConfig{EvalTimeoutSeconds: 600}); err != nil {
		t.Fatal(err)
	}

	s := startServer(t, dir)
	byKey := map[string]ThreadView{}
	eventually(t, "#1's eval expired at startup", func() bool {
		for _, th := range s.threads() {
			byKey[th.Key] = th
		}
		e := byKey[github.ThreadKey(fakeRepo, 1)].Eval
		return e != nil && e.ID != evals[1].ID
	})
	if e := byKey[github.ThreadKey(fakeRepo, 1)].Eval; e.DeliveredAt != nil {
		t.Errorf("#1's new eval = %+v, want a fresh undelivered one", e)
	}
	if e := byKey[github.ThreadKey(fakeRepo, 2)].Eval; e == nil || e.ID != evals[2].ID || e.DeliveredAt == nil ||
		e.DeliveredAt.Sub(time.Now().Add(-time.Minute)).Abs() > 5*time.Second {
		t.Errorf("#2's eval = %+v, want the same one, delivered a minute ago", e)
	}
	for _, n := range []int{3, 4} {
		if e := byKey[github.ThreadKey(fakeRepo, n)].Eval; e == nil || e.ID != evals[n].ID || e.DeliveredAt != nil {
			t.Errorf("#%d's eval = %+v, want the same one, undelivered", n, e)
		}
	}
	s.stop()

	st, err := store.Open(filepath.Join(dir, "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := effects.New(st, nil).Pending(core.EffectAssessment)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, r := range rows {
		got[r.Key] = r.Seq
	}
	if seq, ok := got[evals[3].ID]; !ok || seq != effectSeq[evals[3].ID] {
		t.Errorf("#3's effect = %d (present %v), want its original row %d", seq, ok, effectSeq[evals[3].ID])
	}
	if _, ok := got[evals[4].ID]; !ok {
		t.Errorf("#4's missing effect was not queued again: %v", got)
	}
	if len(rows) != len(got) {
		t.Errorf("duplicate assessment rows: %v", rows)
	}
}

// policy set saves a cli revision and tells each open Ticket the change bears on: here alice, whose request started
// the Ticket, may no longer start work.
func TestPolicySetNotifiesAffectedTicket(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	dir := newStateDir(t)
	saveSettings(t, dir, policy.Defaults([]string{"alice"}))
	var ticketID string
	offlineWith(t, dir, time.Now, func(e *core.Engine, _ *store.Store) {
		tr := mention("github:issuecomment:1", 1, time.Now())
		if _, err := e.Receive(tr); err != nil {
			t.Fatal(err)
		}
		if err := e.EvaluateDue(); err != nil {
			t.Fatal(err)
		}
		r, err := e.Decide(core.DecisionInput{TriggerID: tr.ID, Kind: core.DecisionAccept, Title: "Look at it"})
		if err != nil || r.Ticket == nil {
			t.Fatalf("accept = %+v %v", r, err)
		}
		ticketID = r.Ticket.ID
	})
	s := startServer(t, dir)
	s.running(ticketID)

	var show PolicyView
	s.ok(Request{Op: OpPolicyShow}, &show)
	if show.Revision != 1 || show.Version != policy.Version(policy.Defaults([]string{"alice"})) ||
		len(show.Settings.Sources["github"].Allowed) != 1 {
		t.Fatalf("show = %+v", show)
	}

	if r, err := call(dir, Request{Op: OpPolicySet, Policy: json.RawMessage(`{"mode":"sometimes"}`)}); err != nil || r.OK || !strings.Contains(r.Error, "mode") {
		t.Fatalf("invalid settings = %+v %v", r, err)
	}
	next := policy.Defaults([]string{"bob"})
	raw, _ := json.Marshal(next)
	var res PolicySetResult
	s.ok(Request{Op: OpPolicySet, Policy: raw}, &res)
	if res.Revision != 2 || res.Version != policy.Version(next) || len(res.Notified) != 1 || res.Notified[0] != ticketID {
		t.Fatalf("set = %+v, want revision 2 telling %s", res, ticketID)
	}
	s.ok(Request{Op: OpPolicyShow}, &show)
	if show.Revision != 2 || show.Settings.Sources["github"].Allowed[0] != "bob" {
		t.Fatalf("show after set = %+v", show)
	}
	var notes int
	eventually(t, "the notice reached the ticket", func() bool {
		notes = 0
		offline(t, dir, func(e *core.Engine) {
			tk, _, _ := e.Ticket(ticketID)
			for _, in := range tk.Inputs {
				if in.Kind == core.InputNote && strings.Contains(in.Text, "may no longer start work") {
					notes++
				}
			}
		})
		return notes == 1
	})
	s.ok(Request{Op: OpPolicyShow}, &show)
	// The output of `policy show` is accepted as it is (revision and version are ignored), and unknown fields are
	// refused at either level.
	shown, _ := json.Marshal(show)
	var same PolicySetResult
	s.ok(Request{Op: OpPolicySet, Policy: shown}, &same)
	if same.Version != show.Version {
		t.Fatalf("policy show fed to policy set = %+v, want version %s", same, show.Version)
	}
	for _, bad := range []string{`{"revision":1,"settings":{"mode":"observe","bogus":1}}`, `{"settings":{"mode":"observe"},"bogus":1}`} {
		if r, err := call(dir, Request{Op: OpPolicySet, Policy: json.RawMessage(bad)}); err != nil || r.OK || !strings.Contains(r.Error, "bogus") {
			t.Fatalf("settings %s = %+v %v, want refused", bad, r, err)
		}
	}
	s.stop()
	st, err := store.Open(filepath.Join(dir, "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hist, err := policy.NewStore(st, policy.Defaults(nil)).History(0)
	if err != nil || len(hist) != 3 || hist[0].Origin != policy.OriginCLI || hist[0].Revision != 3 {
		t.Errorf("history = %+v %v", hist, err)
	}
}

// fastCheckRetry shortens the background retry of a failed GitHub check for one test.
func fastCheckRetry(t *testing.T) {
	first, max := checkRetryFirst, checkRetryMax
	checkRetryFirst, checkRetryMax = 100*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { checkRetryFirst, checkRetryMax = first, max })
}

func workerStarts(t *testing.T, f fakeEnv, ticket string) int {
	n := 0
	for _, env := range f.invocationEnvs(t) {
		if env["ATHENA_TICKET"] == ticket {
			n++
		}
	}
	return n
}

// Fix round 1: GitHub unreachable at boot (the startup check fails) does not fail the open GitHub Tickets. The node
// has the App's credentials all the same; a launch that cannot get them waits and retries, and the check is retried
// in the background: once it passes, the Source starts and the Ticket runs again.
func TestGitHubTicketSurvivesFailingCheck(t *testing.T) {
	fake := newFakeEnv(t, openTurnFixture(t))
	fastCheckRetry(t)
	g := newFakeGitHub(t)
	dir := newStateDir(t)
	g.configure(t, dir, 600)
	if err := os.MkdirAll(node.TicketDir(filepath.Join(dir, "work"), core.Ticket{ID: "T-0001", Title: "Bump the deps"}), 0o700); err != nil {
		t.Fatal(err)
	}
	fastNode := func(c *Config) { c.Node.CredentialRetry = 50 * time.Millisecond }
	s := startServerWith(t, dir, fastNode)
	var sub struct {
		ID string `json:"id"`
	}
	s.ok(Request{Op: OpSubmit, Text: "Bump the deps", Repo: fakeRepo}, &sub)
	s.running(sub.ID) // mid-work when the server stops: interrupted
	s.stop()

	g.down.Store(true)
	s = startServerWith(t, dir, fastNode)
	if h := s.health(); len(h.Sources) != 1 || h.Sources[0].Error == "" {
		t.Fatalf("health with github down = %+v", h)
	}
	time.Sleep(500 * time.Millisecond)
	d := s.detail(sub.ID)
	if r := d.Assignments[len(d.Assignments)-1].Report; r != nil || strings.HasPrefix(d.Stage, "person") || d.Ended != nil {
		t.Fatalf("with github down: stage %s, report %+v; want the ticket waiting, not failed", d.Stage, r)
	}
	if n := workerStarts(t, fake, sub.ID); n != 1 {
		t.Fatalf("worker started %d times while github is down, want 1 (the first run only)", n)
	}

	g.down.Store(false)
	eventually(t, "the check passed late and the source runs", func() bool {
		h := s.health()
		return len(h.Sources) == 1 && h.Sources[0].Error == ""
	})
	eventually(t, "the ticket's worker started again", func() bool { return workerStarts(t, fake, sub.ID) == 2 })
	s.running(sub.ID)
	if d := s.detail(sub.ID); d.Assignments[len(d.Assignments)-1].Report != nil {
		t.Fatalf("report = %+v, want none", d.Assignments[len(d.Assignments)-1].Report)
	}
}

// R30 M4: the bot the startup check fetched seeds the credentials: Identity needs no lookup of its own.
func TestCredentialsSeededByCheck(t *testing.T) {
	g := newFakeGitHub(t)
	cfg := &GitHubConfig{AppID: 1, Slug: "athena-bot", KeyFile: appKey(t), InstallationID: 777, Repos: []string{fakeRepo}, APIURL: g.srv.URL}
	gh := setupGitHub(cfg, "test", nil, nil, discardLogger())
	if gh.err != nil {
		t.Fatal(gh.err)
	}
	g.down.Store(true) // any lookup now would fail
	login, email := gh.creds.Identity()
	if login != fakeBot || email != fmt.Sprintf("%d+%s@users.noreply.github.com", fakeBotID, fakeBot) {
		t.Fatalf("identity = %q %q, want the bot the check fetched", login, email)
	}
	n := 0
	for _, r := range g.requestLog() {
		if r == "GET /users/athena-bot[bot]" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("bot looked up %d times, want once (by the check)", n)
	}
}

// A repository on github.receiveAll receives every message: an allowed account's comment that does not mention the App
// takes the listedUnaddressed cell (inferred by default) and reaches the router as a candidate, instead of being
// refused not_received.
func TestReceiveAllAdmitsUnaddressed(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	fastEvaluate(t)
	g := newFakeGitHub(t)
	dir := newStateDir(t)
	g.configure(t, dir, 600)
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.GitHub.ReceiveAll = []string{"O/R"}
	if err := SaveConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	s := startServer(t, dir)
	eventually(t, "router running and github healthy", func() bool {
		h := s.health()
		return h.Router.State == "running" && len(h.Sources) == 1 && h.Sources[0].Error == ""
	})
	g.addComment(3, "o", "the login test is flaky again")
	thread := github.ThreadKey(fakeRepo, 3)
	eventually(t, "the unaddressed comment reached the router", func() bool {
		for _, th := range s.threads() {
			if th.Key == thread && th.Eval != nil && th.Eval.DeliveredAt != nil {
				return true
			}
		}
		return false
	})
}

// A reply the Channel refused for good (a missing App permission, say) is dropped, and health shows it for a day.
func TestHealthShowsRefusedReplies(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := &control{st: st}
	if h := c.health(); len(h.Effects) != 0 {
		t.Fatalf("effects = %v, want none", h.Effects)
	}
	at := time.Now()
	for i, id := range []string{"r1", "r2"} {
		r := &core.Receipt{ID: id, TicketID: "T-0001", Purpose: core.PurposeAttention, Text: "Which page?",
			Conversation: "github:o/r#3", State: core.ReceiptDropped, DroppedReason: core.DropRefused, DroppedAt: &at,
			LastError: "github: POST /repos/o/r/issues/3/comments: 403 Resource not accessible by integration", Seq: i + 1}
		if err := st.Tx(func(tx *store.Tx) error { return tx.PutReceipt(r) }); err != nil {
			t.Fatal(err)
		}
	}
	got := c.health().Effects[core.EffectReply]
	if !strings.Contains(got, "r2") || !strings.Contains(got, "github:o/r#3") || !strings.Contains(got, "403 Resource not accessible") ||
		!strings.Contains(got, "1 more") {
		t.Fatalf("reply health = %q, want the latest refusal and the count", got)
	}
}

// Minor 4 (R36): while no Policy is saved and the startup check has not passed, nobody is known to be allowed, so
// nothing is gated: a Trigger queued before the boot waits instead of being recorded ignored, and it is evaluated
// once the check passes, with the installation's account allowed.
func TestNoGatingBeforeTheCheckPasses(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	fastEvaluate(t)
	fastCheckRetry(t)
	g := newFakeGitHub(t)
	dir := newStateDir(t)
	g.configure(t, dir, 600)
	tr := mention("github:issuecomment:9", 3, time.Now())
	tr.Actor.ID = "o" // the installation's account: allowed by the default Policy once the check knows it
	offlineWith(t, dir, time.Now, func(e *core.Engine, _ *store.Store) {
		if _, err := e.Receive(tr); err != nil {
			t.Fatal(err)
		}
	})
	g.down.Store(true)
	s := startServer(t, dir)
	time.Sleep(400 * time.Millisecond) // several evaluation ticks
	var list []DecisionView
	s.ok(Request{Op: OpDecisions}, &list)
	if len(list) != 0 {
		t.Fatalf("decisions before the check passed = %+v, want none", list)
	}
	thread := github.ThreadKey(fakeRepo, 3)
	g.down.Store(false)
	eventually(t, "the trigger evaluated once the check passed", func() bool {
		for _, th := range s.threads() {
			if th.Key == thread && th.Eval != nil {
				return true
			}
		}
		return false
	})
	s.ok(Request{Op: OpDecisions}, &list)
	for _, d := range list {
		if d.Kind == core.DecisionIgnore {
			t.Fatalf("decisions = %+v: the trigger was ignored", list)
		}
	}
}
