package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/sources/github"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A worker's MCP tools beside report (reference 007 "Executor tools", under Athena's names): ack, reply,
// follow_thread, mark_unrelated and handback. `athena mcp --ticket` forwards each call, with the session's Ticket and
// Assignment, to the node socket (OpWorker), which runs it on the Engine for that live session only; an Engine refusal
// comes back as a tool error the model reads.

// OpWorker runs one of a worker's tools other than report; OpWorkerTools lists the tools a session's MCP server offers
// (a WorkerRequest without Tool and Args, answered with a WorkerToolsResponse).
const (
	OpWorker      = "worker"
	OpWorkerTools = "worker_tools"
)

// WorkerToolsResponse names the tools a session's MCP server offers, report first, or the Error that refused to.
type WorkerToolsResponse struct {
	OK    bool     `json:"ok"`
	Tools []string `json:"tools,omitempty"`
	Error string   `json:"error,omitempty"`
}

// WorkerRequest asks the node to run one worker tool with its JSON arguments for the session of Ticket and Assignment.
type WorkerRequest struct {
	Op         string          `json:"op"`
	Ticket     string          `json:"ticket"`
	Assignment string          `json:"assignment"`
	Tool       string          `json:"tool"`
	Args       json.RawMessage `json:"args,omitempty"`
}

// WorkerResponse is a worker tool's JSON result, or the Error that refused the call.
type WorkerResponse struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

var workerTools = []routerTool{
	{"ack", "Acknowledge a person's request that reached your ticket, before substantial work on it: one brief sentence, " +
		"at most 100 words, naming the concrete object and the intended result (\"On it: renaming the Save changes button " +
		"to Save.\"). It is posted as a reply to their message, once per message. Check the input card's acknowledged line " +
		"first: when Athena acknowledged the message already (it often does when it accepts a ticket), do not ack it; a " +
		"repeat ack posts nothing. Name the card's trigger_id, never the input id. Do not acknowledge bots, CI, thanks, " +
		"ordinary discussion, messages you hand back or the owner's messages (they have no trigger_id), and do not repeat " +
		"or quote the message.",
		obj([]string{"trigger_id", "text"}, map[string]any{
			"trigger_id": sprop("The trigger_id line of the person's input card (not the input id)."),
			"text":       sprop("The acknowledgement, at most 100 words."),
		})},
	{"reply", "Answer a person's explicit question that reached your ticket, naming the trigger_id of its input card; " +
		"a quick question needs no acknowledgement first. Only answers: never use it for progress, results or questions. " +
		"Athena delivers the result to people from your ready (or dropped) report, and asks your question from the " +
		"question of a waiting or blocked report. At most 4,000 characters; GitHub comments support Markdown.",
		obj([]string{"trigger_id", "text"}, map[string]any{
			"trigger_id": sprop("The trigger_id line of the question's input card (not the input id)."),
			"text":       sprop("The answer."),
			"purpose":    enum("answer, the only purpose a worker sends; it may be omitted.", core.PurposeAnswer),
		})},
	{"follow_thread", "Subscribe your ticket to a related thread for context: its messages reach you but are not requests " +
		"to you and do not resume the ticket. Name the thread by its key (github:owner/name#12), as owner/name#12, as #12 " +
		"in your ticket's repository, or by its GitHub URL.",
		obj([]string{"thread"}, map[string]any{"thread": sprop("The thread.")})},
	{"mark_unrelated", "Drop a thread that was attached to your ticket after it started and turned out to be about " +
		"something else, with a one-sentence reason; the follow-ups there that you own are handed back. Read its original " +
		"messages first. Your ticket's own origin thread cannot be dropped.",
		obj([]string{"thread", "reason"}, map[string]any{
			"thread": sprop("The thread, in any form follow_thread accepts."),
			"reason": sprop("One sentence: why it is unrelated."),
		})},
	{"handback", "Hand back, once and unacknowledged, a person's message that reached your ticket but asks for work beyond " +
		"its scope or for another ticket, with a one-sentence reason; it goes to a fresh assessment. Returns queued: false " +
		"when another ticket still owns the message or yours never did.",
		obj([]string{"trigger_id", "reason"}, map[string]any{
			"trigger_id": sprop("The trigger_id of the message, from its input card."),
			"reason":     sprop("One sentence: why it is not this ticket's."),
		})},
}

// restrictedWithout are the worker tools a restricted session (toolpolicy.go) goes without: follow_thread reads other
// Threads.
var restrictedWithout = []string{"follow_thread"}

// WorkerTools returns the names of a worker's tools, report first, in the order the server lists them.
func WorkerTools() []string { return workerToolsFor(false) }

// workerToolsFor is WorkerTools for a session under the standard tool policy, or the restricted one.
func workerToolsFor(restricted bool) []string {
	out := []string{"report"}
	for _, t := range workerTools {
		if !restricted || !slices.Contains(restrictedWithout, t.name) {
			out = append(out, t.name)
		}
	}
	return out
}

// sessionWorkerTools asks the node which tools the session's MCP server offers. When the node cannot say, it offers a
// restricted session's: the server fails closed.
func sessionWorkerTools(ticketID, assignmentID, socket string) []string {
	var resp WorkerToolsResponse
	if err := callNode(socket, WorkerRequest{Op: OpWorkerTools, Ticket: ticketID, Assignment: assignmentID}, &resp); err != nil || !resp.OK {
		return workerToolsFor(true)
	}
	return resp.Tools
}

// handleWorkerTools names the tools a live worker session's MCP server offers (workerToolsFor its tool policy).
func (n *Node) handleWorkerTools(req WorkerRequest) WorkerToolsResponse {
	s := n.lookup(req.Ticket, req.Assignment)
	if s == nil {
		return WorkerToolsResponse{Error: fmt.Sprintf("%s: ticket %q assignment %q", denyNoSession, req.Ticket, req.Assignment)}
	}
	if s.reviewing() {
		return WorkerToolsResponse{Error: "a review session has only the review tool"}
	}
	return WorkerToolsResponse{OK: true, Tools: workerToolsFor(s.restricted())}
}

// addWorkerTools adds the forwarding tools named in offered to a session's MCP server.
func addWorkerTools(s *mcp.Server, ticketID, assignmentID, socket string, offered []string) {
	for _, t := range workerTools {
		if !slices.Contains(offered, t.name) {
			continue
		}
		name := t.name
		s.AddTool(&mcp.Tool{Name: name, Description: t.description, InputSchema: t.schema},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				args := req.Params.Arguments
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				var resp WorkerResponse
				err := callNode(socket, WorkerRequest{Op: OpWorker, Ticket: ticketID, Assignment: assignmentID, Tool: name, Args: args}, &resp)
				switch {
				case err != nil:
					return toolError("the athena node is unreachable: " + err.Error()), nil
				case !resp.OK:
					return toolError(name + " refused: " + resp.Error), nil
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(resp.Result)}}}, nil
			})
	}
}

// handleWorker runs one worker tool for a live session.
func (n *Node) handleWorker(req WorkerRequest) WorkerResponse {
	label := "worker_unknown"
	known := slices.ContainsFunc(workerTools, func(t routerTool) bool { return t.name == req.Tool })
	if known {
		label = req.Tool
	}
	if n.reg != nil {
		start := time.Now()
		defer func() { n.reg.Histogram("mcp_tool_seconds", "tool", label).Observe(time.Since(start)) }()
	}
	s := n.lookup(req.Ticket, req.Assignment)
	if s == nil {
		return WorkerResponse{Error: fmt.Sprintf("%s: ticket %q assignment %q", denyNoSession, req.Ticket, req.Assignment)}
	}
	if !known {
		return WorkerResponse{Error: fmt.Sprintf("unknown tool %q", req.Tool)}
	}
	if s.reviewing() {
		return WorkerResponse{Error: "a review session has only the review tool"}
	}
	if s.restricted() && slices.Contains(restrictedWithout, req.Tool) {
		// Its server does not offer the tool; a call that comes anyway is refused.
		s.log.Warn("worker tool refused", "tool", req.Tool, "error", "restricted tool policy")
		return WorkerResponse{Error: restrictedFollowReason}
	}
	res, err := n.workerTool(s, req.Tool, req.Args)
	if err != nil {
		s.log.Warn("worker tool refused", "tool", req.Tool, "error", err)
		return WorkerResponse{Error: err.Error()}
	}
	s.log.Info("worker tool", "tool", req.Tool)
	raw, err := json.Marshal(res)
	if err != nil {
		return WorkerResponse{Error: "the result does not encode: " + err.Error()}
	}
	return WorkerResponse{OK: true, Result: raw}
}

// decodeArgs decodes a tool's arguments, refusing unknown fields.
func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("arguments: %w", err)
	}
	return nil
}

func (n *Node) workerTool(s *session, tool string, raw json.RawMessage) (any, error) {
	eng := n.engine()
	switch tool {
	case "ack":
		var in struct {
			TriggerID string `json:"trigger_id"`
			Text      string `json:"text"`
		}
		if err := decodeArgs(raw, &in); err != nil {
			return nil, err
		}
		if in.TriggerID == "" {
			return nil, errors.New("trigger_id is required: name the trigger_id line of the person's input card; the " +
				"owner's messages have none and need no acknowledgement")
		}
		out, err := eng.ReplyOnce(core.ReplyRequest{TicketID: s.t.ID, TriggerID: in.TriggerID, Purpose: core.PurposeAcknowledgement, Text: in.Text})
		if err != nil {
			return nil, err
		}
		if out.Repeat {
			return map[string]any{"queued": false, "receipt": out.ReceiptID, "note": "Already acknowledged (Athena or you " +
				"posted an acknowledgement of this message before), so nothing new was posted. Go on with the work."}, nil
		}
		return map[string]any{"queued": true, "receipt": out.ReceiptID}, nil
	case "reply":
		var in struct {
			Purpose   string `json:"purpose"`
			Text      string `json:"text"`
			TriggerID string `json:"trigger_id,omitempty"`
		}
		if err := decodeArgs(raw, &in); err != nil {
			return nil, err
		}
		switch in.Purpose {
		case "", core.PurposeAnswer:
		case core.PurposeAcknowledgement:
			return nil, errors.New("acknowledge with the ack tool")
		case core.PurposeResult:
			return nil, errors.New("a worker sends no result: Athena posts the result to people from your report. When the " +
				"work is done and verified, file report {status: ready, summary, pull} in a step of its own (pull when there is " +
				"a pull request; no question); a ready report filed already needs nothing more")
		case core.PurposeAttention:
			return nil, errors.New("a worker sends no question by reply: file report {status: waiting (or blocked), summary, " +
				"question}, and Athena asks the person")
		default:
			return nil, fmt.Errorf("unknown purpose %q: a worker's reply is an answer", in.Purpose)
		}
		if in.TriggerID == "" {
			return nil, errors.New("trigger_id is required: name the trigger_id line of the question's input card; answer " +
				"the owner's messages, which have none, in your session output")
		}
		out, err := eng.ReplyOnce(core.ReplyRequest{TicketID: s.t.ID, TriggerID: in.TriggerID, Purpose: core.PurposeAnswer, Text: in.Text})
		if err != nil {
			return nil, err
		}
		if out.Repeat {
			return map[string]any{"queued": false, "receipt": out.ReceiptID, "note": "This answer was queued before; nothing new was sent."}, nil
		}
		return map[string]any{"queued": true, "receipt": out.ReceiptID}, nil
	case "follow_thread":
		var in struct {
			Thread string `json:"thread"`
		}
		if err := decodeArgs(raw, &in); err != nil {
			return nil, err
		}
		key, err := ThreadArg(in.Thread, s.t.Repo)
		if err != nil {
			return nil, err
		}
		if err := eng.FollowForContext(s.t.ID, key); err != nil {
			return nil, err
		}
		return map[string]any{"thread": key, "following": "context"}, nil
	case "mark_unrelated":
		var in struct {
			Thread string `json:"thread"`
			Reason string `json:"reason"`
		}
		if err := decodeArgs(raw, &in); err != nil {
			return nil, err
		}
		key, err := ThreadArg(in.Thread, s.t.Repo)
		if err != nil {
			return nil, err
		}
		if err := eng.MarkUnrelated(s.t.ID, key, in.Reason); err != nil {
			return nil, err
		}
		return map[string]any{"thread": key, "dropped": true}, nil
	case "handback":
		var in struct {
			TriggerID string `json:"trigger_id"`
			Reason    string `json:"reason"`
		}
		if err := decodeArgs(raw, &in); err != nil {
			return nil, err
		}
		queued, err := eng.Handback(s.t.ID, in.TriggerID, in.Reason)
		if err != nil {
			return nil, err
		}
		return map[string]any{"queued": queued}, nil
	}
	return nil, fmt.Errorf("unknown tool %q", tool)
}

// ThreadArg turns a thread a worker names into its canonical key: a key (<source>:<key>; GitHub ones lower-cased), a
// GitHub issue or pull request URL, owner/name#N, or #N in repo (the Ticket's repository).
func ThreadArg(raw, repo string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("name a thread")
	}
	if u, err := url.Parse(s); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if !strings.EqualFold(u.Hostname(), githubHost) || len(parts) < 4 ||
			(parts[2] != "issues" && parts[2] != "pull" && parts[2] != "pulls") {
			return "", fmt.Errorf("%q is not a GitHub issue or pull request URL", s)
		}
		return githubIssueKey(parts[0]+"/"+parts[1], parts[3])
	}
	rest, isGitHub := strings.CutPrefix(s, github.SourceName+":")
	if !isGitHub && strings.Contains(s, ":") {
		if src, key, _ := strings.Cut(s, ":"); src == "" || key == "" {
			return "", fmt.Errorf("thread key %q is not <source>:<key>", s)
		}
		return s, nil
	}
	if r, branch, ok := strings.Cut(rest, "@"); ok && !strings.Contains(rest, "#") {
		if _, _, err := splitRepo(r); err != nil || branch == "" {
			return "", fmt.Errorf("thread %q is not owner/name@branch", s)
		}
		return github.BranchThreadKey(r, branch), nil
	}
	r, num, ok := strings.Cut(rest, "#")
	if !ok {
		return "", fmt.Errorf("thread %q is not a key, a URL, owner/name#N or #N", s)
	}
	if r == "" {
		if repo == "" {
			return "", fmt.Errorf("thread %q names no repository, and the ticket works in none", s)
		}
		r = repo
	}
	return githubIssueKey(r, num)
}

func githubIssueKey(repo, num string) (string, error) {
	if _, _, err := splitRepo(repo); err != nil {
		return "", err
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return "", fmt.Errorf("%q is not an issue or pull request number", num)
	}
	return github.ThreadKey(repo, n), nil
}

// headerLike matches a line that reads like one Athena writes over an Input: an Input header ("[input …]", or any "["
// line) or a card line ("from: …").
var headerLike = regexp.MustCompile(`^(\[|(trigger_id|acknowledged|from|at|thread|url|replies_to|context|routing note):)`)

// quoteText makes text the node did not write (a comment, a Trigger's text, the owner's text, a note) safe to place
// under a header: each line of it that starts like a header or a card line gets one leading space. Athena's own headers
// and card lines start at a line's first column, so text can never carry one (prompts/worker.md tells the worker, and
// internal/render reads it the same way and removes the space for display). Every line separator is a new line first.
func quoteText(s string) string {
	lines := strings.Split(newLines(s), "\n")
	for i, l := range lines {
		if headerLike.MatchString(l) {
			lines[i] = " " + l
		}
	}
	return strings.Join(lines, "\n")
}

// oneLine is s with its line breaks as spaces: a title placed in a header line stays in it.
func oneLine(s string) string {
	return strings.ReplaceAll(newLines(s), "\n", " ")
}

// lineBreaks are the separators a reader (the worker's model among them) may take for the end of a line: CRLF, a lone
// CR, VT, FF, NEL, U+2028 and U+2029, besides LF.
var lineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n")

// newLines is s with every line separator made a \n, so each line of it can be seen, and quoted, as one.
func newLines(s string) string { return lineBreaks.Replace(s) }

// cardTextMaxRunes bounds the text of a Trigger card (reference 007 "Messages delivered to Sessions").
const cardTextMaxRunes = 600

// inputCard renders an Input of Ticket ticketID whose Trigger came from a Source's Thread: who wrote it and when,
// whether it is acknowledged, its Thread, its URL, what it replies to and its text, cut at cardTextMaxRunes with a
// pointer to the rest.
func inputCard(ticketID string, in core.Input, tr core.Trigger) string {
	var b strings.Builder
	what := "message"
	if tr.Kind != "" && tr.Kind != core.KindMessage {
		what = tr.Kind
	}
	if in.Human {
		what += " from a person"
	}
	fmt.Fprintf(&b, "[input %s: %s]\n", in.ID, what)
	fmt.Fprintf(&b, "trigger_id: %s\n", tr.ID)
	if line := ackLine(ticketID, in, tr); line != "" {
		b.WriteString(line + "\n")
	}
	from := tr.Actor.ID
	if tr.Bot {
		from += " (bot)"
	}
	fmt.Fprintf(&b, "from: %s\n", from)
	fmt.Fprintf(&b, "at: %s\n", tr.At.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "thread: %s (%s)\n", threadLabel(tr), tr.Threads[0])
	u := tr.Meta["url"]
	if u != "" {
		fmt.Fprintf(&b, "url: %s\n", u)
	}
	if tr.ReplyTo != "" {
		fmt.Fprintf(&b, "replies_to: %s\n", tr.ReplyTo)
	}
	if in.Context {
		b.WriteString("context: it reached you on a thread you follow for context; it is not a request to this ticket\n")
	}
	if in.Note != "" {
		fmt.Fprintf(&b, "routing note: %s\n", quoteText(in.Note))
	}
	text := in.Text
	if text == "" {
		text = tr.Text
	}
	if r := []rune(text); len(r) > cardTextMaxRunes {
		where := "with gh on " + threadLabel(tr)
		if u != "" {
			where = "at " + u + " (gh can read it)"
		}
		text = quoteText(string(r[:cardTextMaxRunes])) + fmt.Sprintf("…\n[cut: %d of %d characters shown; read the rest %s]", cardTextMaxRunes, len(r), where)
	} else {
		text = quoteText(text)
	}
	b.WriteString(text + "\n\n")
	return b.String()
}

// ackLine tells the worker whether a person's message still waits for its acknowledgement: Athena may have posted one
// when it accepted the Ticket. A bot's message, or one that reached the Ticket only for context, gets none.
func ackLine(ticketID string, in core.Input, tr core.Trigger) string {
	switch {
	case !tr.Human || in.Context:
		return ""
	case slices.Contains(tr.Disposition.Acknowledged, ticketID):
		return "acknowledged: yes, already on its thread; do not ack it again"
	}
	return "acknowledged: no; ack it before substantial work if it is a request you take up"
}

// threadLabel names a Trigger's primary Thread for a person: owner/name#N for a GitHub Issue or pull request, else
// the Thread's key without its Source.
func threadLabel(tr core.Trigger) string {
	if repo, num := tr.Meta["repo"], tr.Meta["number"]; repo != "" && num != "" {
		return repo + "#" + num
	}
	_, key, ok := strings.Cut(tr.Threads[0], ":")
	if !ok {
		return tr.Threads[0]
	}
	return key
}
