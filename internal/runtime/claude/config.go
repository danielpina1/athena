package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	Env   map[string]string        `json:"env"`
	Hooks map[string][]hookMatcher `json:"hooks"`
}

// WriteSettings writes the Claude Code settings file passed with --settings.
// Every hook event runs `<athenaBin> hook <event>`; the socket reaches the hook
// process through the settings "env". hookTimeout applies to PreToolUse and
// PermissionRequest, which block while a person answers an Approval (zero means
// DefaultDecisionHookTimeout); the other events get 30 seconds.
func WriteSettings(path, athenaBin, socket string, hookTimeout time.Duration) error {
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
		Env:   map[string]string{"ATHENA_SOCKET": socket},
		Hooks: map[string][]hookMatcher{},
	}
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
