package planner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danielpina1/athena/internal/runtime/claude"
	"github.com/danielpina1/athena/internal/runtime/rt"
)

// Claude is the Completer that runs one `claude -p` turn with no tools, no MCP servers and no hooks: there is
// nothing for a hook to guard.
type Claude struct {
	// Binary is the `claude` binary; Model the model the turn runs on.
	Binary, Model string
	// Dir is the session's working directory: its .athena holds the settings and MCP config.
	Dir string
	// StateDir is the server's state directory: the transcript is <StateDir>/transcripts/planner.jsonl.
	StateDir string

	// onStart, when set, is called with the process id once the session started (tests).
	onStart func(pid int)
}

// Complete runs one turn with system appended to the system prompt and user as its message, and returns the turn's
// result text. An error result is an error. The process has exited when it returns.
func (c *Claude) Complete(ctx context.Context, system, user string) (string, error) {
	adir := filepath.Join(c.Dir, ".athena")
	if err := rt.PrivateDir(adir); err != nil {
		return "", fmt.Errorf("claude: %w", err)
	}
	settings, mcp := filepath.Join(adir, "settings.json"), filepath.Join(adir, "mcp.json")
	for path, body := range map[string]string{settings: `{"hooks":{}}`, mcp: `{"mcpServers":{}}`} {
		if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
			return "", fmt.Errorf("claude: %w", err)
		}
	}
	noTools := ""
	s, err := claude.Start(ctx, claude.Options{
		Binary:             c.Binary,
		Dir:                c.Dir,
		Model:              c.Model,
		SettingsPath:       settings,
		MCPConfigPath:      mcp,
		AppendSystemPrompt: system,
		Env:                []string{"ATHENA_ROLE=planner"},
		TranscriptPath:     filepath.Join(c.StateDir, "transcripts", "planner.jsonl"),
		Tools:              &noTools,
	})
	if err != nil {
		return "", err
	}
	defer func() {
		_ = s.Close()
		_ = s.Wait()
	}()
	if c.onStart != nil {
		c.onStart(s.Pid())
	}
	if err := s.Send(user, ""); err != nil {
		return "", err
	}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case ev, ok := <-s.Events():
			if !ok {
				return "", errors.New("the planner session ended without a result")
			}
			res, isResult := ev.(claude.Result)
			if !isResult {
				continue
			}
			if res.IsError {
				why := strings.TrimSpace(strings.Join(append([]string{res.Text}, res.Errors...), " "))
				if why == "" {
					why = res.Subtype
				}
				return "", fmt.Errorf("the planner's turn failed: %s", why)
			}
			return res.Text, nil
		}
	}
}
