// Command sessionctl acts as one worker session from outside it, for the dashboard's end-to-end tests (web/e2e).
// It is test-only. It reads the environment fakeclaude logged for the session (ATHENA_FAKE_ENV_LOG) and drives the
// real paths a Claude Code session takes, so nothing is written to the store but by the node:
//
//	sessionctl approval -env FILE -athena BIN -tool Bash -input '{"command":"make deploy"}'
//	    runs `athena hook PreToolUse` with the tool call, as Claude Code runs the hook. When the rules escalate the
//	    call the node raises an Approval and the hook waits for the owner's answer; sessionctl prints the hook's
//	    output (the verdict) and exits 0 once it has one.
//	sessionctl report -env FILE -athena BIN -status waiting -summary TEXT [-question TEXT]
//	    calls the report tool of `athena mcp --ticket <id>` over MCP stdio, the server Claude Code talks to. It prints
//	    the tool's answer and exits 1 when the tool refused the report.
//	sessionctl review -env FILE -athena BIN -verdict changes -findings TEXT
//	    calls the review tool of `athena mcp --ticket <id> --role review`, a review session's server, with the verdict
//	    and findings. It prints the tool's answer and exits 1 when the tool refused them.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: sessionctl approval|report|review [flags]")
		return 2
	}
	fs := flag.NewFlagSet("sessionctl "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	envFile := fs.String("env", "", "the session's environment, as fakeclaude logged it")
	athena := fs.String("athena", "athena", "the athena binary")
	tool := fs.String("tool", "Bash", "approval: the tool name")
	input := fs.String("input", "{}", "approval: the tool input, JSON")
	status := fs.String("status", "", "report: the report's state (progress, waiting, ready, blocked, failed, dropped)")
	summary := fs.String("summary", "", "report: the summary")
	question := fs.String("question", "", "report: a question for the owner")
	verdict := fs.String("verdict", "", "review: the verdict (approved, changes)")
	findings := fs.String("findings", "", "review: the findings, one per line")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	env, err := loadEnv(*envFile)
	if err != nil {
		fmt.Fprintf(stderr, "sessionctl: %v\n", err)
		return 1
	}
	switch args[0] {
	case "approval":
		err = approval(env, *athena, *tool, *input, stdout)
	case "report":
		args := map[string]any{"status": *status, "summary": *summary}
		if *question != "" {
			args["question"] = *question
		}
		err = callTool(env, *athena, false, "report", args, stdout)
	case "review":
		err = callTool(env, *athena, true, "review", map[string]any{"verdict": *verdict, "findings": *findings}, stdout)
	default:
		fmt.Fprintf(stderr, "sessionctl: unknown command %q\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "sessionctl %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

// loadEnv reads an environment log: one NAME=value per line, blank lines skipped.
func loadEnv(path string) ([]string, error) {
	if path == "" {
		return nil, errors.New("-env is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var env []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.Contains(l, "=") {
			env = append(env, l)
		}
	}
	return env, nil
}

// lookup returns name's value in env, or "".
func lookup(env []string, name string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v
		}
	}
	return ""
}

// hookInput is the PreToolUse hook input Claude Code writes for a call of tool with input (JSON).
func hookInput(env []string, tool, input string) ([]byte, error) {
	if !json.Valid([]byte(input)) {
		return nil, fmt.Errorf("tool input is not JSON: %q", input)
	}
	return json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       tool,
		"tool_input":      json.RawMessage(input),
		"cwd":             lookup(env, "ATHENA_DIR"),
	})
}

func approval(env []string, athena, tool, input string, stdout io.Writer) error {
	raw, err := hookInput(env, tool, input)
	if err != nil {
		return err
	}
	cmd := exec.Command(athena, "hook", "PreToolUse")
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(raw)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("athena hook: %v: %s", err, strings.TrimSpace(errOut.String()))
	}
	_, err = stdout.Write(out.Bytes())
	return err
}

// callTool calls tool of the session's MCP server (`athena mcp --ticket <id>`, with --role review for a review
// session's) with args, and prints its answer; a refusal is the error.
func callTool(env []string, athena string, review bool, tool string, args map[string]any, stdout io.Writer) error {
	ticket := lookup(env, "ATHENA_TICKET")
	if ticket == "" {
		return errors.New("the environment names no ATHENA_TICKET")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	argv := []string{"mcp", "--ticket", ticket}
	if review {
		argv = append(argv, "--role", "review")
	}
	cmd := exec.Command(athena, argv...)
	cmd.Env = env
	client := mcp.NewClient(&mcp.Implementation{Name: "sessionctl", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("start athena mcp: %w", err)
	}
	defer session.Close()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return fmt.Errorf("call %s: %w", tool, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			text.WriteString(t.Text)
		}
	}
	if res.IsError {
		return errors.New(text.String())
	}
	_, err = fmt.Fprintln(stdout, text.String())
	return err
}
