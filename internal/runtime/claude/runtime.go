package claude

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/runtime/rt"
)

// VerifiedVersion is the Claude Code version Athena's use of it was last checked against; Check reports a newer one
// as not Tested.
const VerifiedVersion = "2.1.280"

// Runtime is Claude Code as an rt.Runtime: the node's worker sessions. (The router drives Claude Code through Start
// and its own events.)
type Runtime struct {
	// Binary is the claude executable (default "claude").
	Binary string
}

var _ rt.Runtime = Runtime{}

// runtimeName is Claude Code's rt.Runtime name.
const runtimeName = "claude"

// Name is "claude".
func (Runtime) Name() string { return runtimeName }

func (r Runtime) binary() string {
	if r.Binary == "" {
		return "claude"
	}
	return r.Binary
}

// checkTimeout bounds `claude --version`.
const checkTimeout = 10 * time.Second

// Check runs `claude --version`. Claude Code is ready once it runs: its hooks come from the settings Prepare writes,
// and its login is its own.
func (r Runtime) Check(ctx context.Context) rt.Info {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.binary(), "--version").Output()
	if err != nil {
		return rt.Info{Why: "claude --version: " + err.Error()}
	}
	// "2.1.280 (Claude Code)"
	v, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	info := rt.Info{Installed: true, Ready: true, Version: v}
	if newer, ok := newerVersion(v, VerifiedVersion); ok && !newer {
		info.Tested = true
	} else if !ok {
		info.Why = "unrecognised Claude Code version " + strconv.Quote(v)
	}
	return info
}

// newerVersion reports whether dotted version a is newer than b; ok is false when either is not one.
func newerVersion(a, b string) (newer, ok bool) {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		var err error
		if i < len(pa) {
			if x, err = strconv.Atoi(pa[i]); err != nil {
				return false, false
			}
		}
		if i < len(pb) {
			if y, err = strconv.Atoi(pb[i]); err != nil {
				return false, false
			}
		}
		if x != y {
			return x > y, true
		}
	}
	return false, true
}

// Prepare writes the session's .athena/settings.json (Athena's hooks, the sandbox block, the settings env) and
// .athena/mcp.json (Athena's MCP server for the Ticket and Assignment; for a ReadOnly session, a review's, with only
// its review tool). A read-only session's sandbox is the caller's to make read-only (p.Sandbox).
func (Runtime) Prepare(p rt.Prepare) error {
	adir := filepath.Join(p.WorkDir, ".athena")
	if err := WriteSettingsWith(filepath.Join(adir, "settings.json"), p.AthenaBin, p.Socket, p.HookTimeout, p.Sandbox, p.SandboxEnv...); err != nil {
		return err
	}
	if p.ReadOnly {
		return WriteReviewMCPConfig(filepath.Join(adir, "mcp.json"), p.AthenaBin, p.Socket, p.TicketID, p.AssignmentID)
	}
	return WriteMCPConfig(filepath.Join(adir, "mcp.json"), p.AthenaBin, p.Socket, p.TicketID, p.AssignmentID)
}

// readOnlyTools are the built-in tools a read-only session goes without: those that write files.
const readOnlyTools = "Write,Edit,MultiEdit,NotebookEdit"

// Start launches a worker session in o.Dir with the files Prepare wrote there. The CLI echoes each message it takes
// in (--replay-user-messages), so the transcript keeps what the worker was sent. Its approvals go through Athena's
// hooks and its sandbox is in its settings: o.Approve, o.Sandbox and o.WritableRoots are not used. A read-only session
// (a review) goes without the built-in tools that write files (--disallowedTools), and starts only when Prepare wrote
// its files read-only (its MCP server a review's), never writable; its hooks deny every write (the node's rules).
func (r Runtime) Start(ctx context.Context, o rt.StartOptions) (rt.Session, error) {
	adir := filepath.Join(o.Dir, ".athena")
	var disallowed string
	if o.ReadOnly {
		if !isReviewMCP(filepath.Join(adir, "mcp.json")) {
			return nil, errors.New("claude: a read-only session needs files prepared read-only")
		}
		disallowed = readOnlyTools
	}
	if o.Transcript == nil {
		return nil, errors.New("claude: StartOptions.Transcript is required")
	}
	s, err := start(ctx, Options{
		Binary:             r.binary(),
		Dir:                o.Dir,
		Model:              o.Model,
		ResumeID:           o.ResumeID,
		SettingsPath:       filepath.Join(adir, "settings.json"),
		MCPConfigPath:      filepath.Join(adir, "mcp.json"),
		AppendSystemPrompt: o.SystemPrompt,
		Env:                o.Env,
		CloseGrace:         o.CloseGrace,
		Transcript:         o.Transcript,
		ReplayUserMessages: true,
		DisallowedTools:    disallowed,
	}, true)
	if err != nil {
		return nil, err
	}
	return runtimeSession{s}, nil
}

// runtimeSession is a Session whose events are translated for the node.
type runtimeSession struct{ *Session }

func (s runtimeSession) Events() <-chan rt.Event { return s.rtEvents }

// toRT translates one of Claude Code's events into the node's.
func toRT(ev Event) rt.Event {
	switch e := ev.(type) {
	case Init:
		return rt.Init{SessionID: e.SessionID, Model: e.Model}
	case AssistantText:
		return rt.AssistantText{Text: e.Text}
	case ToolUse:
		return rt.ToolUse{ID: e.ID, Name: e.Name, Input: e.Input}
	case ToolResult:
		return rt.ToolResult{ID: e.ToolUseID, IsError: e.IsError}
	case UserEcho:
		return rt.UserEcho{Text: e.Text, UUID: e.UUID}
	case Result:
		return rt.TurnEnd{
			SessionID:       e.SessionID,
			OK:              !e.IsError && (e.Subtype == "" || e.Subtype == "success"),
			Subtype:         e.Subtype,
			Text:            e.Text,
			Errors:          e.Errors,
			CostUSD:         e.CostUSD,
			InputTokens:     int64(e.InputTokens),
			OutputTokens:    int64(e.OutputTokens),
			CacheReadTokens: int64(e.CacheReadTokens),
			NumTurns:        e.NumTurns,
			Duration:        time.Duration(e.DurationMS) * time.Millisecond,
		}
	case Unknown:
		if e.Type == "rate_limit_event" {
			if q, ok := parseRateLimit(e.Raw); ok {
				return q
			}
		}
		return rt.Unknown{Kind: e.Type}
	}
	return rt.Unknown{}
}
