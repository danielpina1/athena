// Package cli implements the owner's commands: `athena server`, `athena github setup|check` (which read and write the
// configuration and talk to GitHub directly), and the commands that talk to the server over the control socket (task,
// status, logs, reply, stop, retry, approvals, approve, deny, metrics, policy, triggers, threads, decisions,
// reevaluate, dismiss, unsubscribe), and `athena web setup|status|ca|enroll|passkeys|revoke`, which work on the
// configuration, the CA and the store directly.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/metrics"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/render"
	"github.com/danielpina1/athena/internal/server"
)

const dialTimeout = 2 * time.Second

// usage is each command's usage line, without the --state-dir flag every command takes.
var usage = map[string]string{
	"server":      "athena server [--slots N] [--model M] [--blocked a,b] [--approval-timeout D] [--claude PATH]",
	"task":        `athena task "<text>" [--dir D | --repo owner/name] [--model M]`,
	"github":      "athena github setup --app-id N --slug S --key FILE --installation N --repo owner/name [--repo ...] [--receive-all owner/name ...] | athena github check",
	"policy":      "athena policy show | athena policy set FILE",
	"triggers":    "athena triggers [--thread K]",
	"threads":     "athena threads",
	"decisions":   "athena decisions [--thread K]",
	"reevaluate":  "athena reevaluate <trigger>",
	"dismiss":     "athena dismiss <decision>",
	"unsubscribe": "athena unsubscribe <thread>",
	"status":      "athena status [<id>] [--all]",
	"logs":        "athena logs <id> [-f]",
	"reply":       `athena reply <id> "<text>"`,
	"stop":        "athena stop <id> [--reason R]",
	"retry":       "athena retry <id>",
	"approvals":   "athena approvals",
	"approve":     "athena approve <id> [--note N]",
	"deny":        "athena deny <id> [--note N]",
	"metrics":     "athena metrics [--window 1h|24h|start]",
	"web":         "athena web setup --host H [--ip IP ...] [--listen ADDR] [--tls ca|acme|off] [--acme-email E --acme-provider P --acme-agree-tos] | athena web status | athena web ca | athena web enroll [--ttl D] | athena web passkeys | athena web revoke <id>",
}

// Run runs one command and returns its exit code: 0 on success, 1 when it failed, 2 on a usage error.
func Run(ctx context.Context, cmd string, args []string, stdout, stderr io.Writer) int {
	if _, ok := usage[cmd]; !ok {
		fmt.Fprintf(stderr, "athena: unknown command %q\n", cmd)
		return 2
	}
	c := &command{name: cmd, stdout: stdout, stderr: stderr, fs: flag.NewFlagSet("athena "+cmd, flag.ContinueOnError)}
	c.fs.SetOutput(stderr)
	c.fs.Usage = func() { fmt.Fprintf(stderr, "usage: %s [--state-dir S]\n", usage[cmd]) }
	c.fs.StringVar(&c.stateDir, "state-dir", defaultStateDir(), "Athena's state directory")
	switch cmd {
	case "server":
		return c.server(ctx, args)
	case "task":
		return c.task(args)
	case "status":
		return c.status(args)
	case "logs":
		return c.logs(ctx, args)
	case "reply":
		return c.reply(args)
	case "stop":
		return c.stop(args)
	case "retry":
		return c.retry(args)
	case "approvals":
		return c.approvals(args)
	case "metrics":
		return c.metrics(args)
	case "github":
		return c.github(ctx, args)
	case "web":
		return c.web(args)
	case "policy":
		return c.policy(args)
	case "triggers":
		return c.triggers(args)
	case "threads":
		return c.threads(args)
	case "decisions":
		return c.decisions(args)
	case "reevaluate", "dismiss", "unsubscribe":
		return c.routingCommand(args)
	default: // approve, deny
		return c.answer(args, cmd == "approve")
	}
}

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".athena"
	}
	return filepath.Join(home, ".athena")
}

type command struct {
	name     string
	stdout   io.Writer
	stderr   io.Writer
	fs       *flag.FlagSet
	stateDir string
}

// parse parses flags anywhere among the arguments and returns the positional ones; ok is false after a usage error
// (code is the exit code to return then).
func (c *command) parse(args []string, minPos, maxPos int) (pos []string, code int, ok bool) {
	for {
		if err := c.fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, 0, false
			}
			return nil, 2, false
		}
		rest := c.fs.Args()
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			pos = append(pos, rest...)
			break
		}
		if len(rest) == 0 {
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	if len(pos) < minPos || (maxPos >= 0 && len(pos) > maxPos) {
		c.fs.Usage()
		return nil, 2, false
	}
	if abs, err := filepath.Abs(c.stateDir); err == nil {
		c.stateDir = abs
	}
	return pos, 0, true
}

// fail prints an error and returns exit code 1.
func (c *command) fail(err error) int {
	fmt.Fprintln(c.stderr, err.Error())
	return 1
}

// --- server ---

func (c *command) server(ctx context.Context, args []string) int {
	slots := c.fs.Int("slots", 0, "how many sessions run at once (default 4)")
	model := c.fs.String("model", "", "model of a task that names none (default sonnet)")
	blocked := c.fs.String("blocked", "", "comma-separated commands sessions may not run")
	timeout := c.fs.Duration("approval-timeout", 0, "how long an escalated tool call waits for an answer (default 30m)")
	claudeBin := c.fs.String("claude", "", "the claude executable (default claude)")
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	var list []string
	for _, b := range strings.Split(*blocked, ",") {
		if b = strings.TrimSpace(b); b != "" {
			list = append(list, b)
		}
	}
	// A closed terminal sends SIGHUP: it stops the server gracefully, like Ctrl-C, so its sessions end and resume on
	// the next start instead of being orphaned.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	err := server.Run(ctx, server.Config{
		StateDir: c.stateDir,
		Node: node.Config{
			Slots: *slots, Model: *model, Blocked: list, ApprovalTimeout: *timeout, ClaudeBinary: *claudeBin,
		},
		Stderr:  c.stderr,
		Version: Version,
	})
	if err != nil {
		return c.fail(err)
	}
	return 0
}

// --- commands over the control socket ---

func (c *command) task(args []string) int {
	dir := c.fs.String("dir", "", "directory the task runs in (a git repository gets its own worktree)")
	repo := c.fs.String("repo", "", "GitHub repository (owner/name) the task works in, through the App")
	model := c.fs.String("model", "", "model for this task")
	pos, code, ok := c.parse(args, 1, -1)
	if !ok {
		return code
	}
	if *dir != "" && *repo != "" {
		fmt.Fprintln(c.stderr, "athena task: --dir and --repo exclude each other")
		c.fs.Usage()
		return 2
	}
	// The server runs elsewhere: a relative directory means this command's working directory.
	taskDir := *dir
	if taskDir != "" {
		abs, err := filepath.Abs(taskDir)
		if err != nil {
			return c.fail(fmt.Errorf("athena task: directory %s: %w", taskDir, err))
		}
		taskDir = abs
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := call(c.stateDir, server.Request{Op: server.OpSubmit, Text: strings.Join(pos, " "), Dir: taskDir, Repo: *repo, Model: *model}, &out); err != nil {
		return c.fail(err)
	}
	fmt.Fprintln(c.stdout, out.ID)
	return 0
}

func (c *command) status(args []string) int {
	all := c.fs.Bool("all", false, "include ended tickets")
	pos, code, ok := c.parse(args, 0, 1)
	if !ok {
		return code
	}
	if len(pos) == 1 {
		var d server.TicketDetail
		if err := call(c.stateDir, server.Request{Op: server.OpTicket, ID: pos[0]}, &d); err != nil {
			return c.fail(err)
		}
		printDetail(c.stdout, d, time.Now())
		return 0
	}
	var list []server.TicketSummary
	if err := call(c.stateDir, server.Request{Op: server.OpTickets, All: *all}, &list); err != nil {
		return c.fail(err)
	}
	var health server.HealthView
	if err := call(c.stateDir, server.Request{Op: server.OpHealth}, &health); err != nil {
		return c.fail(err)
	}
	printHealth(c.stdout, health)
	fmt.Fprintln(c.stdout)
	if len(list) == 0 {
		fmt.Fprintln(c.stdout, "no tickets")
		return 0
	}
	now := time.Now()
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTAGE\tAGE\tTITLE")
	for _, t := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.ID, t.Stage, formatAge(now.Sub(t.CreatedAt)), t.Title)
	}
	w.Flush()
	return 0
}

// printHealth prints the Sources, the effects runner and the router, one line each.
func printHealth(w io.Writer, h server.HealthView) {
	if len(h.Sources) == 0 {
		fmt.Fprintln(w, "sources:   none (owner tasks only; see athena github setup)")
	}
	for _, s := range h.Sources {
		state := "ok"
		if s.Error != "" {
			state = "error: " + s.Error
		}
		line := fmt.Sprintf("source:    %s %s", s.Name, state)
		if s.Name == "github" && h.GitHubRateRemaining != nil {
			line += fmt.Sprintf(" (rate remaining %d)", *h.GitHubRateRemaining)
		}
		fmt.Fprintln(w, line)
	}
	if len(h.Effects) == 0 {
		fmt.Fprintln(w, "effects:   ok")
	} else {
		kinds := make([]string, 0, len(h.Effects))
		for k := range h.Effects {
			kinds = append(kinds, k)
		}
		slices.Sort(kinds)
		for _, k := range kinds {
			fmt.Fprintf(w, "effects:   %s: %s\n", k, h.Effects[k])
		}
	}
	line := "router:    " + h.Router.State
	if h.Router.Session != "" {
		line += " (session " + h.Router.Session + ")"
	}
	if h.Router.Error != "" {
		line += ": " + h.Router.Error
	}
	fmt.Fprintln(w, line)
}

func printDetail(w io.Writer, d server.TicketDetail, now time.Time) {
	fmt.Fprintf(w, "%s  %s\n", d.ID, d.Title)
	fmt.Fprintf(w, "stage:     %s\n", d.Stage)
	fmt.Fprintf(w, "age:       %s (created %s)\n", formatAge(now.Sub(d.CreatedAt)), d.CreatedAt.Local().Format(time.DateTime))
	if d.Dir != "" {
		fmt.Fprintf(w, "dir:       %s\n", d.Dir)
	}
	if d.SourceDir != "" && d.SourceDir != d.Dir {
		fmt.Fprintf(w, "source:    %s\n", d.SourceDir)
	}
	if d.Repo != "" {
		fmt.Fprintf(w, "repo:      %s\n", d.Repo)
	}
	model := d.Model
	if model == "" {
		model = "default"
	}
	fmt.Fprintf(w, "runtime:   %s (model %s)\n", d.Runtime, model)
	if d.Question != "" {
		fmt.Fprintf(w, "question:  %s\n", d.Question)
	}
	if d.Undelivered > 0 {
		fmt.Fprintf(w, "waiting:   %d input(s) not yet delivered\n", d.Undelivered)
	}
	if e := d.Ended; e != nil {
		line := fmt.Sprintf("%s by %s at %s", e.Outcome, e.By, e.At.Local().Format(time.DateTime))
		if e.Reason != "" {
			line += ": " + e.Reason
		}
		fmt.Fprintf(w, "ended:     %s\n", line)
	}
	if a := d.Approval; a != nil {
		fmt.Fprintf(w, "approval:  %s %s: %s\n", a.ID, a.Tool, a.Summary)
	}
	printStats(w, d.Stats)
	fmt.Fprintln(w, "assignments:")
	for _, a := range d.Assignments {
		line := fmt.Sprintf("  %s  %s", a.ID, a.State)
		if a.StartedAt != nil {
			line += "  started " + a.StartedAt.Local().Format(time.DateTime)
		}
		if a.EndedAt != nil {
			line += "  ended " + a.EndedAt.Local().Format(time.DateTime)
			if a.EndedReason != "" {
				line += " (" + a.EndedReason + ")"
			}
		}
		fmt.Fprintln(w, line)
		if r := a.Report; r != nil {
			fmt.Fprintf(w, "    report: %s: %s\n", r.State, r.Summary)
		}
	}
}

// printStats prints a Ticket's performance numbers.
func printStats(w io.Writer, s core.TicketStats) {
	fmt.Fprintln(w, "stats:")
	fmt.Fprintf(w, "  queue wait:      %s\n", formatSeconds(s.QueueWait.Seconds()))
	fmt.Fprintf(w, "  first output:    %s\n", formatSeconds(s.TimeToFirstOutput.Seconds()))
	fmt.Fprintf(w, "  first report:    %s\n", formatSeconds(s.TimeToFirstReport.Seconds()))
	fmt.Fprintf(w, "  active time:     %s\n", formatSeconds(s.ActiveTime.Seconds()))
	fmt.Fprintf(w, "  person wait:     %s\n", formatSeconds(s.PersonWaitTime.Seconds()))
	fmt.Fprintf(w, "  turns:           %d\n", s.Turns)
	fmt.Fprintf(w, "  tool calls:      %d (%d errors)\n", s.ToolCalls, s.ToolErrors)
	fmt.Fprintf(w, "  hooks:           %d denials, %d escalations\n", s.HookDenials, s.Escalations)
	fmt.Fprintf(w, "  attention:       %d questions, %d messages from you\n", s.Questions, s.PersonMessages)
	fmt.Fprintf(w, "  retries/crashes: %d / %d\n", s.Retries, s.Crashes)
	fmt.Fprintf(w, "  tokens:          %d in, %d out, %d cache read (%.0f%% cache hits)\n",
		s.InputTokens, s.OutputTokens, s.CacheReadTokens, 100*s.CacheHitRatio())
	fmt.Fprintf(w, "  cost:            $%.4f\n", s.CostUSD)
}

// formatSeconds renders a duration given in seconds with three significant digits: 12µs, 3.4ms, 1.25s, 2m5s.
func formatSeconds(sec float64) string {
	d := time.Duration(sec * float64(time.Second))
	switch {
	case d <= 0:
		return "0"
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < 100*time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.2fs", d.Seconds())
	default:
		return d.Round(time.Second).String()
	}
}

func (c *command) metrics(args []string) int {
	window := c.fs.String("window", "1h", "period to report: 1h, 24h or start (since the server started)")
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	var snap metrics.Snapshot
	if err := call(c.stateDir, server.Request{Op: server.OpMetrics, Window: *window}, &snap); err != nil {
		return c.fail(err)
	}
	printMetrics(c.stdout, snap)
	return 0
}

// printMetrics prints a snapshot as a table: histograms with their count and percentiles, then counters and gauges
// with their value. Series without a sample in the window are left out.
func printMetrics(out io.Writer, snap metrics.Snapshot) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tLABELS\tCOUNT\tP50\tP95\tP99\tVALUE")
	for _, h := range snap.Histograms {
		if h.Count == 0 {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t-\n", h.Name, formatLabels(h.Labels), h.Count,
			formatSeconds(h.P50), formatSeconds(h.P95), formatSeconds(h.P99))
	}
	for _, v := range snap.Counters {
		if v.Value != 0 {
			fmt.Fprintf(w, "%s\t%s\t-\t-\t-\t-\t%s\n", v.Name, formatLabels(v.Labels), formatValue(v.Name, v.Value))
		}
	}
	for _, v := range snap.Gauges {
		fmt.Fprintf(w, "%s\t%s\t-\t-\t-\t-\t%s\n", v.Name, formatLabels(v.Labels), formatValue(v.Name, v.Value))
	}
	w.Flush()
}

func formatLabels(l map[string]string) string {
	if len(l) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + l[k]
	}
	return strings.Join(parts, ",")
}

func formatValue(name string, v float64) string {
	switch {
	case strings.HasSuffix(name, "_bytes"):
		return fmt.Sprintf("%.1f MB", v/(1<<20))
	case strings.HasSuffix(name, "_usd_total"):
		return fmt.Sprintf("$%.4f", v)
	case v == float64(int64(v)):
		return strconv.FormatInt(int64(v), 10)
	default:
		return strconv.FormatFloat(v, 'f', 3, 64)
	}
}

func (c *command) logs(ctx context.Context, args []string) int {
	var follow bool
	c.fs.BoolVar(&follow, "f", false, "follow the log until the ticket ends")
	c.fs.BoolVar(&follow, "follow", false, "follow the log until the ticket ends")
	pos, code, ok := c.parse(args, 1, 1)
	if !ok {
		return code
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := stream(ctx, c.stateDir, server.Request{Op: server.OpLogs, ID: pos[0], Follow: follow}, func(raw json.RawMessage) {
		var l render.LogLine
		if json.Unmarshal(raw, &l) == nil {
			fmt.Fprintf(c.stdout, "%s  %s  %s\n", l.At.Local().Format(time.TimeOnly), l.Kind, l.Text)
		}
	})
	if err != nil {
		return c.fail(err)
	}
	return 0
}

func (c *command) reply(args []string) int {
	pos, code, ok := c.parse(args, 2, -1)
	if !ok {
		return code
	}
	if err := call(c.stateDir, server.Request{Op: server.OpReply, ID: pos[0], Text: strings.Join(pos[1:], " ")}, nil); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.stdout, "%s: message added\n", pos[0])
	return 0
}

func (c *command) stop(args []string) int {
	reason := c.fs.String("reason", "", "why the ticket is stopped")
	pos, code, ok := c.parse(args, 1, 1)
	if !ok {
		return code
	}
	if err := call(c.stateDir, server.Request{Op: server.OpStop, ID: pos[0], Reason: *reason}, nil); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.stdout, "%s: stopped\n", pos[0])
	return 0
}

func (c *command) retry(args []string) int {
	pos, code, ok := c.parse(args, 1, 1)
	if !ok {
		return code
	}
	if err := call(c.stateDir, server.Request{Op: server.OpRetry, ID: pos[0]}, nil); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.stdout, "%s: retrying\n", pos[0])
	return 0
}

func (c *command) approvals(args []string) int {
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	var list []server.ApprovalView
	if err := call(c.stateDir, server.Request{Op: server.OpApprovals}, &list); err != nil {
		return c.fail(err)
	}
	if len(list) == 0 {
		fmt.Fprintln(c.stdout, "no pending approvals")
		return 0
	}
	now := time.Now()
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTICKET\tAGE\tTOOL\tSUMMARY")
	for _, a := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.ID, a.TicketID, formatAge(now.Sub(a.CreatedAt)), a.Tool, a.Summary)
	}
	w.Flush()
	return 0
}

func (c *command) answer(args []string, approve bool) int {
	note := c.fs.String("note", "", "a note for the session")
	pos, code, ok := c.parse(args, 1, 1)
	if !ok {
		return code
	}
	if err := call(c.stateDir, server.Request{Op: server.OpAnswer, ID: pos[0], Approve: approve, Note: *note}, nil); err != nil {
		return c.fail(err)
	}
	verb := "denied"
	if approve {
		verb = "approved"
	}
	fmt.Fprintf(c.stdout, "%s: %s\n", pos[0], verb)
	return 0
}

// formatAge renders a duration in its largest whole unit: 42s, 5m, 3h, 2d.
func formatAge(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

// --- control client ---

// response is a control response with its data left raw.
type response struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error"`
	Data  json.RawMessage `json:"data"`
}

// dial connects to the server's control socket, or says it does not run.
func dial(stateDir string) (net.Conn, error) {
	path := filepath.Join(stateDir, server.ControlSocket)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("athena server is not running (no %s)", path)
	}
	conn, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("athena server is not running (%s does not answer)", path)
	}
	return conn, nil
}

func send(conn net.Conn, req server.Request) error {
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("athena: send request: %w", err)
	}
	return nil
}

// call sends one request and decodes the response's data into out (unless nil). A refusal is returned as an error
// carrying the server's message.
func call(stateDir string, req server.Request, out any) error {
	conn, err := dial(stateDir)
	if err != nil {
		return err
	}
	defer conn.Close()
	return exchange(conn, req, out)
}

// exchange is call on a connection already dialed.
func exchange(conn net.Conn, req server.Request, out any) error {
	if err := send(conn, req); err != nil {
		return err
	}
	var r response
	if err := json.NewDecoder(conn).Decode(&r); err != nil {
		return fmt.Errorf("athena: read response: %w", err)
	}
	if !r.OK {
		return errors.New(r.Error)
	}
	if out != nil && len(r.Data) > 0 {
		if err := json.Unmarshal(r.Data, out); err != nil {
			return fmt.Errorf("athena: decode response: %w", err)
		}
	}
	return nil
}

// stream sends one request and hands each response line's data to fn until the server ends the stream or ctx is done.
func stream(ctx context.Context, stateDir string, req server.Request, fn func(json.RawMessage)) error {
	conn, err := dial(stateDir)
	if err != nil {
		return err
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopClose()
	if err := send(conn, req); err != nil {
		return err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var r response
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return fmt.Errorf("athena: read response: %w", err)
		}
		if !r.OK {
			return errors.New(r.Error)
		}
		fn(r.Data)
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("athena: read response: %w", err)
	}
	return nil
}
