package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLoadEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env.log")
	if err := os.WriteFile(p, []byte("PATH=/bin\nATHENA_TICKET=T-1\nEMPTY=\nATHENA_DIR=/w/a=b\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := loadEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"PATH=/bin", "ATHENA_TICKET=T-1", "EMPTY=", "ATHENA_DIR=/w/a=b"}; !slices.Equal(env, want) {
		t.Fatalf("env = %q, want %q", env, want)
	}
	if got := lookup(env, "ATHENA_DIR"); got != "/w/a=b" {
		t.Fatalf("lookup = %q", got)
	}
	if _, err := loadEnv(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing log loaded")
	}
}

func TestHookInput(t *testing.T) {
	raw, err := hookInput([]string{"ATHENA_DIR=/w"}, "Bash", `{"command":"make deploy"}`)
	if err != nil {
		t.Fatal(err)
	}
	var in struct {
		Event string          `json:"hook_event_name"`
		Tool  string          `json:"tool_name"`
		Input json.RawMessage `json:"tool_input"`
		Cwd   string          `json:"cwd"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	if in.Event != "PreToolUse" || in.Tool != "Bash" || string(in.Input) != `{"command":"make deploy"}` || in.Cwd != "/w" {
		t.Fatalf("hook input = %s", raw)
	}
	if _, err := hookInput(nil, "Bash", "not json"); err == nil {
		t.Fatal("a non-JSON tool input was accepted")
	}
}
