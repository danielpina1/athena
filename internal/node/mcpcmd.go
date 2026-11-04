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

const reportDescription = "File a Report on your Ticket. progress: an internal update while you keep working. " +
	"waiting: your turn ended and you wait for a message, with nothing for a person to do. " +
	"ready: the work is done and verified; this ends the Ticket. " +
	"blocked: a person must act before work can go on. failed: the work cannot be done. " +
	"dropped: nothing is left to do and nobody needs to act; this closes the Ticket. " +
	"Put a question for a person in question, not only in summary. " +
	"pull: the number of the pull request you opened or adopted for this ticket in its repository; Athena then follows it, " +
	"and the ticket ends when it is merged or closed. Omit it when there is none; never use a placeholder."

// RunMCP implements `athena mcp --ticket <id>`: a stdio MCP server for one session, exposing the report tool and the
// worker tools (workertools.go), which forward to the node socket. The session's Assignment comes from
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
			"question": map[string]any{"type": "string", "description": "One concrete question for a person, if one must decide."},
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
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Report filed (" + in.Status + ")."}}}, nil, nil
		})
	addWorkerTools(s, ticketID, assignmentID, socket)
	return s
}

func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}
