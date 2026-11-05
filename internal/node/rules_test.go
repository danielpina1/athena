package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielpina1/athena/internal/core"
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
		{"write ~user is not inside the ticket", "Write", in(map[string]any{"file_path": "~root/f", "content": "x"}), "escalate", "write-outside"},
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

		// blocked commands behind shell keywords
		{"blocked after time -p", "Bash", in(map[string]any{"command": "time -p rm x"}), "deny", "blocked"},
		{"blocked after time in loop", "Bash", in(map[string]any{"command": "while true; do time -p rm x; done"}), "deny", "blocked"},
		{"blocked in for loop", "Bash", in(map[string]any{"command": "for f in *.tmp; do rm $f; done"}), "deny", "blocked"},
		{"blocked after then", "Bash", in(map[string]any{"command": "if true; then rm x; fi"}), "deny", "blocked"},
		{"blocked in braces", "Bash", in(map[string]any{"command": "{ rm x; }"}), "deny", "blocked"},
		{"blocked after bang", "Bash", in(map[string]any{"command": "! rm x"}), "deny", "blocked"},
		{"blocked in while", "Bash", in(map[string]any{"command": "while true; do sudo -n rm x; done"}), "deny", "blocked"},
		{"blocked after else", "Bash", in(map[string]any{"command": "if a; then b; else rm x; fi"}), "deny", "blocked"},
		{"keyword alone ok", "Bash", in(map[string]any{"command": "for f in a b; do echo $f; done"}), "allow", ""},

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

func TestCredentialCaseInsensitive(t *testing.T) {
	if !foldCase {
		t.Skip("filesystem is case-sensitive on this platform")
	}
	c := newRuleEnv(t).ctx
	h := c.Home
	for _, tc := range []struct {
		tool  string
		input json.RawMessage
	}{
		{"Read", in(map[string]any{"file_path": h + "/.SSH/id_rsa"})},
		{"Read", in(map[string]any{"file_path": h + "/.Ssh/id_rsa"})},
		{"Read", in(map[string]any{"file_path": "~/.AWS/credentials"})},
		{"Read", in(map[string]any{"file_path": h + "/.NETRC"})},
		{"Read", in(map[string]any{"file_path": h + "/.Claude/.Credentials.json"})},
		{"Read", in(map[string]any{"file_path": h + "/.CLAUDE.JSON"})},
		{"Read", in(map[string]any{"file_path": strings.ToUpper(c.StateDir) + "/athena.db"})},
		{"Grep", in(map[string]any{"pattern": "x", "path": strings.ToUpper(h)})},
		{"Read", in(map[string]any{"file_path": h + "/.\u017fsh/id_rsa"})},
		{"Read", in(map[string]any{"file_path": h + "/.\u017fSH/id_rsa"})},
		{"Read", in(map[string]any{"file_path": h + "/.claude/.credential\u017f.json"})},
		{"Read", in(map[string]any{"file_path": filepath.Dir(c.StateDir) + "/\u017ftate" + "/athena.db"})},
		{"Bash", in(map[string]any{"command": "cat ~/.\u017fsh/id_rsa"})},
		{"Bash", in(map[string]any{"command": "cat ~/.SSH/id_rsa"})},
		{"Bash", in(map[string]any{"command": "cat $HOME/.Aws/credentials"})},
		{"Bash", in(map[string]any{"command": "cat " + strings.ToUpper(c.StateDir) + "/athena.db"})},
	} {
		if got := Evaluate(c, tc.tool, tc.input); got.Verdict != "deny" {
			t.Errorf("%s %s = %+v, want deny", tc.tool, tc.input, got)
		}
	}
}

// ".." after a symlink is applied by the kernel to the link's target, not to
// the directory holding the link, so lexical cleaning would be fooled.
func TestDotDotAfterSymlink(t *testing.T) {
	e := newRuleEnv(t)
	c := e.ctx
	sub := filepath.Join(e.outDir, "sub")
	docs := filepath.Join(c.Home, "Documents")
	for _, d := range []string{sub, docs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(sub, filepath.Join(c.TicketDir, "deep")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(docs, filepath.Join(c.TicketDir, "docs")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		tool  string
		input json.RawMessage
		want  string
	}{
		{"write deep/../escape", "Write", in(map[string]any{"file_path": c.TicketDir + "/deep/../escape.txt", "content": "x"}), "escalate"},
		{"write relative deep/../escape", "Write", in(map[string]any{"file_path": "deep/../escape.txt", "content": "x"}), "escalate"},
		{"edit deep/../../escape", "Edit", in(map[string]any{"file_path": c.TicketDir + "/deep/../../escape.txt"}), "escalate"},
		{"read docs/../.ssh", "Read", in(map[string]any{"file_path": c.TicketDir + "/docs/../.ssh/id_rsa"}), "deny"},
		{"grep docs/../.ssh", "Grep", in(map[string]any{"pattern": "x", "path": c.TicketDir + "/docs/../.ssh"}), "deny"},
		{"read docs/.. ancestor", "Grep", in(map[string]any{"pattern": "x", "path": c.TicketDir + "/docs/.."}), "deny"},
		{"plain dotdot inside ok", "Write", in(map[string]any{"file_path": c.TicketDir + "/a/../b.txt", "content": "x"}), "allow"},
		{"read plain dotdot inside ok", "Read", in(map[string]any{"file_path": c.TicketDir + "/a/../b.txt"}), "allow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Evaluate(c, tc.tool, tc.input); got.Verdict != tc.want {
				t.Fatalf("Evaluate(%s %s) = %+v, want %s", tc.tool, tc.input, got, tc.want)
			}
		})
	}
}

// Athena-generated session config must not be writable by the worker.
func TestSessionConfigWrites(t *testing.T) {
	e := newRuleEnv(t)
	c := e.ctx
	if err := os.MkdirAll(filepath.Join(c.TicketDir, ".athena"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(c.TicketDir, ".athena"), filepath.Join(c.TicketDir, "alias")); err != nil {
		t.Fatal(err)
	}
	nested := c
	nested.TicketDir = filepath.Join(c.StateDir, "work", "T1")
	if err := os.MkdirAll(nested.TicketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		ctx   RuleContext
		tool  string
		path  string
		want  string
		rule  string
		onlyF bool // needs a case-insensitive filesystem
	}{
		{"athena settings", c, "Write", ".athena/settings.json", "deny", "session-config", false},
		{"athena abs", c, "Write", c.TicketDir + "/.athena/mcp.json", "deny", "session-config", false},
		{"athena nested file", c, "Edit", ".athena/transcripts/t.jsonl", "deny", "session-config", false},
		{"athena via dotdot", c, "Write", "sub/../.athena/x", "deny", "session-config", false},
		{"athena via symlink", c, "Write", "alias/x", "deny", "session-config", false},
		{"athena upper", c, "Write", ".ATHENA/x", "deny", "session-config", true},
		{"athena inside state dir", nested, "Write", ".athena/x", "deny", "session-config", false},
		{"claude settings", c, "Write", ".claude/settings.json", "escalate", "session-config", false},
		{"claude settings local", c, "Edit", ".claude/settings.local.json", "escalate", "session-config", false},
		{"claude settings multiedit", c, "MultiEdit", c.TicketDir + "/.claude/settings.json", "escalate", "session-config", false},
		{"claude settings via dotdot", c, "Write", "a/../.claude/settings.json", "escalate", "session-config", false},
		{"claude settings upper", c, "Write", ".Claude/Settings.JSON", "escalate", "session-config", true},
		{"claude settings long s", c, "Write", ".claude/\u017fettings.json", "escalate", "session-config", true},
		{"claude settings inside state dir", nested, "Write", ".claude/settings.json", "escalate", "session-config", false},
		{"claude other file ok", c, "Write", ".claude/commands/x.md", "allow", "", false},
		{"claude settings in subdir ok", c, "Write", "sub/.claude/settings.json", "allow", "", false},
		{"athena-like name ok", c, "Write", ".athena-notes.md", "allow", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.onlyF && !foldCase {
				t.Skip("case-sensitive filesystem")
			}
			got := Evaluate(tc.ctx, tc.tool, in(map[string]any{"file_path": tc.path, "content": "x"}))
			if got.Verdict != tc.want || (tc.rule != "" && !hasPrefix(got.Rule, tc.rule)) {
				t.Fatalf("Evaluate(%s %s) = %+v, want %s (%s)", tc.tool, tc.path, got, tc.want, tc.rule)
			}
		})
	}
}

// TestBashWritesOutsideTicketDir: after the owner denied a Write to /tmp/x, the real CLI's worker (haiku) wrote the
// file with `echo -n x > /tmp/x` and the hook allowed it. Bash writes that name a target outside the ticket
// directory escalate like the Write tools do.
func TestBashWritesOutsideTicketDir(t *testing.T) {
	e := newRuleEnv(t)
	c := e.ctx
	c.Blocked = nil
	out := e.outDir
	cases := []struct {
		name, cmd, want, rule string
	}{
		{"the live bypass", "echo -n x > /tmp/athena-live-outside.txt && cat /tmp/athena-live-outside.txt", "escalate", ruleWrite},
		{"append", "echo x >> " + out + "/f", "escalate", ruleWrite},
		{"no space", "echo x >" + out + "/f", "escalate", ruleWrite},
		{"clobber", "echo x >| " + out + "/f", "escalate", ruleWrite},
		{"stderr to file", "make 2> " + out + "/err", "escalate", ruleWrite},
		{"both to file", "make &> " + out + "/all", "escalate", ruleWrite},
		{"quoted target", `echo x > "` + out + `/f"`, "escalate", ruleWrite},
		{"relative dotdot", "echo x > ../out/f", "escalate", ruleWrite},
		{"through symlink", "echo x > link/f", "escalate", ruleWrite},
		{"tilde", "echo x > ~/f", "escalate", ruleWrite},
		{"unresolvable variable", "echo x > $TARGET", "escalate", ruleWrite},
		{"tee", "echo x | tee " + out + "/f", "escalate", ruleWrite},
		{"tee append flag", "echo x | tee -a " + out + "/f", "escalate", ruleWrite},
		{"touch", "touch " + out + "/f", "escalate", ruleWrite},
		{"mkdir", "mkdir -p " + out + "/d", "escalate", ruleWrite},
		{"cp", "cp main.go " + out + "/main.go", "escalate", ruleWrite},
		{"mv", "mv main.go " + out + "/", "escalate", ruleWrite},
		{"dd", "dd if=/dev/zero of=" + out + "/f count=1", "escalate", ruleWrite},
		{"inside sh -c", `sh -c "echo x > ` + out + `/f"`, "escalate", ruleWrite},
		{"after sudo", "sudo tee " + out + "/f", "escalate", ruleWrite},
		{"session config", "echo {} > .athena/settings.json", "deny", ruleSessionConfig},
		{"session config behind an outside target", "echo x > /tmp/a; echo {} > .athena/settings.json", "deny", ruleSessionConfig},
		{"session config after an outside cp", "cp main.go " + out + "/m && touch .athena/mcp.json", "deny", ruleSessionConfig},
		{"claude settings", "echo {} > .claude/settings.json", "escalate", ruleSessionConfig},

		// cd and pushd change what a relative target means
		{"cd outside then relative", "cd " + out + " && echo x > f", "escalate", ruleWrite},
		{"cd tmp then relative", "cd /tmp && echo x > athena-live-outside.txt", "escalate", ruleWrite},
		{"pushd outside then touch", "pushd " + out + "; touch f", "escalate", ruleWrite},
		{"cd dotdot out", "cd .. && echo x > f", "escalate", ruleWrite},
		{"cd no argument is home", "cd && echo x > f", "escalate", ruleWrite},
		{"cd dash is unknown", "cd - && echo x > f", "escalate", ruleWrite},
		{"cd variable is unknown", "cd $DIR && echo x > f", "escalate", ruleWrite},
		{"popd is unknown", "popd; echo x > f", "escalate", ruleWrite},
		{"cd outside in sh -c", `sh -c "cd ` + out + ` && echo x > f"`, "escalate", ruleWrite},
		{"cd outside then absolute inside", "cd " + out + " && echo x > " + c.TicketDir + "/f", "allow", ""},
		{"cd sub then relative", "cd sub && echo x > f", "allow", ""},
		{"cd sub then dotdot", "cd sub && cd .. && echo x > f", "allow", ""},
		{"pushd sub", "pushd sub; touch f", "allow", ""},
		{"cd ticket dir", "cd " + c.TicketDir + " && echo x > f", "allow", ""},
		{"cd ATHENA_DIR", "cd $ATHENA_DIR && echo x > f", "allow", ""},

		// the variables Athena sets
		{"ATHENA_DIR", "echo hi > $ATHENA_DIR/hello.txt", "allow", ""},
		{"ATHENA_DIR braces", "echo hi > ${ATHENA_DIR}/x", "allow", ""},
		{"ATHENA_DIR quoted", `echo hi > "$ATHENA_DIR/x"`, "allow", ""},
		{"PWD", "echo hi > $PWD/x", "allow", ""},
		{"PWD braces", "echo hi > ${PWD}/x", "allow", ""},
		{"PWD after cd outside", "cd " + out + " && echo hi > $PWD/x", "escalate", ruleWrite},
		{"PWD after cd sub", "cd sub && echo hi > $PWD/x", "allow", ""},
		{"PWD after unknown cd", "cd - && echo hi > $PWD/x", "escalate", ruleWrite},
		{"HOME", "echo hi > $HOME/x", "escalate", ruleWrite},
		{"HOME braces", "echo hi > ${HOME}/x", "escalate", ruleWrite},
		{"ATHENA_DIR session config", "echo {} > $ATHENA_DIR/.athena/settings.json", "deny", ruleSessionConfig},
		{"ATHENA_DIR prefix of another name", "echo hi > $ATHENA_DIRX/x", "escalate", ruleWrite},
		{"other variable", "echo hi > $TMPDIR/x", "escalate", ruleWrite},
		{"reassigned ATHENA_DIR", "ATHENA_DIR=/tmp; echo x > $ATHENA_DIR/f", "escalate", ruleWrite},
		{"exported ATHENA_DIR", "export ATHENA_DIR=/tmp && echo x > $ATHENA_DIR/f", "escalate", ruleWrite},
		{"ATHENA_DIR for one command", "ATHENA_DIR=/tmp sh -c 'echo x > $ATHENA_DIR/f'", "escalate", ruleWrite},
		{"env ATHENA_DIR", "env ATHENA_DIR=/tmp sh -c 'echo x > $ATHENA_DIR/f'", "escalate", ruleWrite},
		{"reassigned PWD", "PWD=/tmp; echo x > $PWD/f", "escalate", ruleWrite},
		{"reassigned HOME", "HOME=/tmp; echo x > $HOME/f", "escalate", ruleWrite},
		{"appended ATHENA_DIR", "ATHENA_DIR+=/../../tmp; echo x > $ATHENA_DIR/f", "escalate", ruleWrite},
		{"declare ATHENA_DIR", "declare ATHENA_DIR=/tmp; echo x > ${ATHENA_DIR}/f", "escalate", ruleWrite},
		{"read ATHENA_DIR", "read ATHENA_DIR < f; echo x > $ATHENA_DIR/f", "escalate", ruleWrite},
		{"for ATHENA_DIR", "for ATHENA_DIR in /tmp; do echo x > $ATHENA_DIR/f; done", "escalate", ruleWrite},
		{"unset HOME", "unset HOME; echo x > $HOME/f", "escalate", ruleWrite},
		{"assigned PWD taints cd target", "PWD=/tmp; cd $PWD && echo x > f", "escalate", ruleWrite},
		{"other variable assignment keeps ATHENA_DIR", "NAME=a; echo x > $ATHENA_DIR/$NAME", "escalate", ruleWrite},
		{"unrelated assignment keeps ATHENA_DIR", "N=1; echo x > $ATHENA_DIR/f", "allow", ""},

		// ~ forms other than ~ and ~/ cannot be resolved
		{"cd ~user", "cd ~root && echo x > f", "escalate", ruleWrite},
		{"target ~user", "echo x > ~root/f", "escalate", ruleWrite},
		{"cd ~+", "cd ~+ && echo x > f", "escalate", ruleWrite},
		{"cd ~-", "cd ~- && echo x > f", "escalate", ruleWrite},
		{"tee ~user", "echo x | tee ~root/f", "escalate", ruleWrite},
		{"ATHENA_DIR with another variable", "echo hi > $ATHENA_DIR/$NAME", "escalate", ruleWrite},

		{"inside redirect", "echo x > notes.txt", "allow", ""},
		{"inside absolute", "echo x > " + c.TicketDir + "/sub/notes.txt", "allow", ""},
		{"inside dotdot", "echo x > sub/../notes.txt", "allow", ""},
		{"append inside", "echo x >> notes.txt", "allow", ""},
		{"devnull", "make > /dev/null 2> /dev/null", "allow", ""},
		{"fd dup", "make 2>&1 | tail -5", "allow", ""},
		{"fd dup to stderr", "echo oops >&2", "allow", ""},
		{"stdout device", "echo x > /dev/stdout", "allow", ""},
		{"here-doc", "cat <<EOF\nhi\nEOF", "allow", ""},
		{"here-doc to inside file", "cat > f.txt <<EOF\nhi\nEOF", "allow", ""},
		{"input redirect", "sort < " + out + "/f", "allow", ""},
		{"comparison in awk", "awk '$1 > 5' data.txt", "allow", ""},
		{"cp inside", "cp main.go copy.go", "allow", ""},
		{"touch inside", "touch a b", "allow", ""},
		{"tee inside", "echo x | tee log.txt", "allow", ""},
		{"read only", "ls " + out + " && cat " + out + "/f", "allow", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(c, "Bash", in(map[string]any{"command": tc.cmd}))
			if got.Verdict != tc.want || (tc.rule != "" && got.Rule != tc.rule) {
				t.Fatalf("Evaluate(%q) = %+v, want %s %s", tc.cmd, got, tc.want, tc.rule)
			}
		})
	}
}

func TestRouterRulesDenyWorkTools(t *testing.T) {
	for _, tool := range []string{"Bash", "Write", "Read", "WebFetch", "Task", "mcp__other__x", "mcp__athenax__route", ""} {
		d := RouterRules(tool)
		if d.Verdict != VerdictDeny || d.Rule != ruleRouterTools || !strings.Contains(d.Reason, "mcp__athena__") {
			t.Errorf("RouterRules(%q) = %+v, want a deny naming the router's tools", tool, d)
		}
	}
	for _, tool := range []string{"mcp__athena__route", "mcp__athena__eval_decide"} {
		if d := RouterRules(tool); d.Verdict != VerdictAllow {
			t.Errorf("RouterRules(%q) = %+v, want allow", tool, d)
		}
	}
}

// Review Focus 3: commands that would show or replace a credential are denied in every session, whatever the
// configured blocked list, and so is a command that names a token variable.
func TestBlockedCredentialCommands(t *testing.T) {
	e := newRuleEnv(t)
	c := e.ctx
	c.Blocked = nil
	c.AthenaBin = "/opt/athena/bin/athena-dev"
	shim := filepath.Join(c.StateDir, ShimsDir, "gh")
	for _, cmd := range []string{
		"gh auth status",
		"gh auth token",
		"gh --hostname github.com auth token",
		"cd x && gh auth login --with-token < t",
		"/usr/local/bin/gh auth status",
		"git credential fill",
		"git -C . credential fill",
		"printf 'host=github.com\\n' | git -c credential.useHttpPath=true credential fill",
		"git credential-osxkeychain get",
		"git-credential-osxkeychain get",
		"athena git-credential get",
		"/opt/athena/bin/athena-dev git-credential get",
		"athena-dev gh auth status",
		"athena gh auth token",
		"sh -c 'gh auth token'",
		shim + " auth status",
		"echo $GH_TOKEN",
		"GH_TOKEN=x gh pr list",
		"env | grep GH_ENTERPRISE_TOKEN",
		"GIT_ASKPASS=/tmp/a git push",
		"GIT_TRACE_REDACT=0 GIT_TRACE_CURL=1 git fetch",
		"cat $ATHENA_STATE/athena.db",
	} {
		if d := Evaluate(c, "Bash", in(map[string]any{"command": cmd})); d.Verdict != VerdictDeny {
			t.Errorf("%q: %+v, want deny", cmd, d)
		}
	}
	for _, cmd := range []string{
		"gh pr create --fill",
		"gh pr view 7 --json title",
		"gh issue comment 3 --body 'see the auth module'",
		"git push -u origin athena/t-0001",
		"git commit -m 'credential handling'",
		"git log --grep auth",
		"athena-dev gh pr list",
		"echo $PATH",
	} {
		if d := Evaluate(c, "Bash", in(map[string]any{"command": cmd})); d.Verdict == VerdictDeny {
			t.Errorf("%q: %+v, want it not denied", cmd, d)
		}
	}
	// The state directory, where the shims and the clones live, stays unreadable.
	for _, cmd := range []string{"cat " + shim, "ls " + filepath.Join(c.StateDir, "repos", "o", "r"), "cd " + c.StateDir + " && ls"} {
		if d := Evaluate(c, "Bash", in(map[string]any{"command": cmd})); d.Verdict != VerdictDeny || d.Rule != ruleCredential {
			t.Errorf("%q: %+v, want the state-dir deny", cmd, d)
		}
	}
	if d := Evaluate(c, "Bash", in(map[string]any{"command": "echo $GH_TOKEN"})); d.Rule != ruleCredentialEnv {
		t.Errorf("token variable deny = %+v, want rule %s", d, ruleCredentialEnv)
	}
	if d := Evaluate(c, "Bash", in(map[string]any{"command": "gh auth status"})); !strings.Contains(d.Reason, "gh auth") || d.Rule != ruleBlocked {
		t.Errorf("deny = %+v, want the blocked-command rule naming gh auth", d)
	}
}

// codexEval normalises a Codex tool call and evaluates every call it maps to, as the node does.
func codexEval(c RuleContext, tool string, input json.RawMessage) Decision {
	d, _ := EvaluateCalls(c, core.RuntimeCodex, NormalizeTool(core.RuntimeCodex, tool, input))
	return d
}

func TestCodexShellMapsToBash(t *testing.T) {
	// An argv array is joined with POSIX shell quoting, so an argument with spaces stays one word.
	argv := []any{"bash", "-lc", "echo 'a b' > out.txt"}
	want := `bash -lc 'echo '"'"'a b'"'"' > out.txt'`
	for _, tool := range []string{"Bash", "shell", "local_shell", "exec_command", "unified_exec"} {
		for _, key := range []string{"command", "cmd"} {
			calls := NormalizeTool(core.RuntimeCodex, tool, in(map[string]any{key: argv}))
			if len(calls) != 1 || calls[0].Tool != "Bash" {
				t.Fatalf("%s{%s}: calls = %+v, want one Bash call", tool, key, calls)
			}
			var got struct{ Command string }
			if err := json.Unmarshal(calls[0].Input, &got); err != nil || got.Command != want {
				t.Fatalf("%s{%s}: command = %q (%v), want %q", tool, key, got.Command, err, want)
			}
		}
	}
	// A plain string command is kept as it is.
	calls := NormalizeTool(core.RuntimeCodex, "shell", in(map[string]any{"command": "ls -la"}))
	if len(calls) != 1 || calls[0].Tool != "Bash" || !strings.Contains(string(calls[0].Input), `"ls -la"`) {
		t.Fatalf("string command: %+v", calls)
	}
	// A working directory other than the ticket's is where the command runs: its relative writes are judged from there.
	e := newRuleEnv(t)
	if d := codexEval(e.ctx, "exec_command", in(map[string]any{"cmd": []any{"touch", "f"}, "workdir": e.outDir})); d.Verdict != VerdictEscalate || d.Rule != ruleWrite {
		t.Fatalf("relative write from an outside workdir: %+v, want escalate", d)
	}
	if d := codexEval(e.ctx, "exec_command", in(map[string]any{"cmd": []any{"touch", "f"}, "workdir": e.ctx.TicketDir})); d.Verdict != VerdictAllow {
		t.Fatalf("relative write in the ticket dir: %+v, want allow", d)
	}
	// The mapped command goes through every Bash rule: a credential read in an argument is denied.
	if d := codexEval(e.ctx, "exec_command", in(map[string]any{"cmd": []any{"cat", "/x/.ssh/id_rsa"}})); d.Verdict != VerdictDeny || d.Rule != ruleCredential {
		t.Fatalf("credential read: %+v, want deny", d)
	}
	if d := codexEval(e.ctx, "shell", in(map[string]any{"command": []any{"rm", "-rf", "x y"}})); d.Verdict != VerdictDeny || d.Rule != ruleBlocked {
		t.Fatalf("blocked command: %+v, want deny", d)
	}
	// MCP tools and web_search are evaluated unchanged; another tool's strings are searched for credential paths.
	if d := codexEval(e.ctx, "mcp__athena__report", in(map[string]any{"status": "ready", "summary": "done"})); d.Verdict != VerdictAllow {
		t.Fatalf("mcp tool: %+v, want allow", d)
	}
	if d := codexEval(e.ctx, "view_image", in(map[string]any{"path": "~/.ssh/id_rsa"})); d.Verdict != VerdictDeny || d.Rule != ruleCredential {
		t.Fatalf("other tool naming a credential: %+v, want deny", d)
	}
	if d := codexEval(e.ctx, "view_image", in(map[string]any{"path": "shot.png"})); d.Verdict != VerdictAllow {
		t.Fatalf("other tool: %+v, want allow", d)
	}
	// Claude's input is one call, unchanged.
	raw := in(map[string]any{"command": "ls"})
	if calls := NormalizeTool(core.RuntimeClaude, "Bash", raw); len(calls) != 1 || calls[0].Tool != "Bash" || string(calls[0].Input) != string(raw) {
		t.Fatalf("claude: %+v", calls)
	}
	if calls := NormalizeTool(core.RuntimeClaude, "exec_command", raw); len(calls) != 1 || calls[0].Tool != "exec_command" {
		t.Fatalf("claude keeps its own tool names: %+v", calls)
	}
}

func TestApplyPatchEvaluatesEveryPath(t *testing.T) {
	e := newRuleEnv(t)
	patch := func(move string) string {
		return "*** Begin Patch\n*** Add File: a.txt\n+x\n*** Update File: sub/b.txt\n" + move + "@@\n-x\n+y\n*** Delete File: d.txt\n*** End Patch\n"
	}
	calls := NormalizeTool(core.RuntimeCodex, "apply_patch", in(map[string]any{"command": patch("*** Move to: ../out/c.txt\n")}))
	var paths []string
	for _, c := range calls {
		var w struct {
			FilePath string `json:"file_path"`
		}
		if c.Tool != "Write" || json.Unmarshal(c.Input, &w) != nil {
			t.Fatalf("call %+v, want a Write", c)
		}
		paths = append(paths, w.FilePath)
	}
	if strings.Join(paths, ",") != "a.txt,sub/b.txt,../out/c.txt,d.txt" {
		t.Fatalf("paths = %v", paths)
	}
	// One path outside the ticket directory escalates the whole call, and names that path.
	d, call := EvaluateCalls(e.ctx, core.RuntimeCodex, calls)
	if d.Verdict != VerdictEscalate || d.Rule != ruleWrite || !strings.Contains(string(call.Input), "../out/c.txt") {
		t.Fatalf("decision %+v on %s, want escalate on the moved path", d, call.Input)
	}
	if d := codexEval(e.ctx, "apply_patch", in(map[string]any{"command": patch("")})); d.Verdict != VerdictAllow {
		t.Fatalf("patch inside the ticket dir: %+v, want allow", d)
	}
	// The patch may be the whole input (a freeform tool), or under another key.
	whole, _ := json.Marshal(patch("*** Move to: ../out/c.txt\n"))
	if d := codexEval(e.ctx, "apply_patch", whole); d.Verdict != VerdictEscalate {
		t.Fatalf("patch as the input: %+v, want escalate", d)
	}
	if d := codexEval(e.ctx, "apply_patch", in(map[string]any{"input": patch("*** Move to: ../out/c.txt\n")})); d.Verdict != VerdictEscalate {
		t.Fatalf("patch under input: %+v, want escalate", d)
	}
	// A credential path anywhere in the patch is denied, whatever else it does; deny beats escalate.
	if d := codexEval(e.ctx, "apply_patch", in(map[string]any{"command": "*** Begin Patch\n*** Update File: ../out/x\n*** Add File: " +
		e.ctx.Home + "/.ssh/config\n+x\n*** End Patch\n"})); d.Verdict != VerdictDeny {
		t.Fatalf("patch to a credential file: %+v, want deny", d)
	}
	// A patch whose paths cannot be read asks the owner.
	if d := codexEval(e.ctx, "apply_patch", in(map[string]any{"command": "not a patch"})); d.Verdict != VerdictEscalate {
		t.Fatalf("unreadable patch: %+v, want escalate", d)
	}
}

// What write_stdin types into a running shell or REPL is held to the Bash text rules: credential and state paths, token
// variables, blocked commands and AI attributions deny; anything else is allowed.
func TestWriteStdinUsesBashTextRules(t *testing.T) {
	e := newRuleEnv(t)
	stdin := func(chars string) Decision {
		return codexEval(e.ctx, "write_stdin", in(map[string]any{"session_id": 3, "chars": chars}))
	}
	for chars, rule := range map[string]string{
		"cat ~/.ssh/id_rsa\n": ruleCredential,
		"echo $GH_TOKEN\n":    ruleCredentialEnv,
		"rm -rf build\n":      ruleBlocked,
		"gh auth token\n":     ruleBlocked,
		"git commit -m 'x' -m 'Co-Authored-By: Claude <noreply@anthropic.com>'\n": ruleAttribution,
	} {
		if d := stdin(chars); d.Verdict != VerdictDeny || d.Rule != rule {
			t.Errorf("write_stdin %q: %+v, want deny by %s", chars, d, rule)
		}
	}
	for _, chars := range []string{"print(1 + 1)\n", "go test ./...\n", "\u0003", ""} {
		if d := stdin(chars); d.Verdict != VerdictAllow {
			t.Errorf("write_stdin %q: %+v, want allow", chars, d)
		}
	}
}

// Codex's apply_patch fails closed: indented headers are read, a shell command that carries a patch (heredoc or argv)
// writes its paths, and a line naming a header the rules cannot read escalates.
func TestApplyPatchFailsClosed(t *testing.T) {
	e := newRuleEnv(t)
	c := e.ctx
	if err := os.MkdirAll(filepath.Join(c.TicketDir, ".git", "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	c.Protected = ProtectedPaths(c.TicketDir)
	const hidden = "*** Begin Patch\n*** Add File: ok.txt\n+x\n  *** Add File: .git/hooks/pre-commit\n+#!/bin/sh\n*** End Patch\n"
	// An indented header is a header (Codex's parser trims marker lines).
	if d := codexEval(c, "apply_patch", in(map[string]any{"command": hidden})); d.Verdict != VerdictDeny || d.Rule != ruleProtectedPath {
		t.Errorf("indented header: %+v, want deny", d)
	}
	// Through a shell tool: a heredoc, the argv form, and a relative path under the command's workdir.
	for name, input := range map[string]map[string]any{
		"heredoc":  {"cmd": "apply_patch <<'EOF'\n" + hidden + "EOF\n"},
		"bash -lc": {"cmd": []any{"bash", "-lc", "apply_patch <<'EOF'\n" + hidden + "EOF"}},
		"argv":     {"command": []any{"apply_patch", hidden}},
	} {
		if d := codexEval(c, "exec_command", in(input)); d.Verdict != VerdictDeny || d.Rule != ruleProtectedPath {
			t.Errorf("%s: %+v, want deny", name, d)
		}
	}
	out := "*** Begin Patch\n*** Add File: notes.txt\n+x\n*** End Patch\n"
	if d := codexEval(c, "shell", in(map[string]any{"command": []any{"apply_patch", out}, "workdir": e.outDir})); d.Verdict != VerdictEscalate || d.Rule != ruleWrite {
		t.Errorf("shell patch under an outside workdir: %+v, want escalate", d)
	}
	if d := codexEval(c, "shell", in(map[string]any{"command": []any{"apply_patch", out}, "workdir": c.TicketDir})); d.Verdict != VerdictAllow {
		t.Errorf("shell patch in the ticket dir: %+v, want allow", d)
	}
	// A header the rules cannot read escalates, beside the paths they can.
	for name, patch := range map[string]string{
		"no path":      "*** Begin Patch\n*** Add File: ok.txt\n+x\n*** Update File:\n*** End Patch\n",
		"text before":  "*** Begin Patch\n*** Add File: ok.txt\n+x\nx *** Delete File: .git/config\n*** End Patch\n",
		"other casing": "*** Begin Patch\n*** Add File: ok.txt\n+x\n*** add file: y\n*** End Patch\n",
	} {
		if d := codexEval(c, "apply_patch", in(map[string]any{"command": patch})); d.Verdict != VerdictEscalate {
			t.Errorf("%s: %+v, want escalate", name, d)
		}
	}
	if d := codexEval(c, "exec_command", in(map[string]any{"cmd": "grep -rn '*** Add File' docs"})); d.Verdict != VerdictEscalate {
		t.Errorf("a command naming a patch header: %+v, want escalate", d)
	}
}

// Under Codex a Claude Code tool name is evaluated as Claude's: an Edit outside the ticket directory escalates, one of
// a protected path is denied; an unknown tool's strings are still searched for credential paths.
func TestCodexClaudeToolNamesUseEvaluate(t *testing.T) {
	e := newRuleEnv(t)
	c := e.ctx
	if err := os.MkdirAll(filepath.Join(c.TicketDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	c.Protected = ProtectedPaths(c.TicketDir)
	for _, tool := range []string{"Edit", "MultiEdit", "Write", "NotebookEdit"} {
		if d := codexEval(c, tool, in(map[string]any{"file_path": "/etc/passwd", "old_string": "a", "new_string": "b"})); d.Verdict != VerdictEscalate || d.Rule != ruleWrite {
			t.Errorf("%s /etc/passwd: %+v, want escalate", tool, d)
		}
		if d := codexEval(c, tool, in(map[string]any{"file_path": ".git/config"})); d.Verdict != VerdictDeny || d.Rule != ruleProtectedPath {
			t.Errorf("%s .git/config: %+v, want deny", tool, d)
		}
	}
	if d := codexEval(c, "Read", in(map[string]any{"file_path": c.Home + "/.aws/credentials"})); d.Verdict != VerdictDeny || d.Rule != ruleCredential {
		t.Errorf("Read a credential: %+v, want deny", d)
	}
	if d := codexEval(c, "view_image", in(map[string]any{"path": "~/.ssh/id_rsa"})); d.Verdict != VerdictDeny {
		t.Errorf("unknown tool naming a credential: %+v, want deny", d)
	}
}

func TestApplyPatchToGitConfigDenied(t *testing.T) {
	e := newRuleEnv(t)
	c := e.ctx
	// A repository whose git directory is in the ticket directory.
	for _, d := range []string{".git/hooks", ".athena/gh"} {
		if err := os.MkdirAll(filepath.Join(c.TicketDir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c.Protected = ProtectedPaths(c.TicketDir)
	up := func(p string) json.RawMessage {
		return in(map[string]any{"command": "*** Begin Patch\n*** Update File: " + p + "\n@@\n-a\n+b\n*** End Patch\n"})
	}
	for _, p := range []string{".git/config", ".git/config.worktree", ".git/hooks/pre-commit", "sub/../.git/hooks/post-checkout", ".athena/settings.json", ".athena/mcp.json"} {
		if d := codexEval(c, "apply_patch", up(p)); d.Verdict != VerdictDeny {
			t.Errorf("patch to %s: %+v, want deny", p, d)
		}
	}
	if d := codexEval(c, "apply_patch", up(".git/config")); d.Rule != ruleProtectedPath {
		t.Errorf("patch to .git/config: rule %q, want %q", d.Rule, ruleProtectedPath)
	}
	// The same paths through the shell.
	if d := codexEval(c, "exec_command", in(map[string]any{"cmd": []any{"bash", "-lc", "echo x > .git/hooks/pre-commit"}})); d.Verdict != VerdictDeny || d.Rule != ruleProtectedPath {
		t.Errorf("shell write to a git hook: %+v, want deny", d)
	}
	if d := codexEval(c, "apply_patch", up("src/main.go")); d.Verdict != VerdictAllow {
		t.Errorf("patch to a source file: %+v, want allow", d)
	}

	// A worktree: its config and hooks are in the main repository's git directory, outside the ticket directory, where a
	// write would otherwise only need an approval.
	main := filepath.Join(e.root, "main")
	gitDir := filepath.Join(main, ".git", "worktrees", "T1")
	for _, d := range []string{gitDir, filepath.Join(main, ".git", "hooks")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	wt := filepath.Join(e.root, "work", "W1")
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := c
	w.TicketDir, w.Protected = wt, ProtectedPaths(wt)
	for _, p := range []string{filepath.Join(main, ".git", "config"), filepath.Join(main, ".git", "hooks", "pre-push"), filepath.Join(gitDir, "config.worktree"),
		// The files that say where the git directory is.
		filepath.Join(gitDir, "commondir"), filepath.Join(gitDir, "gitdir"), filepath.Join(wt, ".git")} {
		if d := codexEval(w, "apply_patch", up(p)); d.Verdict != VerdictDeny || d.Rule != ruleProtectedPath {
			t.Errorf("worktree patch to %s: %+v, want deny", p, d)
		}
	}
	// The sandbox denies exactly these writes (one list).
	n := &Node{cfg: Config{Sandbox: &SandboxConfig{}}}
	sb, _ := n.sandboxFor(wt)
	if strings.Join(sb.DenyWrite, ",") != strings.Join(ProtectedPaths(wt), ",") {
		t.Errorf("sandbox DenyWrite = %v, want ProtectedPaths %v", sb.DenyWrite, ProtectedPaths(wt))
	}
	// Claude's rules are unchanged: its sandbox protects these.
	cl := c
	cl.Protected = nil
	if d := Evaluate(cl, "Write", in(map[string]any{"file_path": ".git/config", "content": "x"})); d.Verdict != VerdictAllow {
		t.Errorf("claude Write to .git/config: %+v, want allow (its sandbox guards it)", d)
	}
}

func TestAttributionTrailerDenied(t *testing.T) {
	e := newRuleEnv(t)
	c := e.ctx
	c.Blocked = nil
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(c.TicketDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("msg.txt", "Fix the parser\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n")
	write("body.md", "Adds the thing.\n\nCo-authored-by: Codex <noreply@openai.com>\n")
	for _, cmd := range []string{
		`git commit -m "Fix the parser" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`,
		`git commit -am 'Fix

Co-authored-by: codex <codex@example.com>'`,
		`git commit --message="fix" --message "Generated with Codex"`,
		`git commit -mfix -m 'noreply@anthropic.com'`,
		`git -C . commit -F msg.txt`,
		`git commit --file=msg.txt`,
		"cd sub && git commit -F - <<'EOF'\nFix\n\nCo-Authored-By: Claude <noreply@anthropic.com>\nEOF",
		`git add -A && git commit -m "fix" -m "Co-Authored-By: Claude"`,
		`sh -c "git commit -m 'Generated with [Claude Code]'"`,
		`gh pr create --title "Fix" --body "Done.

🤖 Generated with [Claude Code](https://claude.com/claude-code)"`,
		`gh pr create -t Fix -b 'noreply@openai.com'`,
		`gh pr edit 7 --body-file body.md`,
		`gh pr create --title="Co-Authored-By: Codex" --body x`,
		// Abbreviated long options, stdin by its device name, wrappers with operands, a file the same command writes.
		`git commit --mess "fix" --mess="Co-Authored-By: Claude"`,
		`git commit --tr "Co-authored-by: Codex <x@example.com>" -m fix`,
		`git commit --fil msg.txt`,
		"git commit -F /dev/stdin <<'EOF'\nFix\n\nCo-Authored-By: Claude <noreply@anthropic.com>\nEOF",
		`timeout 60 git commit -m "fix" -m "Co-Authored-By: Claude"`,
		`nice -n 5 git commit -m "Generated with [Claude Code]"`,
		`sudo -u dev env -u X git commit -m "noreply@openai.com"`,
		"cat > new-msg.txt <<'EOF'\nFix\n\nCo-Authored-By: Codex <noreply@openai.com>\nEOF\ngit commit -F new-msg.txt",
	} {
		d := Evaluate(c, "Bash", in(map[string]any{"command": cmd}))
		if d.Verdict != VerdictDeny || d.Rule != ruleAttribution || d.Reason != "remove the AI attribution trailer or footer: commits and pull requests carry none" {
			t.Errorf("%q: %+v, want the attribution deny", cmd, d)
		}
	}
	// Codex's argv form, joined, is judged the same.
	if d := codexEval(c, "exec_command", in(map[string]any{"cmd": []any{"git", "commit", "-m", "fix\n\nCo-Authored-By: Codex <noreply@openai.com>"}})); d.Verdict != VerdictDeny || d.Rule != ruleAttribution {
		t.Errorf("codex argv commit: %+v, want the attribution deny", d)
	}
	// The blocked commands come first.
	c.Blocked = []string{"git commit"}
	if d := Evaluate(c, "Bash", in(map[string]any{"command": `git commit -m "Co-Authored-By: Claude"`})); d.Rule != ruleBlocked {
		t.Errorf("blocked commit: %+v, want the blocked-command deny", d)
	}
}

// A message file is read only inside the ticket directory (a symlink out of it is outside), only its first 64 KiB, and
// relative paths name it from where the command runs: the workdir, a cd, or git -C.
func TestAttributionMessageFiles(t *testing.T) {
	e := newRuleEnv(t)
	c := e.ctx
	c.Blocked = nil
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bash := func(cmd string) Decision { return Evaluate(c, "Bash", in(map[string]any{"command": cmd})) }
	// A symlink in the ticket directory to a file outside it is not read: its content is not the rule's to see.
	outside := filepath.Join(e.outDir, "msg.txt")
	write(outside, "Fix\n\nCo-Authored-By: Claude\n")
	if err := os.Symlink(outside, filepath.Join(c.TicketDir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if body, ok := c.messageFile("link.txt", c.TicketDir); ok || body != "" {
		t.Fatalf("a symlink out of the ticket dir was read: %q", body)
	}
	if d := bash(`git commit -F link.txt`); d.Verdict == VerdictDeny {
		t.Errorf("symlinked message: %+v, want it not denied (the file is not read, the command carries nothing)", d)
	}
	// Only the first 64 KiB are read.
	pad := strings.Repeat("x", maxMessageFile)
	write(filepath.Join(c.TicketDir, "long.txt"), pad+"\nCo-Authored-By: Claude\n")
	write(filepath.Join(c.TicketDir, "short.txt"), pad[:maxMessageFile-100]+"\nCo-Authored-By: Claude\n")
	if d := bash(`git commit -F long.txt`); d.Verdict == VerdictDeny {
		t.Errorf("attribution past the cap: %+v, want it not seen", d)
	}
	if d := bash(`git commit -F short.txt`); d.Rule != ruleAttribution {
		t.Errorf("attribution within the cap: %+v, want the attribution deny", d)
	}
	// Relative paths after cd, git -C or from the workdir.
	write(filepath.Join(c.TicketDir, "sub", "m.txt"), "Fix\n\nGenerated with [Claude Code]\n")
	write(filepath.Join(c.TicketDir, "m.txt"), "Fix\n")
	for _, cmd := range []string{`cd sub && git commit -F m.txt`, `git -C sub commit -F m.txt`, `cd sub; git -C . commit --file=m.txt`} {
		if d := bash(cmd); d.Rule != ruleAttribution {
			t.Errorf("%q: %+v, want the attribution deny", cmd, d)
		}
	}
	if d := Evaluate(c, "Bash", in(map[string]any{"command": `git commit -F m.txt`, "workdir": filepath.Join(c.TicketDir, "sub")})); d.Rule != ruleAttribution {
		t.Errorf("relative to the workdir: %+v, want the attribution deny", d)
	}
	if d := bash(`git commit -F m.txt`); d.Verdict == VerdictDeny {
		t.Errorf("the ticket dir's own m.txt: %+v, want it not denied", d)
	}
}

func TestAttributionAllowsPlainMessage(t *testing.T) {
	e := newRuleEnv(t)
	c := e.ctx
	c.Blocked = nil
	if err := os.WriteFile(filepath.Join(c.TicketDir, "msg.txt"), []byte("Fix the Claude config parser\n\nCo-authored-by: Jane <jane@example.com>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(e.outDir, "msg.txt")
	if err := os.WriteFile(outside, []byte("Co-Authored-By: Claude\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		`git commit -m "Read the Claude Code settings" -m "Co-authored-by: Jane <jane@example.com>"`,
		`git commit -m "Codex runtime: generated code is checked"`,
		`git commit -F msg.txt`,
		`git commit -F ` + outside, // outside the ticket directory: not read
		`git log --grep "Co-Authored-By: Claude"`,
		`echo "Co-Authored-By: Claude" > notes.md`,
		`gh pr create --title "Codex runtime" --body "Adds the Codex runtime for Claude Code users"`,
		`gh pr view 7`,
		`gh issue create --body "Co-Authored-By: Claude"`,
	} {
		if d := Evaluate(c, "Bash", in(map[string]any{"command": cmd})); d.Verdict == VerdictDeny {
			t.Errorf("%q: %+v, want it not denied", cmd, d)
		}
	}
}
