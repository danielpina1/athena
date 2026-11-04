package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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

// OpWorker runs one of a worker's tools other than report.
const OpWorker = "worker"

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
		"to Save.\"). It is posted as a reply to their message, once per message. Do not acknowledge bots, CI, thanks, " +
		"ordinary discussion or messages you hand back, and do not repeat or quote the message.",
		obj([]string{"trigger_id", "text"}, map[string]any{
			"trigger_id": sprop("The trigger_id of the person's message, from its input card."),
			"text":       sprop("The acknowledgement, at most 100 words."),
		})},
	{"reply", "Send a message to your ticket's conversation. Use purpose answer to answer a person's explicit question " +
		"(naming its trigger_id); Athena delivers results and questions for people from your reports, so result and " +
		"attention are rarely yours to send. Never use it for progress or to announce an unfinished pull request. At most " +
		"4,000 characters; GitHub comments support Markdown.",
		obj([]string{"purpose", "text"}, map[string]any{
			"purpose":    enum("answer, result or attention.", core.PurposeAnswer, core.PurposeResult, core.PurposeAttention),
			"text":       sprop("The message."),
			"trigger_id": sprop("answer: the person's message it answers; otherwise optionally a message that reached the ticket, to reply where it was written."),
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

// WorkerTools returns the names of a worker's tools, report first, in the order the server lists them.
func WorkerTools() []string {
	out := []string{"report"}
	for _, t := range workerTools {
		out = append(out, t.name)
	}
	return out
}

// addWorkerTools adds the forwarding tools to a session's MCP server.
func addWorkerTools(s *mcp.Server, ticketID, assignmentID, socket string) {
	for _, t := range workerTools {
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
			return nil, errors.New("trigger_id is required")
		}
		id, err := eng.Reply(core.ReplyRequest{TicketID: s.t.ID, TriggerID: in.TriggerID, Purpose: core.PurposeAcknowledgement, Text: in.Text})
		if err != nil {
			return nil, err
		}
		return map[string]any{"queued": true, "receipt": id}, nil
	case "reply":
		var in struct {
			Purpose   string `json:"purpose"`
			Text      string `json:"text"`
			TriggerID string `json:"trigger_id,omitempty"`
		}
		if err := decodeArgs(raw, &in); err != nil {
			return nil, err
		}
		if in.Purpose == core.PurposeAcknowledgement {
			return nil, errors.New("acknowledge with the ack tool")
		}
		id, err := eng.Reply(core.ReplyRequest{TicketID: s.t.ID, TriggerID: in.TriggerID, Purpose: in.Purpose, Text: in.Text})
		if err != nil {
			return nil, err
		}
		return map[string]any{"queued": true, "receipt": id}, nil
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

// cardTextMaxRunes bounds the text of a Trigger card (reference 007 "Messages delivered to Sessions").
const cardTextMaxRunes = 600

// inputCard renders an Input whose Trigger came from a Source's Thread: who wrote it and when, its Thread, its URL,
// what it replies to and its text, cut at cardTextMaxRunes with a pointer to the rest.
func inputCard(in core.Input, tr core.Trigger) string {
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
		fmt.Fprintf(&b, "routing note: %s\n", in.Note)
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
		text = string(r[:cardTextMaxRunes]) + fmt.Sprintf("…\n[cut: %d of %d characters shown; read the rest %s]", cardTextMaxRunes, len(r), where)
	}
	b.WriteString(text + "\n\n")
	return b.String()
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
