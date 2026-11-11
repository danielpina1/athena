package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	fakeOnce sync.Once
	fakePath string
	fakeErr  error
	fakeDir  string
)

// fakeClaude builds the runtime package's fake `claude` once per test binary run.
func fakeClaude(t *testing.T) string {
	t.Helper()
	fakeOnce.Do(func() {
		fakeDir, fakeErr = os.MkdirTemp("", "fakeclaude")
		if fakeErr != nil {
			return
		}
		fakePath = filepath.Join(fakeDir, "claude")
		out, err := exec.Command("go", "build", "-o", fakePath,
			"github.com/danielpina1/athena/internal/runtime/claude/testdata/fakeclaude").CombinedOutput()
		if err != nil {
			fakeErr = fmt.Errorf("build fakeclaude: %v\n%s", err, out)
		}
	})
	if fakeErr != nil {
		t.Fatal(fakeErr)
	}
	return fakePath
}

func TestMain(m *testing.M) {
	code := m.Run()
	if fakeDir != "" {
		os.RemoveAll(fakeDir)
	}
	os.Exit(code)
}

// testCtx fails a hung session instead of hanging the test.
func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func abs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestClaudeCompleterOneTurn(t *testing.T) {
	state := t.TempDir()
	argsLog := filepath.Join(t.TempDir(), "args.json")
	// The worker fixture says "Done."; the planner's role replays the planner fixture instead.
	t.Setenv("ATHENA_FAKE_FIXTURE", abs(t, "../runtime/claude/testdata/claude/simple-turn.jsonl"))
	t.Setenv("ATHENA_FAKE_PLANNER_FIXTURE", abs(t, "testdata/planner-turn.jsonl"))
	t.Setenv("ATHENA_FAKE_ARGS_LOG", argsLog)

	s, err := New(Config{Binary: fakeClaude(t), StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	c := s.(LLM).C.(*Claude)
	var pid int
	c.onStart = func(p int) { pid = p }
	text, err := c.Complete(testCtx(t), "SYSTEM", "USER")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, `{"tasks":[`) {
		t.Errorf("text = %q", text)
	}

	var rec struct {
		Args []string          `json:"args"`
		Env  map[string]string `json:"env"`
	}
	b, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(rec.Args, "--tools"); i < 0 || rec.Args[i+1] != "" {
		t.Errorf("args lack --tools \"\": %q", rec.Args)
	}
	if i := slices.Index(rec.Args, "--model"); i < 0 || rec.Args[i+1] != "haiku" {
		t.Errorf("args lack --model haiku: %q", rec.Args)
	}
	if i := slices.Index(rec.Args, "--append-system-prompt"); i < 0 || rec.Args[i+1] != "SYSTEM" {
		t.Errorf("args lack the system prompt: %q", rec.Args)
	}
	if rec.Env["ATHENA_ROLE"] != "planner" {
		t.Errorf("ATHENA_ROLE = %q", rec.Env["ATHENA_ROLE"])
	}
	for flag, want := range map[string]string{"--settings": `{"hooks":{}}`, "--mcp-config": `{"mcpServers":{}}`} {
		i := slices.Index(rec.Args, flag)
		if i < 0 {
			t.Errorf("args lack %s", flag)
			continue
		}
		got, err := os.ReadFile(rec.Args[i+1])
		if err != nil || strings.TrimSpace(string(got)) != want {
			t.Errorf("%s file = %q, %v", flag, got, err)
		}
	}
	tr, err := os.ReadFile(filepath.Join(state, "transcripts", "planner.jsonl"))
	if err != nil || !strings.Contains(string(tr), `"type":"result"`) {
		t.Errorf("transcript = %d bytes, %v", len(tr), err)
	}
	if pid == 0 {
		t.Fatal("onStart was never called")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the planner process is still there: kill 0 = %v", err)
	}
}

func TestClaudeCompleterErrorResult(t *testing.T) {
	fx := filepath.Join(t.TempDir(), "err.jsonl")
	if err := os.WriteFile(fx, []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"","errors":["boom"]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATHENA_FAKE_PLANNER_FIXTURE", fx)
	c := &Claude{Binary: fakeClaude(t), Model: "haiku", Dir: t.TempDir(), StateDir: t.TempDir()}
	if _, err := c.Complete(testCtx(t), "S", "U"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}
