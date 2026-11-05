package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/sources/github"
	"github.com/danielpina1/athena/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	botLogin = "athena-app[bot]"
	botEmail = "4242+athena-app[bot]@users.noreply.github.com"
)

// fakeCreds is the server's Credentials port: a fixed token per repository, counting its requests. fail makes that
// many requests fail with failErr first; noIdentity hides the bot while set.
type fakeCreds struct {
	life       time.Duration
	calls      atomic.Int32
	fail       atomic.Int32
	failErr    error
	noIdentity atomic.Bool
	mu         sync.Mutex
	repos      []string
	revoked    []string
	revokeErr  error
}

func (f *fakeCreds) Token(_ context.Context, repo string) (string, time.Time, error) {
	n := f.calls.Add(1)
	f.mu.Lock()
	f.repos = append(f.repos, repo)
	f.mu.Unlock()
	if f.fail.Add(-1) >= 0 {
		return "", time.Time{}, f.failErr
	}
	life := f.life
	if life == 0 {
		life = time.Hour
	}
	return fmt.Sprintf("%s%d", fakeToken, n), time.Now().Add(life), nil
}

// Revoke records a revoked token; revokeErr makes it fail.
func (f *fakeCreds) Revoke(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, token)
	return f.revokeErr
}

func (f *fakeCreds) revokedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revoked...)
}

func (f *fakeCreds) Identity() (string, string) {
	if f.noIdentity.Load() {
		return "", ""
	}
	return botLogin, botEmail
}

// listenAll is a SourceScope that listens to every Thread.
type listenAll struct{}

func (listenAll) Listens(string) bool { return true }

// ghHarness is a harness whose Engine has the default Policy (alice may ask), whose node has creds, and whose
// https://github.com/ is a local bare repository (fakeGitHub). logs holds the node's log lines.
type ghHarness struct {
	*harness
	creds *fakeCreds
	work  string
	logs  *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newGHHarness(t *testing.T, creds *fakeCreds, mod func(*Config)) *ghHarness {
	t.Helper()
	root, work := fakeGitHub(t, "o/r")
	addFakeRepo(t, root, "o/s")
	logs := &syncBuffer{}
	pol := policy.New(policy.Defaults([]string{"alice"}), listenAll{}, time.Minute)
	h := newHarnessWith(t, func(c *Config) {
		if creds != nil {
			c.Credentials = creds
		}
		c.Logger = slog.New(slog.NewJSONHandler(logs, nil))
		if mod != nil {
			mod(c)
		}
	}, func(st *store.Store, n *Node, emit func(core.Event)) *core.Engine {
		return core.NewEngineWith(st.Repos(), n, core.Options{Emit: emit, Policy: func() core.Policy { return pol }, Scope: listenAll{}})
	})
	return &ghHarness{harness: h, creds: creds, work: work, logs: logs}
}

// addFakeRepo adds another bare repository, a copy of o/r, under the root fakeGitHub made.
func addFakeRepo(t *testing.T, root, repo string) {
	t.Helper()
	bare := filepath.Join(root, repo+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, root, "clone", "-q", "--bare", filepath.Join(root, "o", "r.git"), bare)
}

// accept receives alice's request on repo#n (an Issue, or a pull request with head) and accepts it; it returns the
// running Ticket and Assignment.
func (h *ghHarness) accept(id, repo string, n int, head string) (core.Ticket, core.Assignment) {
	h.t.Helper()
	thread := fmt.Sprintf("github:%s#%d", repo, n)
	meta := map[string]string{"repo": repo, "number": fmt.Sprint(n), "kind": "issue",
		"url": fmt.Sprintf("https://github.com/%s/issues/%d#issuecomment-1", repo, n)}
	if head != "" {
		meta["kind"], meta["head"] = "pull", head
	}
	tr := core.Trigger{ID: id, Source: "github", Kind: core.KindMessage, Actor: core.ActorRef{Source: "github", ID: "alice"},
		Human: true, Addressed: core.AddressedMention, Scope: "comments", Threads: []string{thread}, At: time.Now(),
		Text: "@athena please fix the README typo", Meta: meta,
		Channels: []core.ChannelRef{{Source: "github", Thread: thread, Address: map[string]string{"repo": repo, "number": fmt.Sprint(n)}}}}
	if got, err := h.eng.Receive(tr); err != nil || got != core.ReceiveStored {
		h.t.Fatalf("Receive: %s %v", got, err)
	}
	if err := h.eng.EvaluateDue(); err != nil {
		h.t.Fatal(err)
	}
	r, err := h.eng.Decide(core.DecisionInput{TriggerID: id, Kind: core.DecisionAccept, Title: "Fix " + id})
	if err != nil || r.Ticket == nil {
		h.t.Fatalf("accept: %+v %v", r, err)
	}
	return h.running(r.Ticket.ID)
}

// gitCredential runs `athena git-credential get` as the session of ticket and assignment with git's request.
func (h *ghHarness) gitCredential(ticket, assignment, request string) string {
	h.t.Helper()
	var out, errb bytes.Buffer
	env := []string{"ATHENA_SOCKET=" + h.cfg.SocketPath, "ATHENA_TICKET=" + ticket, "ATHENA_ASSIGNMENT=" + assignment}
	if code := RunGitCredential([]string{"get"}, strings.NewReader(request), &out, &errb, env); code != 0 {
		h.t.Fatalf("git-credential exit %d: %s", code, errb.String())
	}
	return out.String()
}

func TestCredentialOnlyOwnRepo(t *testing.T) {
	h := newGHHarness(t, &fakeCreds{}, nil)
	tk, a := h.accept("github:issuecomment:1", "o/r", 12, "")
	if tk.Repo != "o/r" {
		t.Fatalf("ticket repo = %q", tk.Repo)
	}
	want := "username=x-access-token\npassword=" + fakeToken + "1\n"
	for _, req := range []string{
		"protocol=https\nhost=github.com\npath=o/r.git\n",
		"protocol=https\nhost=github.com\npath=O/R\n\n",
		"protocol=https\nhost=GitHub.com\npath=o/r.git\nwwwauth[]=Basic\n",
	} {
		if got := h.gitCredential(tk.ID, a.ID, req); got != want {
			t.Errorf("request %q: got %q, want the token", req, got)
		}
	}
	for _, req := range []string{
		"protocol=https\nhost=github.com\npath=o/other.git\n",
		"protocol=https\nhost=github.com\npath=o/s.git\n",
		"protocol=https\nhost=github.com\n",
		"protocol=https\nhost=gitlab.com\npath=o/r.git\n",
		"protocol=http\nhost=github.com\npath=o/r.git\n",
		"protocol=https\nhost=github.com.evil.example\npath=o/r.git\n",
	} {
		if got := h.gitCredential(tk.ID, a.ID, req); got != "" {
			t.Errorf("request %q answered %q, want nothing", req, got)
		}
	}
	// store and erase do nothing and print nothing.
	for _, op := range []string{"store", "erase"} {
		var out bytes.Buffer
		env := []string{"ATHENA_SOCKET=" + h.cfg.SocketPath, "ATHENA_TICKET=" + tk.ID, "ATHENA_ASSIGNMENT=" + a.ID}
		in := "protocol=https\nhost=github.com\npath=o/r.git\nusername=x-access-token\npassword=" + fakeToken + "1\n"
		if code := RunGitCredential([]string{op}, strings.NewReader(in), &out, io.Discard, env); code != 0 || out.Len() != 0 {
			t.Errorf("%s: exit %d, printed %q", op, code, out.String())
		}
	}
	if n := h.creds.calls.Load(); n != 1 {
		t.Errorf("credentials asked %d times, want 1 (the clone's token is reused)", n)
	}
}

func TestCredentialRefusedForOtherSession(t *testing.T) {
	h := newGHHarness(t, &fakeCreds{}, nil)
	tk, a := h.accept("github:issuecomment:1", "o/r", 12, "")
	other, oa := h.accept("github:issuecomment:2", "o/s", 3, "")
	own := "protocol=https\nhost=github.com\npath=o/r.git\n"
	for _, ids := range [][2]string{{tk.ID, "not-the-assignment"}, {other.ID, a.ID}, {"T-9999", a.ID}, {"", ""}, {other.ID, oa.ID}} {
		if got := h.gitCredential(ids[0], ids[1], own); got != "" {
			t.Errorf("session %v got %q for o/r, want nothing", ids, got)
		}
	}
	// The other Ticket's own session gets its own repository's token.
	if got := h.gitCredential(other.ID, oa.ID, "protocol=https\nhost=github.com\npath=o/s.git\n"); !strings.Contains(got, "password="+fakeToken) {
		t.Errorf("o/s session for o/s = %q, want its token", got)
	}
	// A plain owner task has no repository and no token.
	plain := h.submit("Local task", "")
	_, pa := h.running(plain.ID)
	if got := h.gitCredential(plain.ID, pa.ID, own); got != "" {
		t.Errorf("an owner task got %q", got)
	}
	var resp CredentialResponse
	if err := callNode(h.cfg.SocketPath, CredentialRequest{Op: OpCredential, Ticket: plain.ID, Assignment: pa.ID, Purpose: CredentialGh}, &resp); err != nil || resp.OK {
		t.Errorf("gh token for an owner task: %+v %v", resp, err)
	}
}

// writeFakeGh puts a gh on a new PATH directory that records its arguments and the GitHub variables it got.
func writeFakeGh(t *testing.T) (dir, record string) {
	t.Helper()
	dir = t.TempDir()
	record = filepath.Join(t.TempDir(), "gh.rec")
	script := "#!/bin/sh\n{ echo \"args=$*\"; echo \"GH_TOKEN=$GH_TOKEN\"; echo \"GH_HOST=$GH_HOST\"; echo \"GH_CONFIG_DIR=$GH_CONFIG_DIR\"; " +
		"echo \"GH_PROMPT_DISABLED=$GH_PROMPT_DISABLED\"; echo \"GITHUB_TOKEN=$GITHUB_TOKEN\"; } > " + record + "\nexit 3\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, record
}

func TestGhRefusesAuth(t *testing.T) {
	h := newGHHarness(t, &fakeCreds{}, nil)
	tk, a := h.accept("github:issuecomment:1", "o/r", 12, "")
	ghDir, record := writeFakeGh(t)
	shims := filepath.Join(h.cfg.StateDir, ShimsDir)
	env := []string{"ATHENA_SOCKET=" + h.cfg.SocketPath, "ATHENA_TICKET=" + tk.ID, "ATHENA_ASSIGNMENT=" + a.ID,
		"PATH=" + shims + string(os.PathListSeparator) + ghDir, "GITHUB_TOKEN=owner-token"}
	gh := func(args ...string) (int, string) {
		var errb bytes.Buffer
		code := RunGh(args, strings.NewReader(""), io.Discard, &errb, env)
		return code, errb.String()
	}
	for _, args := range [][]string{{"auth", "status"}, {"auth", "token"}, {"--help", "auth"}, {"auth"},
		{"alias", "set", "x", "!env"}, {"extension", "install", "x/y"}, {"config", "set", "pager", "env"}} {
		if code, msg := gh(args...); code != 1 || !strings.Contains(msg, "not available") {
			t.Errorf("gh %v: exit %d %q, want a refusal", args, code, msg)
		}
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("the real gh ran for a refused subcommand")
	}
	// Any other subcommand runs the real gh (not the shim) with the token in gh's environment only.
	if code, msg := gh("pr", "create", "--fill"); code != 3 {
		t.Fatalf("gh pr create: exit %d %q, want gh's own exit 3", code, msg)
	}
	rec, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"args=pr create --fill", "GH_TOKEN=" + fakeToken + "1", "GH_HOST=github.com",
		"GH_CONFIG_DIR=" + filepath.Join(TicketDir(h.cfg.WorkRoot, tk), ".athena", "gh"), "GH_PROMPT_DISABLED=1", "GITHUB_TOKEN=\n"} {
		if !strings.Contains(string(rec)+"\n", want) {
			t.Errorf("gh got:\n%s\nwant %q", rec, want)
		}
	}
	// The shim exists, runs `athena gh`, and is skipped when the real gh is looked up.
	shim, err := os.ReadFile(filepath.Join(shims, "gh"))
	if err != nil || string(shim) != "#!/bin/sh\nexec \""+h.cfg.AthenaBin+"\" gh \"$@\"\n" {
		t.Fatalf("shim = %q, %v", shim, err)
	}
	if st, _ := os.Stat(filepath.Join(shims, "gh")); st.Mode().Perm() != 0o700 {
		t.Errorf("shim mode = %v, want 0700", st.Mode().Perm())
	}
	// Without a real gh the wrapper says so.
	var errb bytes.Buffer
	noGh := append(env[:3:3], "PATH="+shims)
	if code := RunGh([]string{"pr", "list"}, strings.NewReader(""), io.Discard, &errb, noGh); code != 1 || !strings.Contains(errb.String(), "not installed") {
		t.Errorf("without gh: exit %d %q", code, errb.String())
	}
}

// Review Focus 3: the token is in no session's environment, first message, transcript or log line.
func TestWorkerEnvHasNoToken(t *testing.T) {
	envLog := filepath.Join(t.TempDir(), "env.log")
	t.Setenv("ATHENA_FAKE_ENV_LOG", envLog)
	h := newGHHarness(t, &fakeCreds{}, nil)
	tk, a := h.accept("github:issuecomment:1", "o/r", 7, "fix-branch")
	// Ask for the token as the session would, so it is held and served while the logs are written.
	if got := h.gitCredential(tk.ID, a.ID, "protocol=https\nhost=github.com\npath=o/r.git\n"); !strings.Contains(got, fakeToken) {
		t.Fatalf("no token served: %q", got)
	}
	b, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, kv := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	dir := TicketDir(h.cfg.WorkRoot, tk)
	for k, want := range map[string]string{
		"ATHENA_GITHUB_REPOSITORY":   "o/r",
		"ATHENA_GITHUB_ACCOUNT":      botLogin,
		"ATHENA_GITHUB_COMMIT_EMAIL": botEmail,
		"ATHENA_GITHUB_NUMBER":       "7",
		"ATHENA_GITHUB_PR_HEAD":      "fix-branch",
		"ATHENA_GITHUB_KIND":         "pull",
		"GIT_AUTHOR_NAME":            botLogin,
		"GIT_AUTHOR_EMAIL":           botEmail,
		"GIT_COMMITTER_NAME":         botLogin,
		"GIT_COMMITTER_EMAIL":        botEmail,
		"GIT_CONFIG_COUNT":           "5",
		"GIT_CONFIG_KEY_0":           "credential.helper",
		"GIT_CONFIG_VALUE_0":         "",
		"GIT_CONFIG_KEY_1":           "credential.helper",
		"GIT_CONFIG_VALUE_1":         "!'" + h.cfg.AthenaBin + "' git-credential",
		"GIT_CONFIG_KEY_2":           "credential.useHttpPath",
		"GIT_CONFIG_VALUE_2":         "true",
		"GIT_CONFIG_KEY_3":           "url." + githubTarget("o/r") + ".insteadOf",
		"GIT_CONFIG_VALUE_3":         "https://github.com/o/r.git",
		"GIT_CONFIG_KEY_4":           "protocol.ssh.allow",
		"GIT_CONFIG_VALUE_4":         "never",
		"ATHENA_DIR":                 dir,
	} {
		if got, ok := env[k]; !ok || got != want {
			t.Errorf("env %s = %q, want %q", k, got, want)
		}
	}
	if p := env["PATH"]; !strings.HasPrefix(p, filepath.Join(h.cfg.StateDir, ShimsDir)+string(os.PathListSeparator)) {
		t.Errorf("PATH = %q, want the shims first", p)
	}
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "ATHENA_NODE_GIT_TOKEN"} {
		if _, ok := env[k]; ok {
			t.Errorf("env has %s", k)
		}
	}
	// The worktree is on the Ticket's branch, from the clone, and its remote holds no credential.
	if got := git(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "athena/t-0001" {
		t.Errorf("worktree branch = %q", got)
	}
	if got := git(t, dir, "remote", "get-url", "--push", "origin"); got != "https://github.com/o/r.git" {
		t.Errorf("worktree push URL = %q", got)
	}
	if got := git(t, dir, "config", "--get", "remote.origin.url"); got != MirrorDir(h.cfg.StateDir, "o/r") {
		t.Errorf("worktree fetch URL = %q, want the mirror", got)
	}
	if st := git(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("worktree not clean: %q", st)
	}
	// The first message names the GitHub variables; nothing holds the token.
	if !strings.Contains(h.stdin(), "ATHENA_GITHUB_REPOSITORY=o/r") || !strings.Contains(h.stdin(), "trigger_id: github:issuecomment:1") {
		t.Errorf("first message lacks the GitHub variables or the trigger card:\n%s", h.stdin())
	}
	h.eventually("transcript written", func() bool {
		b, err := os.ReadFile(TranscriptPath(h.cfg.StateDir, tk.ID))
		return err == nil && len(b) > 0
	})
	r := h.hook(EventSessionStart, tk.ID, a.ID, map[string]any{"hook_event_name": "SessionStart"})
	if !strings.Contains(r.AdditionalContext, "ATHENA_GITHUB_ACCOUNT="+botLogin) {
		t.Errorf("session context lacks the GitHub variables:\n%s", r.AdditionalContext)
	}
	if rep := h.report(tk.ID, a.ID, core.ReportProgress, "working"); !rep.OK {
		t.Fatal(rep)
	}
	transcript, _ := os.ReadFile(TranscriptPath(h.cfg.StateDir, tk.ID))
	for what, text := range map[string]string{"environment": string(b), "first message": h.stdin(),
		"transcript": string(transcript), "logs": h.logs.String(), "session context": r.AdditionalContext} {
		if strings.Contains(text, fakeToken) {
			t.Errorf("the %s holds the token", what)
		}
	}
	if !strings.Contains(h.logs.String(), "credential served") {
		t.Errorf("logs lack the credential line:\n%s", h.logs.String())
	}
}

func TestTokenDroppedOnStop(t *testing.T) {
	h := newGHHarness(t, &fakeCreds{}, nil)
	tk, a := h.accept("github:issuecomment:1", "o/r", 12, "")
	if !h.n.tokens.holds(a.ID) {
		t.Fatal("no token held for the running assignment")
	}
	if err := h.eng.StopTicket(tk.ID, "enough"); err != nil {
		t.Fatal(err)
	}
	h.eventually("session gone", func() bool { return h.n.liveCount() == 0 })
	h.eventually("token dropped", func() bool { return !h.n.tokens.holds(a.ID) })
	if got := h.gitCredential(tk.ID, a.ID, "protocol=https\nhost=github.com\npath=o/r.git\n"); got != "" {
		t.Fatalf("a stopped session got %q", got)
	}
	var resp CredentialResponse
	if err := callNode(h.cfg.SocketPath, CredentialRequest{Op: OpCredential, Ticket: tk.ID, Assignment: a.ID, Purpose: CredentialGh}, &resp); err != nil || resp.OK || resp.Token != "" {
		t.Fatalf("gh token for a stopped session: %+v %v", resp, err)
	}
	if h.n.tokens.holds(a.ID) {
		t.Fatal("a refused request fetched a token again")
	}
	// Minor 3: the dropped token is revoked, not left valid until it expires.
	h.eventually("the dropped token revoked", func() bool { return len(h.creds.revokedTokens()) > 0 })
	if got := h.creds.revokedTokens(); len(got) != 1 || !strings.HasPrefix(got[0], fakeToken) {
		t.Fatalf("revoked %q, want the dropped token", got)
	}
}

// Revoking is best effort: a failure is logged, and the token is dropped all the same. A token minted for an
// Assignment dropped meanwhile is revoked too.
func TestDropRevokesBestEffort(t *testing.T) {
	creds := &fakeCreds{revokeErr: errors.New("502 bad gateway")}
	ts := newTokenStore(creds)
	tok, err := ts.token(context.Background(), "a1", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	ts.drop("a1")
	ts.wait()
	if ts.holds("a1") || !slices.Equal(creds.revokedTokens(), []string{tok}) {
		t.Fatalf("held %v, revoked %q; want %q dropped and revoked", ts.holds("a1"), creds.revokedTokens(), tok)
	}
	ts.drop("a2") // nothing held: nothing to revoke
	ts.wait()
	if n := len(creds.revokedTokens()); n != 1 {
		t.Fatalf("revoked %d tokens, want 1", n)
	}
}

func TestTokenRefreshedBeforeExpiry(t *testing.T) {
	creds := &fakeCreds{life: 10 * time.Minute}
	ts := newTokenStore(creds)
	clock := time.Now()
	ts.now = func() time.Time { return clock }
	ctx := context.Background()
	first, err := ts.token(ctx, "a1", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := ts.token(ctx, "a1", "o/r"); again != first || creds.calls.Load() != 1 {
		t.Fatalf("a fresh token was fetched again (%d calls)", creds.calls.Load())
	}
	clock = clock.Add(6 * time.Minute) // 4 minutes left: inside the refresh margin
	if next, _ := ts.token(ctx, "a1", "o/r"); next == first || creds.calls.Load() != 2 {
		t.Fatalf("a token 4 minutes from expiry was not refreshed (%d calls)", creds.calls.Load())
	}
	if _, err := ts.token(ctx, "a2", "o/s"); err != nil || creds.calls.Load() != 3 {
		t.Fatalf("another assignment shares a token: %v (%d calls)", err, creds.calls.Load())
	}
	ts.drop("a1")
	if ts.holds("a1") || !ts.holds("a2") {
		t.Fatal("drop dropped the wrong token")
	}
	if got := fmt.Sprintf("%v %s %+v %#v", secret("ghs_x"), secret("ghs_x"), struct{ T secret }{"ghs_x"}, secret("ghs_x")); strings.Contains(got, "ghs_x") {
		t.Fatal("a secret prints its value")
	}
	if b, _ := json.Marshal(struct{ T secret }{"ghs_x"}); strings.Contains(string(b), "ghs_x") {
		t.Fatal("a secret marshals its value")
	}
	if _, err := newTokenStore(nil).token(ctx, "a3", "o/r"); err == nil {
		t.Fatal("a token without credentials")
	}
}

// A Ticket that works in a GitHub repository fails at launch on a node without Credentials.
func TestGitHubTicketWithoutCredentialsFails(t *testing.T) {
	h := newGHHarness(t, nil, nil)
	tr := core.Trigger{ID: "github:issuecomment:1", Source: "github", Kind: core.KindMessage,
		Actor: core.ActorRef{Source: "github", ID: "alice"}, Human: true, Addressed: core.AddressedMention, Scope: "comments",
		Threads: []string{"github:o/r#12"}, At: time.Now(), Text: "@athena fix it", Meta: map[string]string{"repo": "o/r", "number": "12"}}
	if _, err := h.eng.Receive(tr); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.EvaluateDue(); err != nil {
		t.Fatal(err)
	}
	r, err := h.eng.Decide(core.DecisionInput{TriggerID: tr.ID, Kind: core.DecisionAccept, Title: "Fix it"})
	if err != nil || r.Ticket == nil {
		t.Fatalf("accept: %+v %v", r, err)
	}
	h.eventually("failed report", func() bool {
		tk, _ := h.ticket(r.Ticket.ID)
		rep := tk.LatestAssignment().Report
		return rep != nil && rep.State == core.ReportFailed && strings.Contains(rep.Summary, "no GitHub credentials")
	})
	if n := len(h.invocations()); n != 0 {
		t.Fatalf("claude started %d times", n)
	}
}

func TestReportPullBindsThread(t *testing.T) {
	h := newGHHarness(t, &fakeCreds{}, nil)
	tk, a := h.accept("github:issuecomment:1", "O/R", 12, "")
	cs := mcpClient(t, tk.ID, a.ID, h.cfg.SocketPath)
	ctx := context.Background()
	for _, bad := range []map[string]any{{"status": "progress", "summary": "x", "pull": 0}, {"status": "progress", "summary": "x", "pull": "7"}} {
		if res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "report", Arguments: bad}); err == nil && !res.IsError {
			t.Errorf("report %v accepted", bad)
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "report", Arguments: map[string]any{"status": "ready", "summary": "PR #7 is ready for review", "pull": 7}})
	if err != nil || res.IsError {
		t.Fatalf("ready report with pull: %v %s", err, resultText(res))
	}
	tk, st := h.ticket(tk.ID)
	if tk.Ended != nil {
		t.Fatalf("a ready report with an open pull request ended the ticket: %+v", tk.Ended)
	}
	if r := tk.LatestAssignment().Report; r == nil || r.Pull != 7 {
		t.Fatalf("report = %+v, want pull 7", r)
	}
	if len(tk.Follows) != 1 || tk.Follows[0] != (core.Follow{Thread: "github:o/r#7", Own: true}) {
		t.Fatalf("follows = %+v, want github:o/r#7 own", tk.Follows)
	}
	th, err := h.eng.Thread("github:o/r#7")
	if err != nil || len(th.Subscriptions) != 1 || !th.Subscriptions[0].Own || th.Subscriptions[0].TicketID != tk.ID {
		t.Fatalf("thread = %+v %v", th, err)
	}
	if st.Stage == "ended" {
		t.Fatalf("stage = %+v", st)
	}
	// The pull request's merge ends the Ticket.
	if err := h.eng.ThreadClosed("github:o/r#7", core.OutcomeMerged, time.Now()); err != nil {
		t.Fatal(err)
	}
	if tk, _ := h.ticket(tk.ID); tk.Ended == nil || tk.Ended.Outcome != core.OutcomeMerged {
		t.Fatalf("ticket after the merge: %+v", tk.Ended)
	}

	// An owner task has no repository to bind a pull request in.
	plain := h.submit("Local task", "")
	_, pa := h.running(plain.ID)
	var resp Response
	if err := callNode(h.cfg.SocketPath, ReportRequest{Op: OpReport, Ticket: plain.ID, Assignment: pa.ID, Status: "ready", Summary: "x", Pull: 3}, &resp); err != nil || resp.OK {
		t.Fatalf("pull on an owner task: %+v %v", resp, err)
	}
	if tk, _ := h.ticket(plain.ID); tk.Ended != nil {
		t.Fatal("the refused report ended the owner task")
	}
}

func TestWorkerToolsReachEngine(t *testing.T) {
	h := newGHHarness(t, &fakeCreds{}, nil)
	tk, a := h.accept("github:issuecomment:1", "o/r", 12, "")
	cs := mcpClient(t, tk.ID, a.ID, h.cfg.SocketPath)
	ctx := context.Background()
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return resultText(res), res.IsError
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	want := WorkerTools()
	slices.Sort(names)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}

	if out, isErr := call("ack", map[string]any{"trigger_id": "github:issuecomment:1", "text": "On it: fixing the README typo."}); isErr || !strings.Contains(out, `"queued":true`) {
		t.Fatalf("ack: %s", out)
	}
	// A repeat ack, in other words, succeeds and posts nothing.
	if out, isErr := call("ack", map[string]any{"trigger_id": "github:issuecomment:1", "text": "Working on it."}); isErr ||
		!strings.Contains(out, `"queued":false`) || !strings.Contains(out, "Already acknowledged") {
		t.Fatalf("repeat ack: %s", out)
	}
	if out, isErr := call("ack", map[string]any{"trigger_id": "github:issuecomment:404", "text": "On it."}); !isErr ||
		!strings.Contains(out, "ack refused: trigger github:issuecomment:404 not found") || !strings.Contains(out, "trigger_ids: github:issuecomment:1 (acknowledged already)") {
		t.Fatalf("ack of a stranger trigger: %s", out)
	}
	// The input id given for the trigger_id: the refusal names the trigger_id to use.
	tkNow, _ := h.ticket(tk.ID)
	if out, isErr := call("ack", map[string]any{"trigger_id": tkNow.Inputs[0].ID, "text": "On it."}); !isErr ||
		!strings.Contains(out, "is an input id, not a trigger_id: that input's trigger_id is github:issuecomment:1") {
		t.Fatalf("ack by input id: %s", out)
	}
	if out, isErr := call("ack", map[string]any{"text": "On it."}); !isErr || !strings.Contains(out, "name the trigger_id line") {
		t.Fatalf("ack without a trigger_id: %s", out)
	}
	if out, isErr := call("reply", map[string]any{"purpose": "answer", "text": "It is the README.", "trigger_id": "github:issuecomment:1"}); isErr {
		t.Fatalf("reply answer: %s", out)
	}
	if out, isErr := call("reply", map[string]any{"text": "It is the README file.", "trigger_id": "github:issuecomment:1"}); isErr {
		t.Fatalf("reply without a purpose: %s", out)
	}
	// A worker sends no result or question by reply: each refusal says which report to file instead.
	if out, isErr := call("reply", map[string]any{"purpose": "result", "text": "Done."}); !isErr ||
		!strings.Contains(out, "file report {status: ready, summary, pull}") {
		t.Fatalf("a result reply: %s", out)
	}
	if out, isErr := call("reply", map[string]any{"purpose": "attention", "text": "Which page?"}); !isErr ||
		!strings.Contains(out, "file report {status: waiting (or blocked), summary, question}") {
		t.Fatalf("an attention reply: %s", out)
	}
	if out, isErr := call("reply", map[string]any{"text": "Yes."}); !isErr || !strings.Contains(out, "trigger_id is required") {
		t.Fatalf("an answer without a trigger_id: %s", out)
	}
	// A ready report with a question is refused with what to do instead; the report tool says what Athena does next.
	if out, isErr := call("report", map[string]any{"status": "ready", "summary": "x", "question": "More?"}); !isErr ||
		!strings.Contains(out, "report waiting with question instead") {
		t.Fatalf("a ready report with a question: %s", out)
	}
	if out, isErr := call("report", map[string]any{"status": "progress", "summary": "halfway"}); isErr || out != "Report filed (progress)." {
		t.Fatalf("progress report: %s", out)
	}

	if out, isErr := call("follow_thread", map[string]any{"thread": "https://github.com/O/R/pull/31"}); isErr || !strings.Contains(out, "github:o/r#31") {
		t.Fatalf("follow_thread: %s", out)
	}
	th, err := h.eng.Thread("github:o/r#31")
	if err != nil || len(th.Subscriptions) != 1 || !th.Subscriptions[0].Context {
		t.Fatalf("followed thread = %+v %v", th, err)
	}
	if out, isErr := call("mark_unrelated", map[string]any{"thread": "#31", "reason": "it is about the CI cache"}); isErr {
		t.Fatalf("mark_unrelated: %s", out)
	}
	if th, _ := h.eng.Thread("github:o/r#31"); len(th.Subscriptions) != 0 {
		t.Fatalf("unrelated thread still subscribed: %+v", th.Subscriptions)
	}
	if out, isErr := call("mark_unrelated", map[string]any{"thread": "o/r#12", "reason": "x"}); !isErr || !strings.Contains(out, "origin") {
		t.Fatalf("mark_unrelated of the origin thread: %s", out)
	}
	if out, isErr := call("handback", map[string]any{"trigger_id": "github:issuecomment:1", "reason": "belongs to another ticket"}); isErr || !strings.Contains(out, `"queued"`) {
		t.Fatalf("handback: %s", out)
	}
	if out, isErr := call("handback", map[string]any{"trigger_id": "github:issuecomment:1", "reason": "again"}); !isErr {
		t.Fatalf("a second handback: %s", out)
	}
	if out, isErr := call("follow_thread", map[string]any{"thread": "not a thread"}); !isErr {
		t.Fatalf("follow_thread of garbage: %s", out)
	}

	stranger := mcpClient(t, tk.ID, "not-the-assignment", h.cfg.SocketPath)
	res, err := stranger.CallTool(ctx, &mcp.CallToolParams{Name: "ack", Arguments: map[string]any{"trigger_id": "github:issuecomment:1", "text": "x"}})
	if err != nil || !res.IsError {
		t.Fatalf("a stranger's ack: %v %+v", err, res)
	}
}

func TestThreadArg(t *testing.T) {
	for _, c := range []struct{ in, repo, want string }{
		{"github:O/R#12", "", "github:o/r#12"},
		{"o/r#12", "", "github:o/r#12"},
		{"#12", "Own/Repo", "github:own/repo#12"},
		{"https://github.com/O/R/issues/5#issuecomment-9", "", "github:o/r#5"},
		{"https://github.com/o/r/pull/6/files", "", "github:o/r#6"},
		{"github:o/r@main", "", "github:o/r@main"},
		{"feishu:chat-1", "", "feishu:chat-1"},
	} {
		if got, err := ThreadArg(c.in, c.repo); err != nil || got != c.want {
			t.Errorf("ThreadArg(%q, %q) = %q, %v; want %q", c.in, c.repo, got, err, c.want)
		}
	}
	for _, in := range []string{"", "#12", "o/r#x", "o/r#0", "https://gitlab.com/o/r/issues/1", "https://github.com/o/r", "x:", "../r#1"} {
		if got, err := ThreadArg(in, ""); err == nil {
			t.Errorf("ThreadArg(%q) = %q, want an error", in, got)
		}
	}
}

// A credential request made after the runner dropped its token, while the session is still registered, mints nothing.
func TestTokenNotMintedAfterDrop(t *testing.T) {
	type result struct {
		resp CredentialResponse
		held bool
	}
	got := make(chan result, 1)
	var h *ghHarness
	h = newGHHarness(t, &fakeCreds{}, nil)
	h.n.afterDrop = func(s *session) {
		var r CredentialResponse
		_ = callNode(h.cfg.SocketPath, CredentialRequest{Op: OpCredential, Ticket: s.t.ID, Assignment: s.a.ID, Purpose: CredentialGh}, &r)
		got <- result{r, h.n.tokens.holds(s.a.ID)}
	}
	tk, _ := h.accept("github:issuecomment:1", "o/r", 12, "")
	calls := h.creds.calls.Load()
	if err := h.eng.StopTicket(tk.ID, "enough"); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		if r.resp.OK || r.resp.Token != "" || r.held {
			t.Fatalf("a request after the drop got %+v (held %v)", r.resp, r.held)
		}
	case <-time.After(waitTimeout):
		t.Fatal("the drop hook never ran")
	}
	if n := h.creds.calls.Load(); n != calls {
		t.Fatalf("a token was minted after the drop (%d calls, want %d)", n, calls)
	}
}

func TestTokenStoreTombstone(t *testing.T) {
	creds := &fakeCreds{}
	ts := newTokenStore(creds)
	ctx := context.Background()
	if _, err := ts.token(ctx, "a1", "o/r"); err != nil {
		t.Fatal(err)
	}
	ts.drop("a1")
	if _, err := ts.token(ctx, "a1", "o/r"); err == nil || creds.calls.Load() != 1 {
		t.Fatalf("token after drop: %v (%d calls)", err, creds.calls.Load())
	}
	ts.revive("a1") // a new session of the same Assignment
	if _, err := ts.token(ctx, "a1", "o/r"); err != nil {
		t.Fatalf("token after revive: %v", err)
	}
}

// R24: the node's own git gets no token in its environment or arguments; its helper asks the node with a nonce.
func TestNodeGitHasNoTokenInEnvOrArgs(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "git.log")
	wrapper := "#!/bin/sh\n{ printf 'ARGS:'; printf ' %s' \"$@\"; echo; env; } >> " + logPath + "\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrapDir, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := newGHHarness(t, &fakeCreds{}, nil)
	h.accept("github:issuecomment:1", "o/r", 12, "")
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	if !strings.Contains(log, "ARGS: ") || !strings.Contains(log, "clone") {
		t.Fatalf("the wrapper saw no node git:\n%.2000s", log)
	}
	if strings.Contains(log, fakeToken) {
		t.Fatal("a node git process had the token in its environment or arguments")
	}
	if !strings.Contains(log, "git-credential --node ") {
		t.Errorf("the node's git has no athena helper:\n%.2000s", log)
	}
}

// The node's helper answers only github.com, for its own call's repository, by a live nonce; git consults it only for
// https://github.com.
func TestCloneHelperRefusesOtherHost(t *testing.T) {
	h := newGHHarness(t, &fakeCreds{}, nil)
	nonce, revoke := h.n.grantNode("o/r", fakeToken)
	defer revoke()
	helper := func(nonce, req string) string {
		var out bytes.Buffer
		env := []string{"ATHENA_SOCKET=" + h.cfg.SocketPath}
		RunGitCredential([]string{"--node", nonce, "get"}, strings.NewReader(req), &out, io.Discard, env)
		return out.String()
	}
	own := "protocol=https\nhost=github.com\npath=o/r.git\n"
	for _, req := range []string{
		"protocol=https\nhost=evil.example\npath=o/r.git\n",
		"protocol=https\nhost=github.com\npath=o/s.git\n",
		"protocol=https\nhost=github.com\n",
		"protocol=http\nhost=github.com\npath=o/r.git\n",
	} {
		if got := helper(nonce, req); got != "" {
			t.Errorf("request %q answered %q", req, got)
		}
	}
	if got := helper("not-a-nonce", own); got != "" {
		t.Errorf("a made-up nonce got %q", got)
	}
	// A refused request does not use the nonce up; the first answer does (R27).
	if got := helper(nonce, own); got != "username=x-access-token\npassword="+fakeToken+"\n" {
		t.Fatalf("own repository: %q", got)
	}
	if got := helper(nonce, own); got != "" {
		t.Errorf("a second use of the nonce got %q", got)
	}
	again, revokeAgain := h.n.grantNode("o/r", fakeToken)
	revokeAgain()
	if got := helper(again, own); got != "" {
		t.Errorf("a revoked nonce got %q", got)
	}
	// git asks the node's helper only for https://github.com: a helper that would answer anything is never asked
	// for another host.
	leaky := filepath.Join(t.TempDir(), "leaky")
	if err := os.WriteFile(leaky, []byte("#!/bin/sh\nprintf 'username=x\\npassword=LEAKED\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	clone := t.TempDir()
	git(t, clone, "init", "-q")
	fill := func(req string) string {
		cmd := exec.Command("git", "-C", clone, "credential", "fill")
		cmd.Env = mirrorGitEnv(clone, "o/r", &nodeAuth{bin: leaky, socket: h.cfg.SocketPath, nonce: "n"})
		cmd.Stdin = strings.NewReader(req)
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	if out := fill("protocol=https\nhost=evil.example\npath=o/r.git\n\n"); strings.Contains(out, "LEAKED") {
		t.Errorf("the node's helper was asked for another host:\n%s", out)
	}
	if out := fill("protocol=https\nhost=github.com\npath=o/r.git\n\n"); !strings.Contains(out, "password=LEAKED") {
		t.Errorf("the node's helper is not asked for github.com, so the test proves nothing:\n%s", out)
	}
}

// acceptOnly receives and accepts alice's request on repo#n without waiting for the Assignment to run.
func (h *ghHarness) acceptOnly(id, repo string, n int) string {
	h.t.Helper()
	thread := fmt.Sprintf("github:%s#%d", repo, n)
	tr := core.Trigger{ID: id, Source: "github", Kind: core.KindMessage, Actor: core.ActorRef{Source: "github", ID: "alice"},
		Human: true, Addressed: core.AddressedMention, Scope: "comments", Threads: []string{thread}, At: time.Now(),
		Text: "@athena fix it", Meta: map[string]string{"repo": repo, "number": fmt.Sprint(n), "kind": "issue"}}
	if _, err := h.eng.Receive(tr); err != nil {
		h.t.Fatal(err)
	}
	if err := h.eng.EvaluateDue(); err != nil {
		h.t.Fatal(err)
	}
	r, err := h.eng.Decide(core.DecisionInput{TriggerID: id, Kind: core.DecisionAccept, Title: "Fix " + id})
	if err != nil || r.Ticket == nil {
		h.t.Fatalf("accept: %+v %v", r, err)
	}
	return r.Ticket.ID
}

// Fix round 1 (Task 12): GitHub credentials that cannot be had yet (a network failure minting the token, the bot not
// known yet) delay the launch and are retried; they never file a failed report. A permanent refusal does fail it.
func TestGitHubLaunchRetriesTransientCredentials(t *testing.T) {
	creds := &fakeCreds{failErr: fmt.Errorf("mint: %w", github.ErrTransient)}
	creds.fail.Store(2)
	creds.noIdentity.Store(true)
	h := newGHHarness(t, creds, func(c *Config) { c.CredentialRetry = 20 * time.Millisecond })
	id := h.acceptOnly("github:issuecomment:1", "o/r", 12)
	time.Sleep(150 * time.Millisecond)
	if tk, _ := h.ticket(id); tk.LatestAssignment().Report != nil || len(h.invocations()) != 0 {
		t.Fatalf("launch without an identity: report %+v, %d starts; want it waiting", tk.LatestAssignment().Report, len(h.invocations()))
	}
	creds.noIdentity.Store(false)
	h.running(id)
	if tk, _ := h.ticket(id); tk.LatestAssignment().Report != nil {
		t.Fatalf("report = %+v, want none", tk.LatestAssignment().Report)
	}
	if n := creds.calls.Load(); n != 3 {
		t.Fatalf("token asked %d times, want 3 (two transient failures, then the token)", n)
	}
	if !strings.Contains(h.logs.String(), "retry") {
		t.Errorf("no retry logged:\n%s", h.logs.String())
	}
}

func TestGitHubLaunchFailsOnPermanentCredentialError(t *testing.T) {
	creds := &fakeCreds{failErr: fmt.Errorf("owner is not the installation's: %w", github.ErrPermanent)}
	creds.fail.Store(1)
	h := newGHHarness(t, creds, func(c *Config) { c.CredentialRetry = 20 * time.Millisecond })
	id := h.acceptOnly("github:issuecomment:1", "o/r", 12)
	h.eventually("failed report", func() bool {
		tk, _ := h.ticket(id)
		rep := tk.LatestAssignment().Report
		return rep != nil && rep.State == core.ReportFailed && strings.Contains(rep.Summary, "not the installation")
	})
	if n := creds.calls.Load(); n != 1 {
		t.Fatalf("token asked %d times, want 1", n)
	}
}

// Minor 5: a GitHub launch that waits for its credentials holds no slot: another Ticket starts meanwhile.
func TestLaunchWaitingForCredentialsHoldsNoSlot(t *testing.T) {
	creds := &fakeCreds{}
	creds.noIdentity.Store(true)
	h := newGHHarness(t, creds, func(c *Config) { c.Slots = 1; c.CredentialRetry = 20 * time.Millisecond })
	id := h.acceptOnly("github:issuecomment:1", "o/r", 12)
	time.Sleep(100 * time.Millisecond)
	owner := h.submit("Owner work", "")
	h.running(owner.ID)
	if tk, _ := h.ticket(id); tk.LatestAssignment().State != core.AssignmentPending || tk.LatestAssignment().Report != nil {
		t.Fatalf("waiting launch = %+v, want it pending with no report", tk.LatestAssignment())
	}
	if err := h.eng.StopTicket(owner.ID, "enough"); err != nil {
		t.Fatal(err)
	}
	creds.noIdentity.Store(false)
	h.running(id)
}

// The report tool's result says what Athena does with a final report: it posts the result itself.
func TestReportFiledSaysWhatFollows(t *testing.T) {
	if got := reportFiled("ready"); !strings.Contains(got, "Athena posts the result to people from this summary") ||
		!strings.Contains(got, "do not send it again with reply") {
		t.Errorf("ready: %q", got)
	}
	if got := reportFiled("dropped"); !strings.Contains(got, "send nothing more") {
		t.Errorf("dropped: %q", got)
	}
	if got := reportFiled("waiting"); got != "Report filed (waiting)." {
		t.Errorf("waiting: %q", got)
	}
}
