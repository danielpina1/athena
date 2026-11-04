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
	case "git-credential":
		return node.RunGitCredential(args[1:], stdin, stdout, stderr, os.Environ())
	case "gh":
		return node.RunGh(args[1:], stdin, stdout, stderr, os.Environ())
	case "server", "task", "status", "logs", "reply", "stop", "retry",
		"approvals", "approve", "deny", "metrics",
		"github", "policy", "triggers", "threads", "decisions", "reevaluate", "dismiss", "unsubscribe":
		cli.Version = version
		return cli.Run(context.Background(), cmd, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "athena: unknown command %q\n", cmd)
		return 2
	}
}

// runMCP implements `athena mcp --ticket <id>`, the stdio MCP server of one worker session, and
// `athena mcp --role router`, the router session's.
func runMCP(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("athena mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ticket := fs.String("ticket", "", "the Ticket whose session this server serves")
	role := fs.String("role", "", "router: serve the router session's tools")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var err error
	switch {
	case fs.NArg() > 0, *role != "" && (*role != node.RoleRouter || *ticket != ""):
		fmt.Fprintln(stderr, "usage: athena mcp --ticket <id> | athena mcp --role router")
		return 2
	case *role == node.RoleRouter:
		err = runRouterMCPServer(os.Getenv("ATHENA_SOCKET"), os.Getenv("ATHENA_ROUTER_TOKEN"))
	case *ticket == "":
		fmt.Fprintln(stderr, "usage: athena mcp --ticket <id> | athena mcp --role router")
		return 2
	default:
		err = runMCPServer(*ticket, os.Getenv("ATHENA_SOCKET"))
	}
	if err != nil {
		fmt.Fprintf(stderr, "athena mcp: %v\n", err)
		return 1
	}
	return 0
}

// runMCPServer and runRouterMCPServer serve MCP on stdio; tests replace them.
var (
	runMCPServer       = node.RunMCP
	runRouterMCPServer = node.RunRouterMCP
)
