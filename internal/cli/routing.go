package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/server"
)

// Version is Athena's version (main sets it); the server sends it to GitHub in the User-Agent.
var Version = "dev"

const githubCheckTimeout = time.Minute

// --- github ---

// github runs `athena github setup` (writes the App into the configuration) and `athena github check` (talks to
// GitHub through the App the configuration names). Neither needs the server; the server reads the configuration when
// it starts.
func (c *command) github(ctx context.Context, args []string) int {
	if len(args) == 0 {
		c.fs.Usage()
		return 2
	}
	switch args[0] {
	case "setup":
		return c.githubSetup(args[1:])
	case "check":
		return c.githubCheck(ctx, args[1:])
	}
	fmt.Fprintf(c.stderr, "athena github: unknown subcommand %q\n", args[0])
	c.fs.Usage()
	return 2
}

// repoList is a repeatable --repo flag; a value may also list several, comma-separated.
type repoList []string

func (r *repoList) String() string { return strings.Join(*r, ",") }
func (r *repoList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*r = append(*r, p)
		}
	}
	return nil
}

func (c *command) githubSetup(args []string) int {
	appID := c.fs.Int64("app-id", 0, "the GitHub App's id")
	slug := c.fs.String("slug", "", "the App's URL name (its bot is <slug>[bot])")
	key := c.fs.String("key", "", "the App's private key file (PEM, mode 0600); its path is stored, never the key")
	inst := c.fs.Int64("installation", 0, "the installation id")
	interval := c.fs.Int("interval", 0, "seconds between polls of every repository (default 30)")
	var repos repoList
	c.fs.Var(&repos, "repo", "a repository (owner/name) to work in; repeat for more")
	var receiveAll repoList
	c.fs.Var(&receiveAll, "receive-all", "a configured repository that receives every message, not only mentions; repeat for more")
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	g := server.GitHubConfig{AppID: *appID, Slug: *slug, KeyFile: *key, InstallationID: *inst, Repos: repos, IntervalSeconds: *interval,
		ReceiveAll: receiveAll}
	if err := server.SetupGitHub(c.stateDir, g); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.stdout, "github: app %d (%s), installation %d, repositories %s written to %s\n", *appID, *slug, *inst,
		strings.Join(repos, ", "), filepath.Join(c.stateDir, server.ConfigFile))
	fmt.Fprintln(c.stdout, "run athena github check, then restart athena server")
	return 0
}

func (c *command) githubCheck(ctx context.Context, args []string) int {
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(ctx, githubCheckTimeout)
	defer cancel()
	got, err := server.CheckGitHub(ctx, c.stateDir, Version, nil)
	if err != nil {
		return c.fail(err)
	}
	printGitHubCheck(c.stdout, got)
	return 0
}

func printGitHubCheck(w io.Writer, g server.GitHubCheck) {
	fmt.Fprintf(w, "bot:          %s\n", g.Bot)
	fmt.Fprintf(w, "account:      %s\n", g.Account)
	fmt.Fprintln(w, "repositories:")
	for _, r := range g.Repos {
		fmt.Fprintf(w, "  %s\n", r)
	}
}

// --- policy ---

func (c *command) policy(args []string) int {
	if len(args) == 0 {
		c.fs.Usage()
		return 2
	}
	switch args[0] {
	case "show":
		if _, code, ok := c.parse(args[1:], 0, 0); !ok {
			return code
		}
		var v json.RawMessage
		if err := call(c.stateDir, server.Request{Op: server.OpPolicyShow}, &v); err != nil {
			return c.fail(err)
		}
		var out bytes.Buffer
		if err := json.Indent(&out, v, "", "  "); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.stdout, out.String())
		return 0
	case "set":
		pos, code, ok := c.parse(args[1:], 1, 1)
		if !ok {
			return code
		}
		// The file is read here: the server runs elsewhere, and a relative path means this command's directory.
		data, err := os.ReadFile(pos[0])
		if err != nil {
			return c.fail(fmt.Errorf("athena policy set: %w", err))
		}
		if !json.Valid(data) {
			return c.fail(fmt.Errorf("athena policy set: %s is not JSON", pos[0]))
		}
		var res server.PolicySetResult
		if err := call(c.stateDir, server.Request{Op: server.OpPolicySet, Policy: data}, &res); err != nil {
			return c.fail(err)
		}
		told := "no open ticket is affected"
		if len(res.Notified) > 0 {
			told = "told " + strings.Join(res.Notified, ", ")
		}
		fmt.Fprintf(c.stdout, "policy revision %d (version %s) saved; %s\n", res.Revision, res.Version, told)
		return 0
	}
	fmt.Fprintf(c.stderr, "athena policy: unknown subcommand %q\n", args[0])
	c.fs.Usage()
	return 2
}

// --- routing views ---

func (c *command) triggers(args []string) int {
	thread := c.fs.String("thread", "", "only the Triggers of this Thread")
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	var list []server.TriggerView
	if err := call(c.stateDir, server.Request{Op: server.OpTriggers, Thread: *thread}, &list); err != nil {
		return c.fail(err)
	}
	printTriggers(c.stdout, list)
	return 0
}

func printTriggers(out io.Writer, list []server.TriggerView) {
	if len(list) == 0 {
		fmt.Fprintln(out, "no triggers")
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "AT\tID\tACTOR\tTHREAD\tOUTCOME\tTEXT")
	for _, t := range list {
		actor := t.Actor
		if t.Addressed != "" {
			actor += " (" + t.Addressed + ")"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.At.Local().Format(time.DateTime), t.ID, dash(actor), dash(t.Thread),
			dash(t.Outcome), t.Text)
	}
	w.Flush()
}

func (c *command) threads(args []string) int {
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	var list []server.ThreadView
	if err := call(c.stateDir, server.Request{Op: server.OpThreads}, &list); err != nil {
		return c.fail(err)
	}
	printThreads(c.stdout, list)
	return 0
}

func printThreads(out io.Writer, list []server.ThreadView) {
	if len(list) == 0 {
		fmt.Fprintln(out, "no threads")
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "THREAD\tQUEUED\tEVAL\tTICKETS\tDECISIONS\tCLOSED")
	for _, t := range list {
		eval := "-"
		if e := t.Eval; e != nil {
			eval = e.ID + " " + e.Strength
			if e.DeliveredAt != nil {
				eval += " delivered"
			} else {
				eval += " waiting"
			}
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%d\t%s\n", t.Key, t.Queued, eval, dash(strings.Join(t.Tickets, " ")), t.Decisions, dash(t.Closed))
	}
	w.Flush()
}

func (c *command) decisions(args []string) int {
	thread := c.fs.String("thread", "", "only the Decisions made on this Thread")
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	var list []server.DecisionView
	if err := call(c.stateDir, server.Request{Op: server.OpDecisions, Thread: *thread}, &list); err != nil {
		return c.fail(err)
	}
	printDecisions(c.stdout, list)
	return 0
}

func printDecisions(out io.Writer, list []server.DecisionView) {
	if len(list) == 0 {
		fmt.Fprintln(out, "no decisions")
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "AT\tID\tKIND\tTHREAD\tTICKET\tTRIGGERS\tREASONS")
	for _, d := range list {
		kind := d.Kind
		if d.WouldAccept {
			kind += " (would accept)"
		}
		if d.Dismissed {
			kind += " (dismissed)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.At.Local().Format(time.DateTime), d.ID, kind, dash(d.Thread),
			dash(d.TicketID), strconv.Itoa(len(d.Triggers)), dash(strings.Join(d.Reasons, "; ")))
	}
	w.Flush()
}

// routingCommand runs reevaluate, dismiss or unsubscribe: one id, one confirmation line.
func (c *command) routingCommand(args []string) int {
	pos, code, ok := c.parse(args, 1, 1)
	if !ok {
		return code
	}
	req := server.Request{ID: pos[0]}
	var done string
	switch c.name {
	case "reevaluate":
		req.Op, done = server.OpReevaluate, "queued for a fresh eval"
	case "dismiss":
		req.Op, done = server.OpDismiss, "dismissed"
	default:
		req.Op, req.ID, req.Thread, done = server.OpUnsubscribe, "", pos[0], "unsubscribed"
	}
	if err := call(c.stateDir, req, nil); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.stdout, "%s: %s\n", pos[0], done)
	return 0
}

// pauseCommand is athena pause and athena resume: the kill switch.
func (c *command) pauseCommand(args []string) int {
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	if c.name == "resume" {
		if err := call(c.stateDir, server.Request{Op: server.OpResume}, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.stdout, "Athena resumed.")
		return 0
	}
	if err := call(c.stateDir, server.Request{Op: server.OpPause}, nil); err != nil {
		return c.fail(err)
	}
	fmt.Fprintln(c.stdout, "Athena is paused. Running sessions stop at the end of their turn; nothing queued is lost. Resume with: athena resume")
	return 0
}

// proposalCommand is athena proposals, athena apply <id> and athena discard <id>: what Athena holds for the owner.
func (c *command) proposalCommand(args []string) int {
	if c.name == "proposals" {
		if _, code, ok := c.parse(args, 0, 0); !ok {
			return code
		}
		var list []server.ProposalRow
		if err := call(c.stateDir, server.Request{Op: server.OpProposals}, &list); err != nil {
			return c.fail(err)
		}
		if len(list) == 0 {
			fmt.Fprintln(c.stdout, "nothing is held for you")
			return 0
		}
		for _, p := range list {
			fmt.Fprintln(c.stdout, proposalLine(p))
		}
		return 0
	}
	pos, code, ok := c.parse(args, 1, 1)
	if !ok {
		return code
	}
	op, done := server.OpApply, "applied"
	if c.name == "discard" {
		op, done = server.OpDiscard, "discarded"
	}
	if err := call(c.stateDir, server.Request{Op: op, ID: pos[0]}, nil); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.stdout, "%s: %s\n", pos[0], done)
	return 0
}

// proposalLine is one Proposal: id, repository, kind and summary, two spaces apart, and for a failed one its error.
func proposalLine(p server.ProposalRow) string {
	line := p.ID + "  " + dash(p.Repo) + "  " + p.Kind + "  " + p.Summary
	if p.State == core.ProposalFailed {
		line += "  failed: " + p.Error
	}
	return line
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

var _ flag.Value = (*repoList)(nil)
