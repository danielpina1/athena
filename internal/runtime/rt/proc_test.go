package rt

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestProcKillsGroupAfterGrace: a process that ignores its stdin closing is killed, with every process of its group,
// once the close grace has passed; Killed says so and Wait reports the kill.
func TestProcKillsGroupAfterGrace(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	p, err := StartProc(context.Background(), ProcOptions{
		Binary: "/bin/sh",
		// The shell ignores stdin and waits on a child of its own group.
		Args:       []string{"-c", `sleep 60 & echo $! > "$PIDFILE"; wait`},
		Dir:        t.TempDir(),
		Env:        []string{"PATH=" + os.Getenv("PATH"), "PIDFILE=" + pidFile},
		CloseGrace: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stdout().Close()
	child := waitPid(t, pidFile)
	if pgid, err := syscall.Getpgid(p.Pid()); err != nil || pgid != p.Pid() {
		t.Fatalf("pgid = %d, %v; want the process to lead its own group (%d)", pgid, err, p.Pid())
	}
	start := time.Now()
	p.CloseStdin()
	if d := time.Since(start); d < 250*time.Millisecond || d > 5*time.Second {
		t.Fatalf("CloseStdin took %v, want ~300ms", d)
	}
	if !p.Killed() {
		t.Fatal("Killed = false after the grace ran out")
	}
	if err := p.Wait(); err == nil {
		t.Fatal("Wait should report the kill")
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(child, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the group's other process survived")
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.CloseStdin() // idempotent once exited
	if err := p.KillGroup(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("KillGroup after exit = %v, want os.ErrProcessDone", err)
	}
}

// A process that ends when its stdin closes is not killed; its stdout and its stderr tail are read.
func TestProcCooperativeExit(t *testing.T) {
	p, err := StartProc(context.Background(), ProcOptions{
		Binary: "/bin/sh",
		Args:   []string{"-c", `cat; echo out; echo err >&2`},
		Dir:    t.TempDir(),
		Env:    []string{"PATH=" + os.Getenv("PATH")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(p.Stdin(), "hello\n"); err != nil {
		t.Fatal(err)
	}
	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(p.Stdout())
		out <- string(b)
	}()
	p.CloseStdin()
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait = %v", err)
	}
	if p.Killed() {
		t.Fatal("a cooperative process must not be killed")
	}
	if got := <-out; got != "hello\nout\n" {
		t.Fatalf("stdout = %q", got)
	}
	if got := p.Stderr(); got != "err\n" {
		t.Fatalf("stderr = %q", got)
	}
	select {
	case <-p.Exited():
	default:
		t.Fatal("Exited not closed after Wait")
	}
}

func TestProcContextCancelKills(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, err := StartProc(ctx, ProcOptions{Binary: "/bin/sleep", Args: []string{"60"}, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stdout().Close()
	cancel()
	if err := p.Wait(); err == nil {
		t.Fatal("Wait should report the kill")
	}
	if !p.Killed() {
		t.Fatal("Killed = false after the context was cancelled")
	}
}

func TestProcStderrTailIsBounded(t *testing.T) {
	p, err := StartProc(context.Background(), ProcOptions{
		Binary: "/bin/sh",
		Args:   []string{"-c", `i=0; while [ $i -lt 2000 ]; do echo "line $i" >&2; i=$((i+1)); done; echo last >&2`},
		Dir:    t.TempDir(),
		Env:    []string{"PATH=" + os.Getenv("PATH")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stdout().Close()
	_ = p.Wait()
	if got := p.Stderr(); len(got) != stderrTail || !strings.HasSuffix(got, "last\n") {
		t.Fatalf("stderr tail is %d bytes ending %q, want %d ending in the last line", len(got), got[max(0, len(got)-10):], stderrTail)
	}
}

func TestStartProcErrors(t *testing.T) {
	if _, err := StartProc(context.Background(), ProcOptions{Binary: "/nonexistent/binary", Dir: t.TempDir()}); err == nil {
		t.Fatal("a missing binary must fail")
	}
}

func waitPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never wrote its pid")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestChildEnvKeepsRuntimePrefixesDropsSecrets: a session inherits only the basic process variables, the locale, XDG
// and Athena's variables without secret-looking names, plus the variables under the runtime's own prefixes whatever
// their names (they carry its credentials), plus extra. Another runtime's prefixes, the server's tokens and cloud keys
// never pass.
func TestChildEnvKeepsRuntimePrefixesDropsSecrets(t *testing.T) {
	parent := []string{
		"PATH=/bin", "HOME=/h", "LANG=C", "LC_ALL=C", "XDG_CONFIG_HOME=/xdg", "FOO=bar", "NOEQUALS",
		"GITHUB_TOKEN=ghp_parent", "AWS_SECRET_ACCESS_KEY=aws",
		"ATHENA_FROM_PARENT=kept", "ATHENA_TOKEN=dropped",
		"ANTHROPIC_API_KEY=sk-ant", "CLAUDE_CODE_OAUTH_TOKEN=oauth",
		"OPENAI_API_KEY=sk-openai", "CODEX_HOME=/c",
	}
	env := func(keep ...string) map[string]string {
		m := map[string]string{}
		for _, kv := range ChildEnv(parent, []string{"ATHENA_TICKET=T-7"}, keep...) {
			k, v, _ := strings.Cut(kv, "=")
			m[k] = v
		}
		return m
	}
	common := map[string]string{"PATH": "/bin", "HOME": "/h", "LANG": "C", "LC_ALL": "C", "XDG_CONFIG_HOME": "/xdg",
		"ATHENA_FROM_PARENT": "kept", "ATHENA_TICKET": "T-7"}
	never := []string{"FOO", "NOEQUALS", "GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY", "ATHENA_TOKEN"}
	for name, c := range map[string]struct {
		keep      []string
		has, lack []string
	}{
		"claude": {[]string{"CLAUDE_", "ANTHROPIC_"}, []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}, []string{"OPENAI_API_KEY", "CODEX_HOME"}},
		"codex":  {[]string{"CODEX_", "OPENAI_"}, []string{"OPENAI_API_KEY", "CODEX_HOME"}, []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}},
		"none":   {nil, nil, []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "CODEX_HOME"}},
	} {
		got := env(c.keep...)
		for k, v := range common {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", name, k, got[k], v)
			}
		}
		for _, k := range c.has {
			if _, ok := got[k]; !ok {
				t.Errorf("%s: %s missing", name, k)
			}
		}
		for _, k := range append(c.lack, never...) {
			if _, ok := got[k]; ok {
				t.Errorf("%s: child has %s", name, k)
			}
		}
	}
}
