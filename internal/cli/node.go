package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/danielpina1/athena/internal/fleet"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/nodelink"
	"github.com/danielpina1/athena/internal/server"
)

// --- node ---

// stdin is where a command reads the owner's answers (revoke's question); tests replace it.
var stdin io.Reader = os.Stdin

// nodeUsage is each node subcommand's usage line. The server's commands take --state-dir (the server's state, default
// ~/.athena); join and run take --state (the node's, default ~/.athena-node).
var nodeUsage = map[string]string{
	"token":   "athena node token [--ttl D] [--state-dir S]",
	"list":    "athena node list [--state-dir S]",
	"drain":   "athena node drain <name|id> [--state-dir S]",
	"undrain": "athena node undrain <name|id> [--state-dir S]",
	"revoke":  "athena node revoke <name|id> [--yes] [--state-dir S]",
	"join":    "athena node join <dashboard-url> <token> [--name N] [--ca-sha256 FP] [--state DIR]",
	"run":     "athena node run [--state DIR] [--slots N] [--model M] [--blocked a,b] [--approval-timeout D] [--claude PATH] [--codex PATH]",
}

// defaultNodeStateDir is a node's state directory: ~/.athena-node, apart from a server's ~/.athena on the same machine.
func defaultNodeStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".athena-node"
	}
	return filepath.Join(home, ".athena-node")
}

// node runs `athena node ...`: token, list, drain, undrain and revoke on the server's machine, through its control
// socket; join and run on the machine that becomes a node.
func (c *command) node(ctx context.Context, args []string) int {
	c.fs.Usage = func() {
		fmt.Fprintln(c.stderr, "usage:")
		for _, sub := range []string{"token", "list", "drain", "undrain", "revoke", "join", "run"} {
			fmt.Fprintf(c.stderr, "  %s\n", nodeUsage[sub])
		}
	}
	if len(args) == 0 {
		c.fs.Usage()
		return 2
	}
	sub := args[0]
	line, ok := nodeUsage[sub]
	if !ok {
		fmt.Fprintf(c.stderr, "athena node: unknown subcommand %q\n", sub)
		c.fs.Usage()
		return 2
	}
	c.fs.Init("athena node "+sub, flag.ContinueOnError)
	c.fs.Usage = func() { fmt.Fprintf(c.stderr, "usage: %s\n", line) }
	if sub == "join" || sub == "run" {
		// The node's own state; --state-dir means the same, for whoever types it out of habit.
		c.fs.StringVar(&c.stateDir, "state", defaultNodeStateDir(), "this node's state directory")
	}
	switch sub {
	case "token":
		return c.nodeToken(args[1:])
	case "list":
		return c.nodeList(args[1:])
	case "drain", "undrain":
		return c.nodeDrain(args[1:], sub == "drain")
	case "revoke":
		return c.nodeRevoke(args[1:])
	case "join":
		return c.nodeJoin(ctx, args[1:])
	default: // run
		return c.nodeRun(ctx, args[1:])
	}
}

// joinCommand is the line `athena node token` prints, to paste on the new machine. fingerprint is the dashboard CA's;
// with ACME ("") the system roots verify the server and the flag is left out.
func joinCommand(url, token, fingerprint string) string {
	return server.JoinCommand(url, token, fingerprint)
}

func (c *command) nodeToken(args []string) int {
	ttl := c.fs.Duration("ttl", fleet.JoinTokenTTL, "how long the token is good (at most 24h)")
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	if *ttl <= 0 || *ttl > fleet.MaxJoinTokenTTL {
		return c.fail(errors.New("athena node token: a join token is good for more than 0 and at most 24h; give --ttl in that range"))
	}
	var r server.NodeTokenResult
	if err := call(c.stateDir, server.Request{Op: server.OpNodeToken, TTL: *ttl}, &r); err != nil {
		return c.fail(err)
	}
	// One line, nothing else: it is meant to be copied whole. The token is in it and nowhere else.
	fmt.Fprintln(c.stdout, joinCommand(r.URL, r.Token, r.Fingerprint))
	return 0
}

func (c *command) nodeList(args []string) int {
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	var rows []server.NodeRow
	if err := call(c.stateDir, server.Request{Op: server.OpNodes}, &rows); err != nil {
		return c.fail(err)
	}
	printNodes(c.stdout, rows, time.Now())
	return 0
}

// printNodes prints the nodes as a table: name, id, state, slots in use of all, runtimes, last seen and when the
// node's certificate expires (the date; "expired" once past; "-" for this machine and when not known).
func printNodes(out io.Writer, rows []server.NodeRow, now time.Time) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tID\tSTATE\tSLOTS\tRUNTIMES\tLAST SEEN\tEXPIRES")
	for _, r := range rows {
		name := safeText(r.Name)
		slots := "-"
		if r.Slots > 0 {
			slots = fmt.Sprintf("%d/%d", r.SlotsInUse, r.Slots)
		}
		var rts []string
		for _, rt := range r.Runtimes {
			if slices.Contains(r.NotReady, rt) {
				rt += "(not ready)"
			}
			rts = append(rts, safeText(rt))
		}
		runtimes := strings.Join(rts, ",")
		if runtimes == "" {
			runtimes = "-"
		}
		seen := "never"
		switch {
		case r.Local:
			seen = "this machine"
		case r.State == fleet.StateOnline || r.State == fleet.StateDraining: // a remote node shows draining only online
			seen = "now"
		case !r.LastSeen.IsZero():
			seen = formatAge(now.Sub(r.LastSeen)) + " ago"
		}
		expires := "-"
		switch {
		case r.Local || r.CertExpires.IsZero():
		case !now.Before(r.CertExpires):
			expires = "expired"
		default:
			expires = r.CertExpires.Local().Format("2006-01-02")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", name, safeText(r.ID), r.State, slots, runtimes, seen, expires)
	}
	w.Flush()
}

func (c *command) nodeDrain(args []string, on bool) int {
	pos, code, ok := c.parse(args, 1, 1)
	if !ok {
		return code
	}
	op, said := server.OpNodeUndrain, "taking new work"
	if on {
		op, said = server.OpNodeDrain, "draining"
	}
	var r server.NodeRow
	if err := call(c.stateDir, server.Request{Op: op, ID: pos[0]}, &r); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.stdout, "%s: %s\n", safeText(r.Name), said)
	return 0
}

func (c *command) nodeRevoke(args []string) int {
	yes := c.fs.Bool("yes", false, "revoke without asking")
	pos, code, ok := c.parse(args, 1, 1)
	if !ok {
		return code
	}
	// The question names the node, however the owner named it.
	var rows []server.NodeRow
	if err := call(c.stateDir, server.Request{Op: server.OpNodes}, &rows); err != nil {
		return c.fail(err)
	}
	var target *server.NodeRow
	for i := range rows {
		if rows[i].ID == pos[0] {
			target = &rows[i]
			break
		}
	}
	for i := range rows {
		if target == nil && rows[i].Name == pos[0] {
			target = &rows[i]
		}
	}
	if target == nil {
		return c.fail(fmt.Errorf("No node is named %s; athena node list lists them.", safeText(pos[0])))
	}
	// The server's own node is refused by the server: there is nothing to ask.
	if !*yes && !target.Local && target.State != fleet.StateRevoked {
		fmt.Fprintf(c.stdout, "Revoke %s? Its sessions stop and it must join again. [y/N] ", safeText(target.Name))
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(c.stdout, "not revoked")
			return 1
		}
	}
	var r server.NodeRow
	if err := call(c.stateDir, server.Request{Op: server.OpNodeRevoke, ID: target.ID}, &r); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.stdout, "%s: revoked; it must join again to work for this server\n", safeText(r.Name))
	return 0
}

func (c *command) nodeJoin(ctx context.Context, args []string) int {
	name := c.fs.String("name", "", "this node's name (default: the host name, lower case)")
	fp := c.fs.String("ca-sha256", "", "the dashboard CA's SHA-256 fingerprint, from athena node token (left out with ACME)")
	// A join token is base64url, so it may start with "-": it is a positional all the same.
	pos, code, ok := c.parse(dashPositionals(c.fs, args), 2, 2)
	if !ok {
		return code
	}
	url, token := strings.TrimRight(pos[0], "/"), pos[1]
	if !strings.HasPrefix(url, "https://") {
		return c.fail(fmt.Errorf("athena node join: %s is not the dashboard's https:// URL", url))
	}
	if *name == "" {
		host, _ := os.Hostname()
		*name = fleet.LocalName(host)
	}
	if !fleet.ValidName(*name) {
		return c.fail(errors.New("athena node join: --name: a node name is 1 to 32 characters of a-z, 0-9 and -, starting with a letter"))
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	id, err := nodelink.Join(ctx, url, token, *name, *fp, c.stateDir)
	if err != nil {
		return c.fail(err)
	}
	run := "athena node run"
	if c.stateDir != defaultNodeStateDir() {
		run += " --state " + c.stateDir
	}
	fmt.Fprintf(c.stdout, "Joined as %s (%s). Start the node with: %s\n", safeText(id.Name), safeText(id.NodeID), run)
	return 0
}

func (c *command) nodeRun(ctx context.Context, args []string) int {
	slots := c.fs.Int("slots", 0, "how many sessions run at once (default 4)")
	model := c.fs.String("model", "", "model of a task that names none (default sonnet)")
	blocked := c.fs.String("blocked", "", "comma-separated commands sessions may not run")
	timeout := c.fs.Duration("approval-timeout", 0, "how long an escalated tool call waits for an answer (default 30m)")
	claudeBin := c.fs.String("claude", "", "the claude executable (default claude)")
	codexBin := c.fs.String("codex", "", "the codex executable (default: runtimes.codex.binary of the node's config.json, or codex)")
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	var list []string
	for _, b := range strings.Split(*blocked, ",") {
		if b = strings.TrimSpace(b); b != "" {
			list = append(list, b)
		}
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	err := server.RunNode(ctx, server.NodeRunConfig{
		StateDir: c.stateDir,
		Node: node.Config{Slots: *slots, Model: *model, Blocked: list, ApprovalTimeout: *timeout,
			ClaudeBinary: *claudeBin},
		CodexBinary: *codexBin,
		Stderr:      c.stderr,
		Version:     Version,
	})
	if err != nil {
		return c.fail(err)
	}
	return 0
}

// --- retry-elsewhere ---

func (c *command) retryElsewhere(args []string) int {
	pos, code, ok := c.parse(args, 1, 1)
	if !ok {
		return code
	}
	if err := call(c.stateDir, server.Request{Op: server.OpRetryElsewhere, ID: pos[0]}, nil); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.stdout, "%s: retrying elsewhere\n", pos[0])
	return 0
}
