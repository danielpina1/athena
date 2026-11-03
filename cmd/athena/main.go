// Command athena is the single binary for the Athena system.
package main

import (
	"fmt"
	"io"
	"os"

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
	case "server", "task", "status", "logs", "reply", "stop", "retry",
		"approvals", "approve", "deny", "mcp":
		fmt.Fprintf(stderr, "athena %s: not implemented\n", cmd)
		return 1
	default:
		fmt.Fprintf(stderr, "athena: unknown command %q\n", cmd)
		return 2
	}
}
