package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/server"
)

func keyFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGithubSetupAndCheckCommands(t *testing.T) {
	ctx := context.Background()
	dir := newStateDir(t)
	if code, _, errOut := athena(ctx, "github", "check", "--state-dir", dir); code != 1 || !strings.Contains(errOut, "github is not configured") {
		t.Fatalf("check without config: %d %q", code, errOut)
	}
	wide := keyFile(t, 0o640)
	code, _, errOut := athena(ctx, "github", "setup", "--state-dir", dir, "--app-id", "12", "--slug", "athena-bot", "--key", wide,
		"--installation", "777", "--repo", "o/r")
	if code != 1 || !strings.Contains(errOut, "mode 0640") || !strings.Contains(errOut, "chmod 0600") {
		t.Fatalf("setup with a wide key: %d %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, server.ConfigFile)); !os.IsNotExist(err) {
		t.Fatalf("a refused setup wrote the config: %v", err)
	}
	key := keyFile(t, 0o600)
	code, out, errOut := athena(ctx, "github", "setup", "--state-dir", dir, "--app-id", "12", "--slug", "athena-bot", "--key", key,
		"--installation", "777", "--repo", "o/r", "--repo", "o/s,o/t", "--receive-all", "o/s")
	if code != 0 || !strings.Contains(out, "repositories o/r, o/s, o/t") {
		t.Fatalf("setup: %d %q %q", code, out, errOut)
	}
	cfg, err := server.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if g := cfg.GitHub; g == nil || g.AppID != 12 || g.Slug != "athena-bot" || g.KeyFile != key || g.InstallationID != 777 ||
		strings.Join(g.Repos, ",") != "o/r,o/s,o/t" || strings.Join(g.ReceiveAll, ",") != "o/s" || g.IntervalSeconds != 30 ||
		cfg.Router.Model != "haiku" || cfg.EvalTimeoutSeconds != 600 {
		t.Fatalf("config = %+v %+v", cfg, cfg.GitHub)
	}
	raw, err := os.ReadFile(filepath.Join(dir, server.ConfigFile))
	if err != nil || !strings.Contains(string(raw), `"intervalSeconds": 30`) || !strings.Contains(string(raw), `"evalTimeoutSeconds": 600`) ||
		strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatalf("config file:\n%s", raw)
	}
	st, err := os.Stat(filepath.Join(dir, server.ConfigFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode: %v %v", st, err)
	}
	if code, _, errOut := athena(ctx, "github", "setup", "--state-dir", dir, "--app-id", "12", "--slug", "x", "--key", key,
		"--installation", "777", "--repo", "not-a-repo"); code != 1 || !strings.Contains(errOut, "not owner/name") {
		t.Fatalf("setup with a bad repo: %d %q", code, errOut)
	}
	if code, _, errOut := athena(ctx, "github", "setup", "--state-dir", dir, "--app-id", "12", "--slug", "x", "--key", key,
		"--installation", "777", "--repo", "o/r", "--receive-all", "o/s"); code != 1 || !strings.Contains(errOut, "not one of repos") {
		t.Fatalf("setup receiving all of an unconfigured repo: %d %q", code, errOut)
	}
}

func TestRoutingCommandsAgainstServer(t *testing.T) {
	dir := startServer(t)
	ctx := context.Background()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"triggers"}, "no triggers\n"},
		{[]string{"triggers", "--thread", "github:o/r#1"}, "no triggers\n"},
		{[]string{"threads"}, "no threads\n"},
		{[]string{"decisions"}, "no decisions\n"},
	} {
		if code, out, errOut := athena(ctx, append(c.args, "--state-dir", dir)...); code != 0 || out != c.want {
			t.Errorf("%v: %d %q %q, want %q", c.args, code, out, errOut, c.want)
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"reevaluate", "github:issuecomment:1"}, "trigger github:issuecomment:1 not found\n"},
		{[]string{"dismiss", "d-1"}, "decision d-1 is not an unavailable decision\n"},
		{[]string{"unsubscribe", "github:O/R#1"}, "thread github:o/r#1 not found\n"},
		{[]string{"task", "Bump deps", "--repo", "o/r"}, "github is not running: a task on a repository needs it (see athena status)\n"},
	} {
		if code, out, errOut := athena(ctx, append(c.args, "--state-dir", dir)...); code != 1 || errOut != c.want || out != "" {
			t.Errorf("%v: %d %q %q, want exit 1 with %q", c.args, code, out, errOut, c.want)
		}
	}

	// The kill switch: pause says what stops, status shows it, resume clears it.
	if code, out, errOut := athena(ctx, "pause", "--state-dir", dir); code != 0 || errOut != "" ||
		out != "Athena is paused. Running sessions stop at the end of their turn; nothing queued is lost. Resume with: athena resume\n" {
		t.Fatalf("pause: %d %q %q", code, out, errOut)
	}
	if code, out, _ := athena(ctx, "status", "--state-dir", dir); code != 0 || !strings.Contains(out, "paused since ") || !strings.Contains(out, "(by cli)") {
		t.Fatalf("status while paused: %d %q", code, out)
	}
	if code, out, errOut := athena(ctx, "resume", "--state-dir", dir); code != 0 || errOut != "" || out != "Athena resumed.\n" {
		t.Fatalf("resume: %d %q %q", code, out, errOut)
	}
	if code, out, _ := athena(ctx, "status", "--state-dir", dir); code != 0 || strings.Contains(out, "paused") {
		t.Fatalf("status after resume: %d %q", code, out)
	}

	code, out, errOut := athena(ctx, "policy", "show", "--state-dir", dir)
	var shown server.PolicyView
	if code != 0 || json.Unmarshal([]byte(out), &shown) != nil || shown.Revision != 0 || shown.Settings.Mode != "observe" {
		t.Fatalf("policy show: %d %q %q", code, out, errOut)
	}
	file := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(file, []byte(`{"mode":"auto","rules":[],"sources":{"github":{"allowed":["alice"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = athena(ctx, "policy", "set", file, "--state-dir", dir)
	if code != 0 || !strings.HasPrefix(out, "policy revision 1 (version ") || !strings.HasSuffix(out, "saved; no open ticket is affected\n") {
		t.Fatalf("policy set: %d %q %q", code, out, errOut)
	}
	code, out, _ = athena(ctx, "policy", "show", "--state-dir", dir)
	if code != 0 || json.Unmarshal([]byte(out), &shown) != nil || shown.Revision != 1 || shown.Settings.Mode != "auto" {
		t.Fatalf("policy show after set: %d %q", code, out)
	}
	if err := os.WriteFile(file, []byte(`{"mode":"never"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := athena(ctx, "policy", "set", file, "--state-dir", dir); code != 1 || !strings.Contains(errOut, `mode "never"`) {
		t.Fatalf("policy set invalid: %d %q", code, errOut)
	}
	if err := os.WriteFile(file, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := athena(ctx, "policy", "set", file, "--state-dir", dir); code != 1 || !strings.Contains(errOut, "is not JSON") {
		t.Fatalf("policy set not json: %d %q", code, errOut)
	}
}

// withUTC renders local times as UTC for a golden test.
func withUTC(t *testing.T) {
	prev := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = prev })
}

func TestRoutingViewsGolden(t *testing.T) {
	withUTC(t)
	at := time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC)
	delivered := at.Add(time.Minute)
	rate := 4321
	for _, c := range []struct {
		name  string
		print func(*bytes.Buffer)
		want  string
	}{
		{"triggers", func(b *bytes.Buffer) {
			printTriggers(b, []server.TriggerView{
				{ID: "github:issuecomment:11", At: at, Actor: "alice", Addressed: "mention", Thread: "github:o/r#3", Outcome: "reached:T-0001", Text: "@athena-bot fix it"},
				{ID: "github:checksuite:5", At: at, Thread: "github:o/r@main", Outcome: "filtered:not_listening", Text: "build failed"},
			})
		}, "" +
			"AT                   ID                      ACTOR            THREAD           OUTCOME                 TEXT\n" +
			"2026-11-03 09:30:00  github:issuecomment:11  alice (mention)  github:o/r#3     reached:T-0001          @athena-bot fix it\n" +
			"2026-11-03 09:30:00  github:checksuite:5     -                github:o/r@main  filtered:not_listening  build failed\n"},
		{"threads", func(b *bytes.Buffer) {
			printThreads(b, []server.ThreadView{
				{Key: "github:o/r#3", Queued: 1, Eval: &server.EvalView{ID: "ev1", Strength: "explicit", DeliveredAt: &delivered}, Decisions: 2},
				{Key: "github:o/r#7", Tickets: []string{"T-0001(own,bound)"}, Closed: "merged"},
			})
		}, "" +
			"THREAD        QUEUED  EVAL                    TICKETS            DECISIONS  CLOSED\n" +
			"github:o/r#3  1       ev1 explicit delivered  -                  2          -\n" +
			"github:o/r#7  0       -                       T-0001(own,bound)  0          merged\n"},
		{"decisions", func(b *bytes.Buffer) {
			printDecisions(b, []server.DecisionView{
				{ID: "d2", At: at, Kind: "ignore", Thread: "github:o/r#4", Triggers: []string{"a"}, Reasons: []string{"not_a_member"}, WouldAccept: true},
				{ID: "d1", At: at, Kind: "accept", Thread: "github:o/r#3", TicketID: "T-0001", Triggers: []string{"a", "b"}, Reasons: []string{"said: clear request"}},
			})
		}, "" +
			"AT                   ID  KIND                   THREAD        TICKET  TRIGGERS  REASONS\n" +
			"2026-11-03 09:30:00  d2  ignore (would accept)  github:o/r#4  -       1         not_a_member\n" +
			"2026-11-03 09:30:00  d1  accept                 github:o/r#3  T-0001  2         said: clear request\n"},
		{"health", func(b *bytes.Buffer) {
			printHealth(b, server.HealthView{Sources: []server.SourceHealth{{Name: "github"}}, GitHubRateRemaining: &rate,
				Effects: map[string]string{"reply": "reply r1 failed 3 times: boom"}, Router: server.RouterHealth{State: "running", Session: "27abadfa"}})
		}, "" +
			"source:    github ok (rate remaining 4321)\n" +
			"effects:   reply: reply r1 failed 3 times: boom\n" +
			"router:    running (session 27abadfa)\n"},
		{"health down", func(b *bytes.Buffer) {
			printHealth(b, server.HealthView{Sources: []server.SourceHealth{{Name: "github", Error: "github: 3 polls in a row failed"}},
				Router: server.RouterHealth{State: "crashed", Error: "router: session ended: exited"}})
		}, "" +
			"source:    github error: github: 3 polls in a row failed\n" +
			"effects:   ok\n" +
			"router:    crashed: router: session ended: exited\n"},
		{"github check", func(b *bytes.Buffer) {
			printGitHubCheck(b, server.GitHubCheck{Bot: "athena-bot[bot]", Account: "o", Repos: []string{"o/r", "o/s"}})
		}, "bot:          athena-bot[bot]\naccount:      o\nrepositories:\n  o/r\n  o/s\n"},
	} {
		var b bytes.Buffer
		c.print(&b)
		if b.String() != c.want {
			t.Errorf("%s:\n got:\n%s\nwant:\n%s", c.name, b.String(), c.want)
		}
	}
}

func TestCLIProposals(t *testing.T) {
	dir := startServer(t)
	ctx := context.Background()
	if code, out, errOut := athena(ctx, "proposals", "--state-dir", dir); code != 0 || errOut != "" || out != "nothing is held for you\n" {
		t.Fatalf("proposals: %d %q %q", code, out, errOut)
	}
	for _, c := range []struct{ args []string }{{[]string{"apply", "p-1"}}, {[]string{"discard", "p-1"}}} {
		if code, out, errOut := athena(ctx, append(c.args, "--state-dir", dir)...); code != 1 || out != "" || errOut == "" {
			t.Errorf("%v: %d %q %q, want exit 1 for an unknown proposal", c.args, code, out, errOut)
		}
	}
	if code, _, errOut := athena(ctx, "apply", "--state-dir", dir); code != 2 || errOut == "" {
		t.Errorf("apply without an id: %d %q, want a usage error", code, errOut)
	}
}

func TestProposalLineFormat(t *testing.T) {
	got := proposalLine(server.ProposalRow{ID: "p-1", Repo: "acme/web", Kind: "pull_request", Summary: "Open a pull request"})
	if want := "p-1  acme/web  pull_request  Open a pull request"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
	if got := proposalLine(server.ProposalRow{ID: "p-2", Kind: "reply", Summary: "Reply"}); got != "p-2  -  reply  Reply" {
		t.Errorf("a proposal without a repository: %q", got)
	}
	failed := server.ProposalRow{ID: "p-3", Repo: "acme/web", Kind: "push", Summary: "Push b", State: core.ProposalFailed, Error: "remote rejected"}
	if got, want := proposalLine(failed), "p-3  acme/web  push  Push b  failed: remote rejected"; got != want {
		t.Errorf("a failed proposal: %q, want %q", got, want)
	}
}
