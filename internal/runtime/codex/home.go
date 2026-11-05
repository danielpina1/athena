package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultDecisionHookTimeout bounds the hooks that wait for a person (PreToolUse, PermissionRequest) when
// Home.HookTimeout is zero; the other hooks get quickHookTimeout.
const DefaultDecisionHookTimeout = 2 * time.Hour

const (
	quickHookTimeout = 30 * time.Second
	// configName is the file Codex reads from CODEX_HOME.
	configName = "config.toml"
	// backgroundTerminalMaxTimeout is the 30-minute cap (in ms) on a background terminal after its turn (spec §6.1).
	backgroundTerminalMaxTimeout = 1800000
)

// Home is Athena's own CODEX_HOME: a directory of Athena's state, never the owner's ~/.codex, holding the config.toml
// WriteHome writes and the credential `codex login` writes there (auth.json).
type Home struct {
	// Dir is the CODEX_HOME directory (<stateDir>/codex).
	Dir string
	// AthenaBin is the athena binary the hooks and the MCP server run. Socket is the node's socket; the sessions get
	// it in their environment at start, so config.toml does not hold it.
	AthenaBin, Socket string
	// HookTimeout bounds the hooks that wait for a person's Approval; zero is DefaultDecisionHookTimeout.
	HookTimeout time.Duration
	// Model is the default model; empty leaves it to Codex.
	Model string
}

// hookEvents are the hook events Athena installs, in config.toml order; decision events wait for a person.
var hookEvents = []struct {
	name     string
	decision bool
}{
	{"PreToolUse", true},
	{"PermissionRequest", true},
	{"PostToolUse", false},
	{"Stop", false},
	{"SessionStart", false},
}

// disabledFeatures are the Codex features Athena turns off: nothing that acts outside the worktree or spends quota
// unasked.
var disabledFeatures = []string{
	"apps", "plugins", "remote_plugin", "browser_use", "browser_use_external", "computer_use", "in_app_browser",
	"multi_agent", "image_generation", "memories", "realtime_conversation",
}

// HookSpec is one hook Athena installs: its event (as config.toml names it, "PreToolUse") and its command.
type HookSpec struct {
	Event, Command string
}

// HookSpecs returns the hooks WriteHome installs for h.
func HookSpecs(h Home) []HookSpec {
	specs := make([]HookSpec, 0, len(hookEvents))
	for _, ev := range hookEvents {
		specs = append(specs, HookSpec{Event: ev.name, Command: hookCommand(h.AthenaBin, ev.name)})
	}
	return specs
}

func hookCommand(athenaBin, event string) string {
	return shellQuote(athenaBin) + " hook --runtime codex " + event
}

// renderConfig is config.toml for h: a fixed header and exactly the keys Athena sets (see WriteHome).
func renderConfig(h Home) ([]byte, error) {
	if h.AthenaBin == "" {
		return nil, errors.New("codex: athena binary path is empty")
	}
	for _, v := range []string{h.AthenaBin, h.Model} {
		if !utf8.ValidString(v) {
			return nil, fmt.Errorf("codex: %q is not valid UTF-8, which config.toml cannot hold", v)
		}
	}
	decision := h.HookTimeout
	if decision <= 0 {
		decision = DefaultDecisionHookTimeout
	}
	var b strings.Builder
	b.WriteString("# Athena's Codex configuration. The Athena node writes this file; do not edit it, a change is overwritten.\n")
	b.WriteString("# Per-session values (the MCP server's args and env) are -c overrides when a session starts. Codex records\n")
	b.WriteString("# below, under [hooks.state], that Athena trusts the hooks it wrote here.\n\n")
	if h.Model != "" {
		fmt.Fprintf(&b, "model = %s\n", tomlString(h.Model))
	}
	b.WriteString("approval_policy = \"on-request\"\n")
	b.WriteString("approvals_reviewer = \"user\"\n")
	b.WriteString("sandbox_mode = \"workspace-write\"\n")
	b.WriteString("check_for_update_on_startup = false\n")
	fmt.Fprintf(&b, "background_terminal_max_timeout = %d\n", backgroundTerminalMaxTimeout)
	b.WriteString("\n[analytics]\nenabled = false\n")
	// The process environment is already allowlisted (rt.ChildEnv).
	b.WriteString("\n[shell_environment_policy]\ninherit = \"all\"\n")
	b.WriteString("\n[features]\n")
	for _, f := range disabledFeatures {
		fmt.Fprintf(&b, "%s = false\n", f)
	}
	fmt.Fprintf(&b, "\n[mcp_servers.athena]\ncommand = %s\nargs = [\"mcp\"]\ndefault_tools_approval_mode = \"approve\"\n",
		tomlString(h.AthenaBin))
	for _, ev := range hookEvents {
		d := quickHookTimeout
		if ev.decision {
			d = decision
		}
		secs := int64((d + time.Second - 1) / time.Second)
		// async = false: a hook that decides must finish before Codex goes on.
		fmt.Fprintf(&b, "\n[[hooks.%s]]\n[[hooks.%s.hooks]]\ntype = \"command\"\ncommand = %s\ntimeout = %d\nasync = false\n",
			ev.name, ev.name, tomlString(hookCommand(h.AthenaBin, ev.name)), secs)
	}
	return []byte(b.String()), nil
}

// WriteHome writes h.Dir/config.toml (0600; the directory 0700, auth.json tightened to 0600 when it is there).
// config.toml holds a fixed header and exactly these keys:
//
//   - model (only when h.Model is set), approval_policy "on-request", approvals_reviewer "user", sandbox_mode
//     "workspace-write", check_for_update_on_startup false, background_terminal_max_timeout 1800000;
//   - [analytics] enabled false; [shell_environment_policy] inherit "all";
//   - [features]: apps, plugins, remote_plugin, browser_use, browser_use_external, computer_use, in_app_browser,
//     multi_agent, image_generation, memories and realtime_conversation false;
//   - [mcp_servers.athena]: command h.AthenaBin, args ["mcp"], default_tools_approval_mode "approve";
//   - for PreToolUse, PermissionRequest, PostToolUse, Stop and SessionStart, a [[hooks.<Event>]] matcher group with
//     one [[hooks.<Event>.hooks]] handler: type "command", command `<athenaBin> hook --runtime codex <Event>`, timeout
//     h.HookTimeout in seconds for PreToolUse and PermissionRequest (30 for the others), async false.
//
// When the file already holds exactly that, followed only by the [hooks.state."<key>"] trusted_hash entries Codex
// wrote when TrustHooks trusted Athena's hooks, it is left as it is, so a rewrite does not drop their trust. Anything
// else in the file is replaced.
func WriteHome(h Home) error {
	content, err := renderConfig(h)
	if err != nil {
		return err
	}
	if err := EnsureHomeDir(h.Dir); err != nil {
		return err
	}
	path := filepath.Join(h.Dir, configName)
	if cur, err := os.ReadFile(path); err == nil && strings.HasPrefix(string(cur), string(content)) &&
		onlyTrustState(string(cur[len(content):]), path) {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("codex: %w", err)
		}
		return nil
	}
	return writePrivate(path, content)
}

// onlyTrustState reports whether tail is nothing but [hooks.state."<key>"] tables of keys of the config file at path,
// each holding only trusted_hash: what Codex appends when Athena trusts its hooks (config/batchWrite).
func onlyTrustState(tail, path string) bool {
	prefix := "[hooks.state." + strings.TrimSuffix(tomlString(path+":"), `"`)
	inTable := false
	for _, line := range strings.Split(tail, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, prefix) && strings.HasSuffix(line, `"]`):
			inTable = true
		case inTable && strings.HasPrefix(line, "trusted_hash = \"") && strings.HasSuffix(line, `"`) &&
			!strings.ContainsAny(line[len("trusted_hash = \""):len(line)-1], "\"\\"):
		default:
			return false
		}
	}
	return true
}

// EnsureHomeDir makes dir, Athena's CODEX_HOME, a private directory (0700, created when missing) and tightens the
// credential `codex login` writes there (auth.json) to 0600. It refuses the owner's own ~/.codex.
func EnsureHomeDir(dir string) error {
	if dir == "" {
		return errors.New("codex: CODEX_HOME directory is empty")
	}
	if isUserCodexHome(dir) {
		return fmt.Errorf("codex: %s is the owner's own Codex home; Athena keeps its own under its state directory", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("codex: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("codex: %w", err)
	}
	auth := filepath.Join(dir, "auth.json")
	if fi, err := os.Lstat(auth); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(auth, 0o600); err != nil {
			return fmt.Errorf("codex: %w", err)
		}
	}
	return nil
}

// isUserCodexHome reports whether dir is ~/.codex.
func isUserCodexHome(dir string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	user := filepath.Join(home, ".codex")
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	if abs == user {
		return true
	}
	a, err1 := filepath.EvalSymlinks(abs)
	u, err2 := filepath.EvalSymlinks(user)
	return err1 == nil && err2 == nil && a == u
}

// writePrivate writes path (0600), replacing it in one rename.
func writePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("codex: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("codex: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("codex: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("codex: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("codex: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("codex: write %s: %w", path, err)
	}
	return nil
}

// tomlString is s as a TOML basic string: `"` and `\` escaped, control characters as escapes, everything else (UTF-8)
// as it is.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// shellQuote quotes s for the POSIX shell Codex runs hook commands with. Plain paths are left alone.
func shellQuote(s string) string {
	safe := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@%", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ErrHooksUncontrolled is TrustHooks' refusal: the hooks Codex would run in a directory are not exactly Athena's,
// trusted and enabled. Why says what is wrong.
type ErrHooksUncontrolled struct{ Why string }

func (e ErrHooksUncontrolled) Error() string {
	return "codex hooks are not under Athena's control: " + e.Why
}

// hookInfo is one hook of hooks/list.
type hookInfo struct {
	Key         string `json:"key"`
	EventName   string `json:"eventName"`
	HandlerType string `json:"handlerType"`
	Command     string `json:"command"`
	Enabled     bool   `json:"enabled"`
	Source      string `json:"source"`
	SourcePath  string `json:"sourcePath"`
	CurrentHash string `json:"currentHash"`
	TrustStatus string `json:"trustStatus"`
}

type hooksEntry struct {
	Cwd    string      `json:"cwd"`
	Hooks  []hookInfo  `json:"hooks"`
	Errors []hookError `json:"errors"`
}

type hookError struct {
	Message string `json:"message"`
	Path    string `json:"path"`
}

// listHooks is hooks/list for cwd.
func listHooks(ctx context.Context, c *Conn, cwd string) (hooksEntry, error) {
	var res struct {
		Data []hooksEntry `json:"data"`
	}
	if err := c.Call(ctx, "hooks/list", map[string]any{"cwds": []string{cwd}}, &res); err != nil {
		return hooksEntry{}, fmt.Errorf("codex hooks/list: %w", err)
	}
	if len(res.Data) != 1 {
		return hooksEntry{}, fmt.Errorf("codex hooks/list: %d entries for one directory", len(res.Data))
	}
	return res.Data[0], nil
}

// ours reports whether h is the hook spec w that Athena wrote into its CODEX_HOME's config.toml (the "user" layer):
// a command hook of the same event and command.
func ours(h hookInfo, w HookSpec) bool {
	return h.Source == "user" && h.HandlerType == "command" && h.EventName == lowerFirst(w.Event) && h.Command == w.Command
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// TrustHooks makes Codex trust the hooks Athena wrote, and checks that they are the only hooks it would run in cwd:
//
//  1. hooks/list for cwd;
//  2. for each listed hook that is one of want (Athena's own, from its CODEX_HOME's config.toml) and whose trust is
//     "untrusted" or "modified", one config/batchWrite edit hooks.state."<key>".trusted_hash = <currentHash> (in one
//     call);
//  3. hooks/list again;
//  4. ErrHooksUncontrolled unless every want is there, enabled and trusted (or managed), and no other hook is enabled.
//
// It trusts no hook Athena did not write: a foreign hook is never trusted, and an enabled one fails the check (trusted
// or not), whatever layer (a repository's .codex, a plugin, a managed config) it comes from.
func TrustHooks(ctx context.Context, c *Conn, cwd string, want []HookSpec) error {
	entry, err := listHooks(ctx, c, cwd)
	if err != nil {
		return err
	}
	var edits []map[string]any
	for _, h := range entry.Hooks {
		if h.TrustStatus != "untrusted" && h.TrustStatus != "modified" {
			continue
		}
		for _, w := range want {
			if ours(h, w) {
				edits = append(edits, map[string]any{
					"keyPath": "hooks.state." + tomlString(h.Key) + ".trusted_hash", "value": h.CurrentHash,
					"mergeStrategy": "upsert",
				})
				break
			}
		}
	}
	if len(edits) > 0 {
		if err := c.Call(ctx, "config/batchWrite", map[string]any{"edits": edits}, nil); err != nil {
			return fmt.Errorf("codex config/batchWrite (trusting Athena's hooks): %w", err)
		}
		if entry, err = listHooks(ctx, c, cwd); err != nil {
			return err
		}
	}
	return checkHooks(entry, want)
}

// checkHooks is TrustHooks' verdict on a hooks/list entry.
func checkHooks(entry hooksEntry, want []HookSpec) error {
	for _, w := range want {
		var found *hookInfo
		for i, h := range entry.Hooks {
			if ours(h, w) {
				found = &entry.Hooks[i]
				break
			}
		}
		switch {
		case found == nil:
			why := "Athena's " + w.Event + " hook is not loaded"
			if len(entry.Errors) > 0 {
				why += " (" + entry.Errors[0].Message + ")"
			}
			return ErrHooksUncontrolled{Why: why}
		case !found.Enabled:
			return ErrHooksUncontrolled{Why: "Athena's " + w.Event + " hook is disabled"}
		case found.TrustStatus != "trusted" && found.TrustStatus != "managed":
			return ErrHooksUncontrolled{Why: "Athena's " + w.Event + " hook is " + strconv.Quote(found.TrustStatus) + ", not trusted"}
		}
	}
	for _, h := range entry.Hooks {
		if !h.Enabled {
			continue
		}
		mine := false
		for _, w := range want {
			mine = mine || ours(h, w)
		}
		if !mine {
			return ErrHooksUncontrolled{Why: fmt.Sprintf("a %s hook Athena did not write is enabled (%s, from %s)", h.EventName, h.Source, h.SourcePath)}
		}
	}
	return nil
}
