package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/danielpina1/athena/internal/runtime/codex"
)

var (
	fakeCodexOnce sync.Once
	fakeCodexPath string
	fakeCodexErr  error
)

// fakeCodex builds fakecodex once per test binary run, into the fakeclaude build's directory (removed by TestMain).
func fakeCodex(t *testing.T) string {
	t.Helper()
	fakeClaude(t) // makes fakeDir
	fakeCodexOnce.Do(func() {
		fakeCodexPath = filepath.Join(fakeDir, "codex")
		out, err := exec.Command("go", "build", "-o", fakeCodexPath,
			"github.com/danielpina1/athena/internal/runtime/codex/testdata/fakecodex").CombinedOutput()
		if err != nil {
			fakeCodexErr = fmt.Errorf("build fakecodex: %v\n%s", err, out)
		}
	})
	if fakeCodexErr != nil {
		t.Fatal(fakeCodexErr)
	}
	return fakeCodexPath
}

// codexState is a state directory whose config.json runs fakecodex, and a home directory (HOME) whose ~/.codex
// must stay untouched.
func codexState(t *testing.T, extra string) (dir, home string) {
	t.Helper()
	dir = newStateDir(t)
	home = t.TempDir()
	t.Setenv("HOME", home)
	cfg := fmt.Sprintf(`{"runtimes": {"codex": {"binary": %q%s}}}`, fakeCodex(t), extra)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, home
}

func TestCodexLoginUsesAthenaHome(t *testing.T) {
	dir, home := codexState(t, "")
	argsLog := filepath.Join(t.TempDir(), "args.json")
	t.Setenv("ATHENA_FAKE_CODEX_ARGS_LOG", argsLog)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex")) // the owner's own setting must not leak through
	prev := Stdin
	t.Cleanup(func() { Stdin = prev })
	Stdin = strings.NewReader("sk-fixture-not-a-key\n")

	code, out, errOut := athena(context.Background(), "codex", "login", "--with-api-key", "--state-dir", dir)
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if strings.Contains(out+errOut, "sk-fixture") {
		t.Fatalf("the key was printed: %q %q", out, errOut)
	}
	b, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Args      []string
		CodexHome string
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "codex"); rec.CodexHome != want || !reflect.DeepEqual(rec.Args, []string{"login", "--with-api-key"}) {
		t.Fatalf("codex ran with %+v, want CODEX_HOME=%s and login --with-api-key", rec, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("~/.codex was touched: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "codex"))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("CODEX_HOME: %v, %v", fi, err)
	}
	// Athena's config.toml is in place for the login.
	if _, err := os.Stat(filepath.Join(dir, "codex", "config.toml")); err != nil {
		t.Fatal(err)
	}

	// Without the flag: plain `codex login`.
	if code, _, errOut := athena(context.Background(), "codex", "login", "--state-dir", dir); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if b, _ := os.ReadFile(argsLog); !strings.Contains(string(b), `"args":["login"]`) {
		t.Fatalf("args log = %s", b)
	}
}

func TestCodexStatus(t *testing.T) {
	dir, _ := codexState(t, "")
	code, out, errOut := athena(context.Background(), "codex", "status", "--state-dir", dir)
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	for _, want := range []string{"installed: yes\n", "version:   0.160.1 (tested)\n", "config:    current\n", "logged in: yes (chatgpt)\n", "hooks:     trusted\n", "ready:     yes\n",
		"CODEX_HOME=" + filepath.Join(dir, "codex")} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "acct_fixture") || strings.Contains(out, "plus") {
		t.Errorf("status shows more of the account than its kind:\n%s", out)
	}

	t.Setenv("ATHENA_FAKE_CODEX_LOGGED_OUT", "1")
	code, out, _ = athena(context.Background(), "codex", "status", "--state-dir", dir)
	if code != 1 || !strings.Contains(out, "logged in: no\n") || !strings.Contains(out, "ready:     no: Codex is not logged in for Athena: run athena codex login\n") {
		t.Fatalf("logged out: exit %d\n%s", code, out)
	}

	dir, _ = codexState(t, `, "enabled": false`)
	if code, out, _ := athena(context.Background(), "codex", "status", "--state-dir", dir); code != 1 || !strings.Contains(out, "disabled") {
		t.Fatalf("disabled: exit %d\n%s", code, out)
	}
	for _, args := range [][]string{{"codex"}, {"codex", "nope"}, {"codex", "status", "x"}, {"codex", "login", "--bogus"}} {
		if code, _, errOut := athena(context.Background(), append(args, "--state-dir", dir)...); code != 2 || errOut == "" {
			t.Errorf("%v: exit %d, stderr %q; want 2 with usage", args, code, errOut)
		}
	}
}

// The athena codex commands never rewrite the server's config.toml: one written for another athena binary (with the
// trust Codex recorded) stays byte for byte, and the mismatch is reported; without one, they write it.
func TestCodexCommandsLeaveTheServersConfig(t *testing.T) {
	dir, _ := codexState(t, "")
	home := filepath.Join(dir, "codex")
	path := filepath.Join(home, "config.toml")
	// No file: status writes it.
	if code, out, errOut := athena(context.Background(), "codex", "status", "--state-dir", dir); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("status wrote no config.toml: %v", err)
	}
	// The server's own, for its binary, with trust recorded.
	if err := codex.WriteHome(codex.Home{Dir: home, AthenaBin: "/opt/server/bin/athena"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	server := string(b) + "\n[hooks.state.\"" + path + ":pre_tool_use:0:0\"]\ntrusted_hash = \"sha256:00\"\n"
	if err := os.WriteFile(path, []byte(server), 0o600); err != nil {
		t.Fatal(err)
	}
	unchanged := func(what string) {
		t.Helper()
		if b, err := os.ReadFile(path); err != nil || string(b) != server {
			t.Fatalf("%s rewrote config.toml (%v):\n%s", what, err, b)
		}
	}
	code, out, _ := athena(context.Background(), "codex", "status", "--state-dir", dir)
	unchanged("status")
	if code != 1 || !strings.Contains(out, "config:    differs from what this athena writes; left as it is (restart the server to rewrite it)\n") ||
		!strings.Contains(out, "ready:     no: "+codex.StaleHome) || !strings.Contains(out, "logged in: yes (chatgpt)") {
		t.Fatalf("status: exit %d\n%s", code, out)
	}
	code, _, errOut := athena(context.Background(), "codex", "login", "--state-dir", dir)
	unchanged("login")
	if code != 0 || !strings.Contains(errOut, codex.StaleHome) {
		t.Fatalf("login: exit %d, stderr %q", code, errOut)
	}
}
