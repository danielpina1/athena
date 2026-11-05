package codex

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/runtime/rt"
)

var _ interface {
	Name() string
	Check(context.Context) rt.Info
	Prepare(rt.Prepare) error
} = (*Runtime)(nil)

// newRuntime is a Runtime on fakecodex, with its home and work root in a temporary directory; env (the
// ATHENA_FAKE_CODEX_* knobs) is set in the test's environment, which Check passes on to Codex.
func newRuntime(t *testing.T, env ...string) (*Runtime, string) {
	t.Helper()
	dir := t.TempDir()
	rpcLog := filepath.Join(dir, "rpc.jsonl")
	t.Setenv("ATHENA_FAKE_CODEX_RPC_LOG", rpcLog)
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	return &Runtime{Binary: fakeBinary(t), Home: filepath.Join(dir, "codex"), AthenaBin: testAthenaBin,
		Socket: filepath.Join(dir, "node.sock"), WorkRoot: filepath.Join(dir, "work")}, rpcLog
}

// methods returns the methods of the requests in an RPC log.
func methods(t *testing.T, rpcLog string) []string {
	t.Helper()
	b, err := os.ReadFile(rpcLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m struct {
			Method string
			ID     json.RawMessage
		}
		if json.Unmarshal([]byte(l), &m) == nil && m.Method != "" && len(m.ID) > 0 {
			out = append(out, m.Method)
		}
	}
	return out
}

func TestCheckReady(t *testing.T) {
	r, rpcLog := newRuntime(t)
	s := r.Status(context.Background())
	want := Status{Info: rt.Info{Installed: true, Ready: true, Tested: true, Version: VerifiedVersion}, Account: "chatgpt", Hooks: "trusted", AccountAsked: true}
	if s != want {
		t.Fatalf("status = %+v, want %+v", s, want)
	}
	if got := methods(t, rpcLog); !slices.Equal(got, []string{"initialize", "account/read", "hooks/list"}) {
		t.Fatalf("requests = %v", got)
	}
	if m := mode(t, filepath.Join(r.Home, "config.toml")); m != 0o600 {
		t.Errorf("config.toml mode = %o", m)
	}
	if r.Name() != "codex" || r.Prepare(rt.Prepare{WorkDir: t.TempDir()}) != nil {
		t.Fatal("Name or Prepare")
	}
}

func TestCheckNotLoggedIn(t *testing.T) {
	r, _ := newRuntime(t, "ATHENA_FAKE_CODEX_LOGGED_OUT=1")
	s := r.Status(context.Background())
	if s.Ready || !s.Installed || s.Why != NotLoggedIn || s.Account != "" || !s.AccountAsked {
		t.Fatalf("status = %+v", s)
	}
	if NotLoggedIn != "Codex is not logged in for Athena: run athena codex login" {
		t.Fatalf("NotLoggedIn = %q", NotLoggedIn)
	}
}

func TestCheckUntestedVersionWarns(t *testing.T) {
	r, _ := newRuntime(t, "ATHENA_FAKE_CODEX_VERSION=0.161.0")
	info := r.Check(context.Background())
	if !info.Ready || info.Tested || info.Version != "0.161.0" || info.Why != "untested Codex version 0.161.0: newer than the verified "+VerifiedVersion {
		t.Fatalf("info = %+v", info)
	}
	// A newer version still runs only if its hooks pass.
	r, _ = newRuntime(t, "ATHENA_FAKE_CODEX_VERSION=0.161.0", "ATHENA_FAKE_CODEX_HOOKS=foreign")
	if info := r.Check(context.Background()); info.Ready || !strings.Contains(info.Why, "did not write") {
		t.Fatalf("info = %+v", info)
	}
}

func TestCheckRefusesAnOldOrMissingCodex(t *testing.T) {
	r, rpcLog := newRuntime(t, "ATHENA_FAKE_CODEX_VERSION=0.159.9")
	if info := r.Check(context.Background()); info.Ready || !info.Installed || info.Why != "Codex 0.159.9 is older than the supported "+MinVersion {
		t.Fatalf("info = %+v", info)
	}
	if got := methods(t, rpcLog); len(got) != 0 {
		t.Fatalf("an unsupported Codex was asked %v", got)
	}
	r, _ = newRuntime(t)
	missing := filepath.Join(t.TempDir(), "codex")
	r.Binary = missing
	if info := r.Check(context.Background()); info.Installed || info.Ready || info.Why != "Codex is not installed ("+missing+" not found)" {
		t.Fatalf("info = %+v", info)
	}
	r.Binary = "athena-no-such-codex"
	r.Invalidate()
	if info := r.Check(context.Background()); info.Installed || info.Why != "Codex is not installed (athena-no-such-codex not found)" {
		t.Fatalf("info = %+v", info)
	}
	notCodex := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(notCodex, []byte("#!/bin/sh\necho something else\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	r.Binary = notCodex
	r.Invalidate()
	if info := r.Check(context.Background()); !info.Installed || info.Ready || !strings.Contains(info.Why, "no \"codex-cli X.Y.Z\"") {
		t.Fatalf("not codex: %+v", info)
	}
	r.Home = ""
	r.Invalidate()
	if info := r.Check(context.Background()); info.Ready || !strings.Contains(info.Why, "CODEX_HOME") {
		t.Fatalf("no home: %+v", info)
	}
}

// Foreign or untrusted hooks keep Codex from being Ready, with TrustHooks' reason.
func TestCheckHooksMustBeAthenas(t *testing.T) {
	for mode, why := range map[string]string{
		"foreign":       "a preToolUse hook Athena did not write is enabled (project, from ",
		"never-trusted": `Athena's PreToolUse hook is "untrusted", not trusted`,
	} {
		r, _ := newRuntime(t, "ATHENA_FAKE_CODEX_HOOKS="+mode)
		s := r.Status(context.Background())
		if s.Ready || !strings.HasPrefix(s.Why, why) || s.Hooks != s.Why || s.Account != "chatgpt" {
			t.Errorf("%s: status = %+v", mode, s)
		}
	}
	// Untrusted hooks are trusted once, and stay trusted across later checks (WriteHome keeps the recorded trust).
	r, rpcLog := newRuntime(t, "ATHENA_FAKE_CODEX_HOOKS=untrusted")
	for i := 0; i < 2; i++ {
		r.Invalidate()
		if s := r.Status(context.Background()); !s.Ready || s.Hooks != "trusted" {
			t.Fatalf("check %d: %+v", i, s)
		}
	}
	n := 0
	for _, m := range methods(t, rpcLog) {
		if m == "config/batchWrite" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d batchWrites over two checks, want 1", n)
	}
}

func TestCheckIsCachedUntilInvalidated(t *testing.T) {
	r, rpcLog := newRuntime(t)
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	starts := func() int {
		n := 0
		for _, m := range methods(t, rpcLog) {
			if m == "initialize" {
				n++
			}
		}
		return n
	}
	r.Check(context.Background())
	now = now.Add(9 * time.Minute)
	r.Check(context.Background())
	if n := starts(); n != 1 {
		t.Fatalf("%d app-servers within 10 minutes, want 1", n)
	}
	now = now.Add(2 * time.Minute)
	r.Check(context.Background())
	if n := starts(); n != 2 {
		t.Fatalf("%d app-servers after 11 minutes, want 2", n)
	}
	r.Invalidate()
	r.Check(context.Background())
	if n := starts(); n != 3 {
		t.Fatalf("%d app-servers after Invalidate, want 3", n)
	}
	// A check cut short is not kept.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Invalidate()
	if info := r.Check(ctx); info.Ready {
		t.Fatalf("a cancelled check is ready: %+v", info)
	}
	r.Check(context.Background())
	if n := starts(); n != 4 {
		t.Fatalf("%d app-servers, want 4: the cancelled check was cached", n)
	}
}

// A check its own 30-second bound cut short is not kept: the next Check asks again.
func TestCheckTimeoutIsNotCached(t *testing.T) {
	r, _ := newRuntime(t)
	slow := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nexec sleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := r.Binary
	r.Binary, r.timeout = slow, 200*time.Millisecond
	start := time.Now()
	if s := r.Status(context.Background()); s.Ready || !strings.HasPrefix(s.Why, "codex --version") || s.AccountAsked {
		t.Fatalf("slow codex: %+v", s)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the check took %s, past its bound", d)
	}
	r.Binary, r.timeout = fake, 0
	if s := r.Status(context.Background()); !s.Ready {
		t.Fatalf("the timed-out check was cached: %+v", s)
	}
}

// With KeepHome (the athena codex commands) an existing config.toml another athena wrote is left byte for byte, and
// reported; a missing one is written; one that is current is checked as usual.
func TestCheckKeepHomeNeverRewrites(t *testing.T) {
	r, rpcLog := newRuntime(t)
	r.KeepHome = true
	// No file: written, and checked.
	if s := r.Status(context.Background()); !s.Ready || s.HomeStale {
		t.Fatalf("no file: %+v", s)
	}
	path := filepath.Join(r.Home, "config.toml")
	mine := readFile(t, path)
	// The server's file, written for another binary, with trust Codex recorded.
	server := Home{Dir: r.Home, AthenaBin: "/opt/server/athena", Socket: r.Socket}
	if err := WriteHome(server); err != nil {
		t.Fatal(err)
	}
	trusted := readFile(t, path) + "\n[hooks.state." + tomlString(path+":pre_tool_use:0:0") + "]\ntrusted_hash = \"sha256:00\"\n"
	if err := os.WriteFile(path, []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	r.Invalidate()
	before := len(methods(t, rpcLog))
	s := r.Status(context.Background())
	if s.Ready || !s.HomeStale || s.Why != StaleHome || s.Account != "chatgpt" || !s.AccountAsked || s.Hooks != "not checked: "+StaleHome {
		t.Fatalf("stale: %+v", s)
	}
	if got := readFile(t, path); got != trusted {
		t.Fatalf("config.toml was rewritten:\n%s", got)
	}
	for _, m := range methods(t, rpcLog)[before:] {
		if m == "hooks/list" || m == "config/batchWrite" {
			t.Fatalf("a stale home's hooks were asked about or trusted: %s", m)
		}
	}
	// Login leaves it too, and says so.
	var stderr strings.Builder
	if err := r.Login(context.Background(), false, strings.NewReader(""), io.Discard, &stderr); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != trusted || !strings.Contains(stderr.String(), StaleHome) {
		t.Fatalf("login: stderr %q, file rewritten: %v", stderr.String(), got != trusted)
	}
	// The server (KeepHome false) does rewrite it.
	r.KeepHome = false
	r.Invalidate()
	if s := r.Status(context.Background()); !s.Ready || s.HomeStale {
		t.Fatalf("server: %+v", s)
	}
	if got := readFile(t, path); !strings.HasPrefix(got, mine) {
		t.Fatalf("the server did not rewrite it:\n%s", got)
	}
}

// An app-server that stops at start (here: fakecodex without its protocol.json; for Codex, most often --strict-config
// refusing config.toml) fails the check with the last line it printed.
func TestCheckReportsWhyTheAppServerStopped(t *testing.T) {
	r, _ := newRuntime(t, "ATHENA_FAKE_CODEX_PROTOCOL="+filepath.Join(t.TempDir(), "missing.json"))
	s := r.Status(context.Background())
	if s.Ready || !s.Installed || !strings.HasPrefix(s.Why, "codex app-server initialize: codex: connection closed: fakecodex: protocol.json not usable at ") {
		t.Fatalf("status = %+v", s)
	}
}

// A hook whose recorded trust no longer matches (Codex says "modified") is trusted again by the next check.
func TestCheckRetrustsAModifiedHook(t *testing.T) {
	r, _ := newRuntime(t, "ATHENA_FAKE_CODEX_HOOKS=untrusted")
	if s := r.Status(context.Background()); !s.Ready {
		t.Fatalf("status = %+v", s)
	}
	path := filepath.Join(r.Home, "config.toml")
	cfg := strings.Replace(readFile(t, path), "trusted_hash = \"sha256:", "trusted_hash = \"sha256:00", 1)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	r.Invalidate()
	if s := r.Status(context.Background()); !s.Ready || s.Hooks != "trusted" {
		t.Fatalf("a modified hook is not trusted again: %+v", s)
	}
}

func TestEnvKeepsCodexVariablesAndSetsAthenasHome(t *testing.T) {
	parent := []string{"PATH=/bin", "HOME=/home/u", "CODEX_HOME=/home/u/.codex", "CODEX_API_KEY_FILE=/k", "OPENAI_BASE_URL=https://x",
		"GITHUB_TOKEN=ghs_secret", "ANTHROPIC_API_KEY=sk-ant", "AWS_SECRET_ACCESS_KEY=s", "ATHENA_FAKE_CODEX_HOOKS=trusted"}
	got := Env(parent, "/state/codex")
	want := []string{"PATH=/bin", "HOME=/home/u", "CODEX_API_KEY_FILE=/k", "OPENAI_BASE_URL=https://x", "ATHENA_FAKE_CODEX_HOOKS=trusted", "CODEX_HOME=/state/codex"}
	if !slices.Equal(got, want) {
		t.Fatalf("Env = %v, want %v", got, want)
	}
}

// Codex reports a hook's sourcePath (and its key) with the home's symlinks resolved; a Home reached through a symlink
// (macOS's /var/folders is /private/var/folders) is still Athena's, and its recorded trust survives a recheck.
func TestCheckHomeThroughASymlink(t *testing.T) {
	r, rpcLog := newRuntime(t, "ATHENA_FAKE_CODEX_HOOKS=untrusted")
	real := filepath.Join(t.TempDir(), "real-state")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	r.Home = filepath.Join(link, "codex")
	for i := 0; i < 2; i++ {
		r.Invalidate()
		if s := r.Status(context.Background()); !s.Ready || s.Hooks != "trusted" {
			t.Fatalf("check %d: %+v", i, s)
		}
	}
	n := 0
	for _, m := range methods(t, rpcLog) {
		if m == "config/batchWrite" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d batchWrites over two checks, want 1: the trust recorded under the resolved path was dropped", n)
	}
	// The CLI's comparison sees the trust recorded under the resolved path as current, too.
	if exists, cur, err := HomeCurrent(r.home()); !exists || !cur || err != nil {
		t.Fatalf("HomeCurrent: exists=%v current=%v err=%v", exists, cur, err)
	}
}
