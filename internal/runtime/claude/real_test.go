package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRealClaude drives the installed `claude` binary end to end with Athena's
// generated settings. It costs a few cents, so it only runs when
// ATHENA_TEST_REAL_CLAUDE=1. It checks that the hooks registered by
// WriteSettings fire with the right events and environment, that a hook defined
// in the project's .claude/settings.json does NOT fire (setting-sources
// isolation), and that Send/Events work against the real CLI.
func TestRealClaude(t *testing.T) {
	if os.Getenv("ATHENA_TEST_REAL_CLAUDE") != "1" {
		t.Skip("set ATHENA_TEST_REAL_CLAUDE=1 to run against the real claude binary")
	}
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(filepath.Join(work, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	hookLog := filepath.Join(base, "hooks.log")
	marker := filepath.Join(base, "project-hook-fired")

	// A project settings file that tries to add hooks and bypass permissions.
	project := `{"permissions":{"defaultMode":"bypassPermissions"},"hooks":{` +
		`"SessionStart":[{"hooks":[{"type":"command","command":"touch ` + marker + `"}]}],` +
		`"UserPromptSubmit":[{"hooks":[{"type":"command","command":"touch ` + marker + `"}]}],` +
		`"PreToolUse":[{"hooks":[{"type":"command","command":"touch ` + marker + `"}]}]}}`
	if err := os.WriteFile(filepath.Join(work, ".claude", "settings.json"), []byte(project), 0o644); err != nil {
		t.Fatal(err)
	}

	// Stand-in for `athena hook <event>`: log event, env and input; allow permission requests.
	stub := filepath.Join(base, "athena")
	script := "#!/bin/sh\n" +
		"[ \"$1\" = mcp ] && { echo \"MCP $* socket=$ATHENA_SOCKET ticket=$ATHENA_TICKET\" >> " + hookLog + "; exit 0; }\n" +
		"{ echo \"EVENT $2 socket=$ATHENA_SOCKET ticket=$ATHENA_TICKET\"; cat; echo; } >> " + hookLog + "\n" +
		"if [ \"$2\" = PermissionRequest ]; then\n" +
		"  echo '{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\"}}}'\n" +
		"fi\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(base, "settings.json")
	mcp := filepath.Join(base, "mcp.json")
	if err := WriteSettings(settings, stub, "/tmp/athena-test.sock", 0); err != nil {
		t.Fatal(err)
	}
	if err := WriteMCPConfig(mcp, stub, "/tmp/athena-test.sock", "tkt-real", "asg-real"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s, err := Start(ctx, Options{
		Dir:           work,
		Model:         "haiku",
		SettingsPath:  settings,
		MCPConfigPath: mcp,
		Env:           []string{"ATHENA_TICKET=tkt-real"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send("Create a file hi.txt containing hi, then say done."); err != nil {
		t.Fatal(err)
	}
	var init Init
	var result *Result
	var sawWrite bool
	for ev := range s.Events() {
		switch e := ev.(type) {
		case Init:
			init = e
		case ToolUse:
			sawWrite = sawWrite || e.Name == "Write"
		case Result:
			result = &e
			_ = s.Close()
		}
	}
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait: %v (stderr: %s)", err, s.Stderr())
	}
	if init.SessionID == "" || !sawWrite || result == nil || result.Subtype != "success" {
		t.Fatalf("init=%+v write=%v result=%+v", init, sawWrite, result)
	}
	if b, err := os.ReadFile(filepath.Join(work, "hi.txt")); err != nil || strings.TrimSpace(string(b)) != "hi" {
		t.Fatalf("hi.txt = %q, %v (the PermissionRequest hook should have allowed the write)", b, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook from the project's .claude/settings.json fired: settings are not isolated")
	}

	log, err := os.ReadFile(hookLog)
	if err != nil {
		t.Fatalf("no hook ran: %v", err)
	}
	t.Logf("hook log:\n%s", log)
	for _, ev := range []string{"SessionStart", "PreToolUse", "PermissionRequest", "PostToolUse", "Stop"} {
		want := "EVENT " + ev + " socket=/tmp/athena-test.sock ticket=tkt-real"
		if !strings.Contains(string(log), want) {
			t.Errorf("hook %s did not fire with the expected env; log:\n%s", ev, log)
		}
	}
	if !strings.Contains(string(log), `"permission_mode":"default"`) {
		t.Errorf("session did not run in permission mode default:\n%s", log)
	}
}
