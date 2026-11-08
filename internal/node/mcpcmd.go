package node

import (
	"context"
	"errors"
	"os"

	"github.com/danielpina1/athena/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// reportArgs are the report tool's arguments.
type reportArgs struct {
	Status   string `json:"status"`
	Summary  string `json:"summary"`
	Question string `json:"question,omitempty"`
	Pull     int    `json:"pull,omitempty"`
}

var reportStates = []any{
	string(core.ReportProgress), string(core.ReportWaiting), string(core.ReportReady),
	string(core.ReportBlocked), string(core.ReportFailed), string(core.ReportDropped),
}

const reportDescription = "File a Report on your Ticket; reports are how your work reaches people. The order: " +
	"progress reports while you work (with pull as soon as you open or adopt the ticket's pull request), then one " +
	"final report: ready when the work is done and verified (with pull when there is a pull request), else blocked, " +
	"failed or dropped. Athena posts the result to people from the ready or dropped report's summary: send no result " +
	"reply. progress: an internal update while you keep working. " +
	"waiting: your turn ended and you wait for a message, with nothing for a person to do, or with a question. " +
	"ready: the work is done and verified; this ends the Ticket, or, with a pull request, waits for its review and " +
	"merge. It cannot carry a question: mention optional follow-ups in summary. " +
	"blocked: a person must act before work can go on. failed: the work cannot be done. " +
	"dropped: nothing is left to do and nobody needs to act; this closes the Ticket. " +
	"Put a question for a person in question (with waiting, blocked or progress), not only in summary; Athena asks it. " +
	"pull: the number of the pull request you opened or adopted for this ticket in its repository; Athena then follows it, " +
	"and the ticket ends when it is merged or closed. Omit it when there is none; never use a placeholder. " +
	"File a ready report in a step of its own, after you have seen your other calls' results."

// reportFiled is the report tool's result: what Athena does with the Report next.
func reportFiled(status string) string {
	switch core.ReportState(status) {
	case core.ReportReady:
		return "Report filed (ready). Athena posts the result to people from this summary; do not send it again with " +
			"reply. With a pull request, the ticket waits for its review and merge, and a review or comment resumes you."
	case core.ReportDropped:
		return "Report filed (dropped). Athena closes the ticket and tells people from this summary; send nothing more."
	}
	return "Report filed (" + status + ")."
}

// RunMCP implements `athena mcp --ticket <id>`: a stdio MCP server for one session, exposing the report tool and the
// worker tools the node names for the session (workertools.go), which forward to the node socket. The session's Assignment comes from
// ATHENA_ASSIGNMENT, set by the node.
func RunMCP(ticketID, socket string) error {
	if ticketID == "" {
		return errors.New("athena mcp: --ticket is required")
	}
	if socket == "" {
		return errors.New("athena mcp: ATHENA_SOCKET is not set")
	}
	return newMCPServer(ticketID, os.Getenv("ATHENA_ASSIGNMENT"), socket).Run(context.Background(), &mcp.StdioTransport{})
}

// newMCPServer returns the MCP server of one session.
func newMCPServer(ticketID, assignmentID, socket string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "athena", Version: "1"}, nil)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status":   map[string]any{"type": "string", "enum": reportStates, "description": "The Report's state."},
			"summary":  map[string]any{"type": "string", "description": "What happened: changes, checks actually run, risks, where the result is."},
			"question": map[string]any{"type": "string", "description": "One concrete question for a person, if one must decide; with waiting, blocked or progress, never ready."},
			"pull":     map[string]any{"type": "integer", "minimum": 1, "description": "Your ticket's pull request number, once it exists."},
		},
		"required":             []any{"status", "summary"},
		"additionalProperties": false,
	}
	mcp.AddTool(s, &mcp.Tool{Name: "report", Description: reportDescription, InputSchema: schema},
		func(ctx context.Context, _ *mcp.CallToolRequest, in reportArgs) (*mcp.CallToolResult, any, error) {
			var resp Response
			err := callNode(socket, ReportRequest{
				Op: OpReport, Ticket: ticketID, Assignment: assignmentID,
				Status: in.Status, Summary: in.Summary, Question: in.Question, Pull: in.Pull,
			}, &resp)
			switch {
			case err != nil:
				return toolError("the athena node is unreachable: " + err.Error()), nil, nil
			case !resp.OK:
				return toolError("report refused: " + resp.Error), nil, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: reportFiled(in.Status)}}}, nil, nil
		})
	addWorkerTools(s, ticketID, assignmentID, socket, sessionWorkerTools(ticketID, assignmentID, socket))
	return s
}

func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// reviewArgs are the review tool's arguments.
type reviewArgs struct {
	Verdict  string `json:"verdict"`
	Findings string `json:"findings"`
}

const reviewDescription = "File your review of the Ticket's branch, once, when you are done: the verdict and your findings. " +
	"findings: one per line, most serious first, each \"- [high|medium|low] <file>:<line> — <what is wrong and why>\", or " +
	"the one line \"- none\". verdict: changes when any finding is high or medium, approved otherwise; \"- none\" is " +
	"approved. The findings go to the implementer as they are. A verdict or findings that break these rules are refused: " +
	"correct them and call again."

// RunReviewMCP implements `athena mcp --ticket <id> --role review`: the stdio MCP server of a review session, with
// only the review tool. The session's Assignment comes from ATHENA_ASSIGNMENT, set by the node.
func RunReviewMCP(ticketID, socket string) error {
	if ticketID == "" {
		return errors.New("athena mcp: --ticket is required")
	}
	if socket == "" {
		return errors.New("athena mcp: ATHENA_SOCKET is not set")
	}
	return newReviewMCPServer(ticketID, os.Getenv("ATHENA_ASSIGNMENT"), socket).Run(context.Background(), &mcp.StdioTransport{})
}

// newReviewMCPServer returns the MCP server of one review session: the review tool, which forwards the verdict to the
// node socket (OpReview); the Engine checks it (core.CheckReview) and its refusal is the tool's error.
func newReviewMCPServer(ticketID, assignmentID, socket string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "athena", Version: "1"}, nil)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"verdict":  map[string]any{"type": "string", "enum": []any{core.ReviewApproved, core.ReviewChanges}, "description": "approved, or changes when any finding is high or medium."},
			"findings": map[string]any{"type": "string", "description": "One finding per line, \"- [high|medium|low] <file>:<line> — <what>\", or \"- none\"."},
		},
		"required":             []any{"verdict", "findings"},
		"additionalProperties": false,
	}
	mcp.AddTool(s, &mcp.Tool{Name: "review", Description: reviewDescription, InputSchema: schema},
		func(ctx context.Context, _ *mcp.CallToolRequest, in reviewArgs) (*mcp.CallToolResult, any, error) {
			var resp Response
			err := callNode(socket, ReviewRequest{Op: OpReview, Ticket: ticketID, Assignment: assignmentID,
				Verdict: in.Verdict, Findings: in.Findings}, &resp)
			switch {
			case err != nil:
				return toolError("the athena node is unreachable: " + err.Error()), nil, nil
			case !resp.OK:
				return toolError("review refused: " + resp.Error), nil, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
				Text: "Review filed (" + in.Verdict + "). Your review is done: end your turn."}}}, nil, nil
		})
	return s
}
