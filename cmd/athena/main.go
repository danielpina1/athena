// Command athena is the single binary for the Athena system.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/danielpina1/athena/internal/cli"
	"github.com/danielpina1/athena/internal/node"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// stdin is where `athena hook` reads the hook input from; tests replace it.
var stdin io.Reader = os.Stdin

// run dispatches a subcommand and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: athena <command> [args]")
		return 2
	}
	cmd := args[0]
	switch cmd {
	case "version":
		fmt.Fprintf(stdout, "athena %s\n", version)
		return 0
	case "hook":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: athena hook <event>")
			return 2
		}
		return node.RunHook(args[1], stdin, stdout, stderr, os.Getenv("ATHENA_SOCKET"))
	case "mcp":
		return runMCP(args[1:], stderr)
	case "server", "task", "status", "logs", "reply", "stop", "retry",
		"approvals", "approve", "deny":
		return cli.Run(context.Background(), cmd, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "athena: unknown command %q\n", cmd)
		return 2
	}
}

// runMCP implements `athena mcp --ticket <id>`, the stdio MCP server of one worker session.
func runMCP(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("athena mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ticket := fs.String("ticket", "", "the Ticket whose session this server serves")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *ticket == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: athena mcp --ticket <id>")
		return 2
	}
	if err := runMCPServer(*ticket, os.Getenv("ATHENA_SOCKET")); err != nil {
		fmt.Fprintf(stderr, "athena mcp: %v\n", err)
		return 1
	}
	return 0
}

// runMCPServer serves MCP on stdio; tests replace it.
var runMCPServer = node.RunMCP
