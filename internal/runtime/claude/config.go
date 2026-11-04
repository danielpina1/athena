package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// DefaultDecisionHookTimeout is how long PreToolUse and PermissionRequest
	// hooks may block. They wait for a person to answer an Approval.
	DefaultDecisionHookTimeout = 2 * time.Hour
	// quickHookTimeout covers the hooks that must never hold a session up.
	quickHookTimeout = 30 * time.Second
)

// hookEvents lists the hook events Athena registers and whether each one can
// block on a person.
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

type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"` // seconds
}

type hookMatcher struct {
	Hooks []hookCommand `json:"hooks"`
}

type settingsFile struct {
	Env     map[string]string        `json:"env"`
	Hooks   map[string][]hookMatcher `json:"hooks"`
	Sandbox *sandboxSettings         `json:"sandbox,omitempty"`
	// Attribution is always empty: a session's commits and pull requests carry no Claude Code trailer, footer or
	// session link. IncludeCoAuthoredBy false is the older, deprecated form of the same, for a Claude Code that predates
	// attribution; a newer one reads attribution and ignores it (checked against Claude Code 2.1.280).
	Attribution         attributionSettings `json:"attribution"`
	IncludeCoAuthoredBy bool                `json:"includeCoAuthoredBy"`
}

// attributionSettings is Claude Code's "attribution" block: an empty commit or pr hides that attribution, and
// sessionUrl false leaves out the claude.ai session link.
type attributionSettings struct {
	Commit     string `json:"commit"`
	PR         string `json:"pr"`
	SessionURL bool   `json:"sessionUrl"`
}

// Sandbox turns on Claude Code's sandbox for a session's Bash commands (and every process they start): writes only to
// the session's working directory, the sandbox's own temporary directory and AllowWrite; outbound connections only to
// AllowedDomains (through the sandbox's proxy) and the Unix sockets UnixSockets. It does not cover Claude Code's own
// tools (Read, Edit, Write, WebFetch), hooks or MCP servers, which run outside it.
//
// The block always sets autoAllowBashIfSandboxed false, so a sandboxed command still goes through the hooks and the
// permission flow as before, allowUnsandboxedCommands false, so a session cannot ask to run a command outside the
// sandbox (dangerouslyDisableSandbox is ignored), and strictAllowlist, so a host not on the list is refused, never
// asked about.
type Sandbox struct {
	// Required makes the session fail at startup when the sandbox cannot start (failIfUnavailable); otherwise Claude
	// Code warns on stderr and runs commands unsandboxed.
	Required bool
	// AllowWrite are paths writable besides the working directory; DenyWrite are paths never writable, even inside
	// an allowed one; DenyRead are paths that cannot be read.
	AllowWrite, DenyWrite, DenyRead []string
	// AllowedDomains are the hosts commands may reach ("*.example.com" matches the subdomains).
	AllowedDomains []string
	// UnixSockets are the Unix sockets commands may connect to (macOS only: Linux cannot filter sockets by path).
	UnixSockets []string
	// AllowAllUnixSockets lets commands connect to any Unix socket; on Linux it is the only way to reach one.
	AllowAllUnixSockets bool
	// WeakerNetworkIsolation (macOS) lets commands reach the trustd service, which Go programs such as gh need to
	// verify a certificate signed by a CA from the keychain (a TLS-inspecting proxy). It opens a path out of the
	// sandbox through that service.
	WeakerNetworkIsolation bool
}

type sandboxSettings struct {
	Enabled                      bool              `json:"enabled"`
	FailIfUnavailable            bool              `json:"failIfUnavailable"`
	AutoAllowBashIfSandboxed     bool              `json:"autoAllowBashIfSandboxed"`
	AllowUnsandboxedCommands     bool              `json:"allowUnsandboxedCommands"`
	EnableWeakerNetworkIsolation bool              `json:"enableWeakerNetworkIsolation,omitempty"`
	Filesystem                   sandboxFilesystem `json:"filesystem"`
	Network                      sandboxNetwork    `json:"network"`
}

type sandboxFilesystem struct {
	AllowWrite []string `json:"allowWrite"`
	DenyWrite  []string `json:"denyWrite"`
	DenyRead   []string `json:"denyRead"`
}

type sandboxNetwork struct {
	AllowedDomains      []string `json:"allowedDomains"`
	StrictAllowlist     bool     `json:"strictAllowlist"`
	AllowUnixSockets    []string `json:"allowUnixSockets"`
	AllowAllUnixSockets bool     `json:"allowAllUnixSockets,omitempty"`
}

// settings is the "sandbox" block of sb, nil without one.
func (sb *Sandbox) settings() *sandboxSettings {
	if sb == nil {
		return nil
	}
	list := func(v []string) []string {
		if v == nil {
			return []string{}
		}
		return slices.Clone(v)
	}
	return &sandboxSettings{
		Enabled:                      true,
		FailIfUnavailable:            sb.Required,
		EnableWeakerNetworkIsolation: sb.WeakerNetworkIsolation,
		Filesystem:                   sandboxFilesystem{AllowWrite: list(sb.AllowWrite), DenyWrite: list(sb.DenyWrite), DenyRead: list(sb.DenyRead)},
		Network: sandboxNetwork{AllowedDomains: list(sb.AllowedDomains), StrictAllowlist: true,
			AllowUnixSockets: list(sb.UnixSockets), AllowAllUnixSockets: sb.AllowAllUnixSockets},
	}
}

// WriteSettings writes the Claude Code settings file passed with --settings, without a sandbox block (WriteSettingsWith).
func WriteSettings(path, athenaBin, socket string, hookTimeout time.Duration) error {
	return WriteSettingsWith(path, athenaBin, socket, hookTimeout, nil)
}

// WriteSettingsWith writes the Claude Code settings file passed with --settings.
// Every hook event runs `<athenaBin> hook <event>`; the socket reaches the hook
// process through the settings "env". hookTimeout applies to PreToolUse and
// PermissionRequest, which block while a person answers an Approval (zero means
// DefaultDecisionHookTimeout); the other events get 30 seconds. A non-nil sb adds
// the "sandbox" block; env is added to the settings "env" (Claude Code passes it
// to the commands it runs).
func WriteSettingsWith(path, athenaBin, socket string, hookTimeout time.Duration, sb *Sandbox, env ...string) error {
	if athenaBin == "" {
		return errors.New("claude: athena binary path is empty")
	}
	if socket == "" {
		return errors.New("claude: socket path is empty")
	}
	if hookTimeout <= 0 {
		hookTimeout = DefaultDecisionHookTimeout
	}
	s := settingsFile{
		Env:     map[string]string{},
		Hooks:   map[string][]hookMatcher{},
		Sandbox: sb.settings(),
	}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return fmt.Errorf("claude: settings env %q is not NAME=value", kv)
		}
		s.Env[k] = v
	}
	s.Env["ATHENA_SOCKET"] = socket
	for _, ev := range hookEvents {
		d := quickHookTimeout
		if ev.decision {
			d = hookTimeout
		}
		secs := int((d + time.Second - 1) / time.Second)
		s.Hooks[ev.name] = []hookMatcher{{Hooks: []hookCommand{{
			Type:    "command",
			Command: shellQuote(athenaBin) + " hook " + ev.name,
			Timeout: secs,
		}}}}
	}
	return writeJSON(path, s)
}

type mcpServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// WriteMCPConfig writes the file passed with --mcp-config: one stdio server
// named "athena" running `<athenaBin> mcp --ticket <ticketID>`, with the
// socket, ticket and assignment in its env (not left to env inheritance).
func WriteMCPConfig(path, athenaBin, socket, ticketID, assignmentID string) error {
	if athenaBin == "" || socket == "" || ticketID == "" || assignmentID == "" {
		return errors.New("claude: athena binary, socket, ticket and assignment are all required")
	}
	cfg := struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}{map[string]mcpServer{"athena": {
		Type:    "stdio",
		Command: athenaBin,
		Args:    []string{"mcp", "--ticket", ticketID},
		Env:     map[string]string{"ATHENA_SOCKET": socket, "ATHENA_TICKET": ticketID, "ATHENA_ASSIGNMENT": assignmentID},
	}}}
	return writeJSON(path, cfg)
}

// WriteRouterMCPConfig writes the router's --mcp-config: one stdio server named "athena" running
// `<athenaBin> mcp --role router`, with the socket and the router start's token in its env. The router's ops carry
// that token instead of any Ticket or Assignment.
func WriteRouterMCPConfig(path, athenaBin, socket, token string) error {
	if athenaBin == "" || socket == "" || token == "" {
		return errors.New("claude: athena binary, socket and router token are all required")
	}
	cfg := struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}{map[string]mcpServer{"athena": {
		Type:    "stdio",
		Command: athenaBin,
		Args:    []string{"mcp", "--role", "router"},
		Env:     map[string]string{"ATHENA_SOCKET": socket, "ATHENA_ROUTER_TOKEN": token},
	}}}
	return writeJSON(path, cfg)
}

// writeJSON writes v as indented JSON with mode 0600, atomically.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// shellQuote quotes s for the POSIX shell Claude Code runs hook commands with.
// Plain paths are left alone.
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
