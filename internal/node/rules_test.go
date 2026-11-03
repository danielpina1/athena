package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type ruleEnv struct {
	ctx    RuleContext
	root   string
	outDir string
}

// newRuleEnv builds a state dir, a ticket dir beside it, a home dir, an outside dir and
// a symlink inside the ticket dir that points at the outside dir.
func newRuleEnv(t *testing.T) ruleEnv {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	state := filepath.Join(root, "state")
	ticket := filepath.Join(root, "work", "T1")
	out := filepath.Join(root, "out")
	for _, d := range []string{home, ticket, out, filepath.Join(home, ".ssh")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(out, filepath.Join(ticket, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".ssh"), filepath.Join(ticket, "sshlink")); err != nil {
		t.Fatal(err)
	}
	return ruleEnv{
		root:   root,
		outDir: out,
		ctx: RuleContext{
			TicketDir: ticket,
			Home:      home,
			StateDir:  state,
			Blocked:   []string{"rm", "git push"},
		},
	}
}

func in(kv map[string]any) json.RawMessage {
	b, err := json.Marshal(kv)
	if err != nil {
		panic(err)
	}
	return b
}

func TestEvaluate(t *testing.T) {
	e := newRuleEnv(t)
	c := e.ctx
	h := c.Home
	cases := []struct {
		name  string
		tool  string
		input json.RawMessage
		want  string
		rule  string // expected Rule prefix; empty = don't care
	}{
		// credential reads
		{"read ssh key", "Read", in(map[string]any{"file_path": h + "/.ssh/id_rsa"}), "deny", "credential"},
		{"read aws tilde", "Read", in(map[string]any{"file_path": "~/.aws/credentials"}), "deny", "credential"},
		{"grep gh config", "Grep", in(map[string]any{"pattern": "token", "path": h + "/.config/gh"}), "deny", "credential"},
		{"grep home ancestor", "Grep", in(map[string]any{"pattern": "token", "path": h}), "deny", "credential"},
		{"glob home ancestor", "Glob", in(map[string]any{"pattern": "**/*", "path": h}), "deny", "credential"},
		{"glob absolute pattern", "Glob", in(map[string]any{"pattern": h + "/.ssh/*"}), "deny", "credential"},
		{"read netrc", "Read", in(map[string]any{"file_path": h + "/.netrc"}), "deny", "credential"},
		{"read codex auth", "Read", in(map[string]any{"file_path": h + "/.codex/auth.json"}), "deny", "credential"},
		{"read claude credentials", "Read", in(map[string]any{"file_path": h + "/.claude/.credentials.json"}), "deny", "credential"},
		{"read claude alt credentials", "Read", in(map[string]any{"file_path": h + "/.claude-work/.credentials.json"}), "deny", "credential"},
		{"read claude.json", "Read", in(map[string]any{"file_path": h + "/.claude.json"}), "deny", "credential"},
		{"read claude settings ok", "Read", in(map[string]any{"file_path": h + "/.claude/settings.json"}), "allow", ""},
		{"read state db", "Read", in(map[string]any{"file_path": c.StateDir + "/athena.db"}), "deny", "credential"},
		{"edit state file", "Edit", in(map[string]any{"file_path": c.StateDir + "/x"}), "deny", "credential"},
		{"write ssh", "Write", in(map[string]any{"file_path": h + "/.ssh/authorized_keys", "content": "x"}), "deny", "credential"},
		{"read via symlink into ssh", "Read", in(map[string]any{"file_path": c.TicketDir + "/sshlink/id_rsa"}), "deny", "credential"},
		{"read ticket file", "Read", in(map[string]any{"file_path": c.TicketDir + "/main.go"}), "allow", ""},
		{"read relative", "Read", in(map[string]any{"file_path": "main.go"}), "allow", ""},
		{"grep ticket dir", "Grep", in(map[string]any{"pattern": "x", "path": c.TicketDir}), "allow", ""},
		{"bash cat ssh", "Bash", in(map[string]any{"command": "cat ~/.ssh/id_rsa"}), "deny", "credential"},
		{"bash cat HOME aws", "Bash", in(map[string]any{"command": "cat $HOME/.aws/credentials"}), "deny", "credential"},
		{"bash cat braces HOME", "Bash", in(map[string]any{"command": "cat ${HOME}/.config/gh/hosts.yml"}), "deny", "credential"},
		{"bash cat abs netrc", "Bash", in(map[string]any{"command": "cat " + h + "/.netrc"}), "deny", "credential"},
		{"bash claude creds", "Bash", in(map[string]any{"command": "cat ~/.claude/.credentials.json"}), "deny", "credential"},
		{"bash state db", "Bash", in(map[string]any{"command": "sqlite3 " + c.StateDir + "/athena.db .dump"}), "deny", "credential"},

		// blocked commands
		{"blocked simple", "Bash", in(map[string]any{"command": "rm -rf x"}), "deny", "blocked"},
		{"blocked after and", "Bash", in(map[string]any{"command": "ls && rm x"}), "deny", "blocked"},
		{"blocked after semicolon", "Bash", in(map[string]any{"command": "ls; rm x"}), "deny", "blocked"},
		{"blocked absolute path", "Bash", in(map[string]any{"command": "/bin/rm x"}), "deny", "blocked"},
		{"blocked env prefix sudo", "Bash", in(map[string]any{"command": "FOO=1 sudo rm x"}), "deny", "blocked"},
		{"blocked in subshell", "Bash", in(map[string]any{"command": "echo $(rm x)"}), "deny", "blocked"},
		{"blocked via bash -c", "Bash", in(map[string]any{"command": "bash -c 'rm -rf /'"}), "deny", "blocked"},
		{"blocked multiword", "Bash", in(map[string]any{"command": "git push origin main"}), "deny", "blocked"},
		{"not blocked arg", "Bash", in(map[string]any{"command": "echo rm"}), "allow", ""},
		{"not blocked prefix word", "Bash", in(map[string]any{"command": "rmdir x"}), "allow", ""},
		{"not blocked git status", "Bash", in(map[string]any{"command": "git status"}), "allow", ""},

		// writes
		{"write inside new subdir", "Write", in(map[string]any{"file_path": c.TicketDir + "/sub/ok.txt", "content": "x"}), "allow", ""},
		{"write relative inside", "Write", in(map[string]any{"file_path": "sub/ok.txt", "content": "x"}), "allow", ""},
		{"edit inside ticket dir", "Edit", in(map[string]any{"file_path": c.TicketDir + "/a.go"}), "allow", ""},
		{"write dotdot escape", "Write", in(map[string]any{"file_path": "../escape.txt", "content": "x"}), "escalate", "write"},
		{"write etc hosts", "Write", in(map[string]any{"file_path": "/etc/hosts", "content": "x"}), "escalate", "write"},
		{"write via symlink dir", "Write", in(map[string]any{"file_path": c.TicketDir + "/link/x.txt", "content": "x"}), "escalate", "write"},
		{"write symlink itself", "Write", in(map[string]any{"file_path": c.TicketDir + "/link", "content": "x"}), "escalate", "write"},
		{"write sibling prefix", "Write", in(map[string]any{"file_path": c.TicketDir + "2/x", "content": "x"}), "escalate", "write"},
		{"write dotdot sibling", "Write", in(map[string]any{"file_path": c.TicketDir + "/../T2/x", "content": "x"}), "escalate", "write"},
		{"multiedit outside", "MultiEdit", in(map[string]any{"file_path": "/etc/hosts"}), "escalate", "write"},
		{"notebook outside", "NotebookEdit", in(map[string]any{"notebook_path": "/etc/n.ipynb"}), "escalate", "write"},
		{"notebook inside", "NotebookEdit", in(map[string]any{"notebook_path": "n.ipynb"}), "allow", ""},
		{"write missing path", "Write", in(map[string]any{"content": "x"}), "escalate", "write"},

		// bash escalations
		{"env staging", "Bash", in(map[string]any{"command": "tool --env=staging"}), "escalate", "bash"},
		{"env prod space", "Bash", in(map[string]any{"command": "tool --env production"}), "escalate", "bash"},
		{"deploy", "Bash", in(map[string]any{"command": "make deploy"}), "escalate", "bash"},
		{"kubectl", "Bash", in(map[string]any{"command": "kubectl get pods"}), "escalate", "bash"},
		{"terraform apply", "Bash", in(map[string]any{"command": "terraform apply -auto-approve"}), "escalate", "bash"},
		{"terraform plan ok", "Bash", in(map[string]any{"command": "terraform plan"}), "allow", ""},
		{"brew install", "Bash", in(map[string]any{"command": "brew install jq"}), "escalate", "bash"},
		{"apt install", "Bash", in(map[string]any{"command": "apt install jq"}), "escalate", "bash"},
		{"apt-get install", "Bash", in(map[string]any{"command": "apt-get install -y jq"}), "escalate", "bash"},
		{"npm i -g", "Bash", in(map[string]any{"command": "npm i -g typescript"}), "escalate", "bash"},
		{"npm install global flag late", "Bash", in(map[string]any{"command": "npm install typescript -g"}), "escalate", "bash"},
		{"npm install local ok", "Bash", in(map[string]any{"command": "npm install left-pad"}), "allow", ""},
		{"pip install", "Bash", in(map[string]any{"command": "pip install requests"}), "escalate", "bash"},
		{"python -m pip install", "Bash", in(map[string]any{"command": "python3 -m pip install requests"}), "escalate", "bash"},
		{"pip install after activate", "Bash", in(map[string]any{"command": "source .venv/bin/activate && pip install requests"}), "allow", ""},
		{"pip install venv path", "Bash", in(map[string]any{"command": ".venv/bin/pip install requests"}), "allow", ""},
		{"git push force", "Bash", in(map[string]any{"command": "git push --force origin main"}), "escalate", "bash"},
		{"git push -f", "Bash", in(map[string]any{"command": "git push origin main -f"}), "escalate", "bash"},
		{"git push force-with-lease", "Bash", in(map[string]any{"command": "git push --force-with-lease"}), "escalate", "bash"},
		{"git push normal ok", "Bash", in(map[string]any{"command": "git push origin main"}), "allow", ""},
		{"git branch -D", "Bash", in(map[string]any{"command": "git branch -D old"}), "escalate", "bash"},
		{"git branch -d ok", "Bash", in(map[string]any{"command": "git branch -d old"}), "allow", ""},
		{"git push delete", "Bash", in(map[string]any{"command": "git push origin --delete old"}), "escalate", "bash"},
		{"gh pr merge", "Bash", in(map[string]any{"command": "gh pr merge 12 --squash"}), "escalate", "bash"},
		{"plain bash", "Bash", in(map[string]any{"command": "go test ./..."}), "allow", ""},

		// everything else
		{"web fetch", "WebFetch", in(map[string]any{"url": "https://example.com"}), "allow", ""},
		{"mcp tool", "mcp__athena__report", in(map[string]any{"status": "ready"}), "allow", ""},
		{"bad json", "Bash", json.RawMessage(`{not json`), "deny", "input"},
		{"empty input", "Read", nil, "deny", "input"},
	}
	// The git push cases use a different blocked list than the default.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := c
			if tc.name != "blocked multiword" {
				ctx.Blocked = []string{"rm"}
			}
			got := Evaluate(ctx, tc.tool, tc.input)
			if got.Verdict != tc.want {
				t.Fatalf("Evaluate(%s %s) = %+v, want verdict %s", tc.tool, tc.input, got, tc.want)
			}
			if tc.rule != "" && !hasPrefix(got.Rule, tc.rule) {
				t.Fatalf("rule = %q, want prefix %q", got.Rule, tc.rule)
			}
			if got.Verdict != "allow" && got.Reason == "" {
				t.Fatalf("non-allow decision without reason: %+v", got)
			}
		})
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

func TestEvaluateVenvActive(t *testing.T) {
	c := newRuleEnv(t).ctx
	c.VenvActive = true
	got := Evaluate(c, "Bash", in(map[string]any{"command": "pip install requests"}))
	if got.Verdict != "allow" {
		t.Fatalf("pip install with VenvActive = %+v, want allow", got)
	}
}

func TestBlockedInPipeline(t *testing.T) {
	c := newRuleEnv(t).ctx
	c.Blocked = []string{"rm"}
	got := Evaluate(c, "Bash", in(map[string]any{"command": "echo x | rm -rf /"}))
	if got.Verdict != "deny" || got.Rule != "blocked-command" {
		t.Fatalf("pipeline = %+v, want deny by blocked-command", got)
	}
}

func TestCredentialBeatsBlockedAndWrites(t *testing.T) {
	c := newRuleEnv(t).ctx
	// Credential denial is evaluated first, so a write into ~/.ssh is a deny
	// rather than an escalate.
	got := Evaluate(c, "Write", in(map[string]any{"file_path": c.Home + "/.ssh/config", "content": "x"}))
	if got.Verdict != "deny" {
		t.Fatalf("got %+v, want deny", got)
	}
}

// In production the work root defaults to <stateDir>/work, so the ticket dir
// lives inside the state dir. The ticket dir stays usable; everything else in
// the state dir (the database, other tickets) is off limits.
func TestTicketDirUnderStateDir(t *testing.T) {
	e := newRuleEnv(t)
	c := e.ctx
	c.TicketDir = filepath.Join(c.StateDir, "work", "T1")
	if err := os.MkdirAll(c.TicketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	c.Blocked = nil
	cases := []struct {
		name  string
		tool  string
		input json.RawMessage
		want  string
	}{
		{"read in ticket", "Read", in(map[string]any{"file_path": c.TicketDir + "/a.go"}), "allow"},
		{"write in ticket", "Write", in(map[string]any{"file_path": "sub/a.go", "content": "x"}), "allow"},
		{"grep ticket", "Grep", in(map[string]any{"pattern": "x", "path": c.TicketDir}), "allow"},
		{"bash in ticket", "Bash", in(map[string]any{"command": "cd " + c.TicketDir + " && ls"}), "allow"},
		{"bash file in ticket", "Bash", in(map[string]any{"command": "cat " + c.TicketDir + "/a.go"}), "allow"},
		{"read state db", "Read", in(map[string]any{"file_path": c.StateDir + "/athena.db"}), "deny"},
		{"read other ticket", "Read", in(map[string]any{"file_path": c.StateDir + "/work/T2/a.go"}), "deny"},
		{"sibling prefix ticket", "Read", in(map[string]any{"file_path": c.TicketDir + "2/a.go"}), "deny"},
		{"write other ticket", "Write", in(map[string]any{"file_path": c.TicketDir + "/../T2/x", "content": "x"}), "deny"},
		{"grep work root", "Grep", in(map[string]any{"pattern": "x", "path": c.StateDir + "/work"}), "deny"},
		{"bash state db", "Bash", in(map[string]any{"command": "cat " + c.StateDir + "/athena.db"}), "deny"},
		{"bash other ticket", "Bash", in(map[string]any{"command": "cat " + c.TicketDir + "2/a.go"}), "deny"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Evaluate(c, tc.tool, tc.input); got.Verdict != tc.want {
				t.Fatalf("Evaluate(%s %s) = %+v, want %s", tc.tool, tc.input, got, tc.want)
			}
		})
	}
}

func TestStateDirAsHomeDotAthena(t *testing.T) {
	e := newRuleEnv(t)
	c := e.ctx
	c.StateDir = filepath.Join(c.Home, ".athena")
	for _, cmd := range []string{"cat ~/.athena/athena.db", "cat $HOME/.athena/athena.db", "cat ${HOME}/.athena/athena.db"} {
		if got := Evaluate(c, "Bash", in(map[string]any{"command": cmd})); got.Verdict != "deny" {
			t.Errorf("%q = %+v, want deny", cmd, got)
		}
	}
}
