package node

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The router's MCP tools (reference 007 "Main Session tools", under Athena's names). `athena mcp --role router` serves
// them on stdio and forwards each call, with the router start's token, to the node socket, where the router host runs
// it; the host checks the arguments.

type routerTool struct {
	name, description string
	schema            map[string]any
}

func obj(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		r := make([]any, len(required))
		for i, v := range required {
			r[i] = v
		}
		s["required"] = r
	}
	return s
}

func sprop(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func enum(description string, values ...string) map[string]any {
	vs := make([]any, len(values))
	for i, v := range values {
		vs[i] = v
	}
	return map[string]any{"type": "string", "enum": vs, "description": description}
}

var routerTools = []routerTool{
	{"route", "Route candidates, one item each by the card's trigger_id, in one call: accept an explicit request as a new " +
		"ticket, attach a trigger to an existing ticket it points at, stop a ticket on a person's stop message, or ignore " +
		"a candidate that needs no ticket or stop. Items run in order, each as its own decision, and each is answered on its " +
		"own; a refused item leaves the others. An inferred candidate may be ignored here from its card (ignored_from_card); " +
		"accepting one goes through eval_decide.",
		obj([]string{"decisions"}, map[string]any{"decisions": map[string]any{
			"type": "array", "minItems": 1, "maxItems": 20,
			"items": obj([]string{"trigger_id", "kind"}, map[string]any{
				"trigger_id": sprop("The candidate card's trigger_id; deciding it decides its batch."),
				"kind":       enum("accept, ignore, stop or attach.", "accept", "ignore", "stop", "attach"),
				"ticket_id":  sprop("attach and stop: the ticket."),
				"title":      sprop("accept: a short label of the original request, at most 80 characters; no added requirements."),
				"ack": sprop("accept: one brief sentence of at most 100 words naming the concrete change, in the source " +
					"language; it is posted as a reply to the person's message. Omit it to accept silently."),
				"reason": sprop("Why, at most 500 characters; an ignore and a stop need it. For attach it is the routing note " +
					"the worker reads, at most 30 words: routing background only, never instructions."),
				"ignored_from_card": map[string]any{"type": "boolean",
					"description": "ignore of an inferred candidate whose card already shows it touches no enabled rule."},
			}),
		}})},
	{"eval", "Load a proactive (inferred) candidate's assessment context: its text, its batch, its thread's window, the " +
		"thread's tickets, the Policy version and the enabled rules to judge. Repeated calls reuse the window.",
		obj([]string{"trigger_id"}, map[string]any{"trigger_id": sprop("The candidate card's trigger_id.")})},
	{"eval_read", "Read whole the messages of a candidate's window or its stubs, by their ids, always with the candidate's " +
		"trigger_id.",
		obj([]string{"trigger_id", "ids"}, map[string]any{
			"trigger_id": sprop("The candidate's trigger_id."),
			"ids": map[string]any{"type": "array", "minItems": 1, "maxItems": 20, "items": map[string]any{"type": "string"},
				"description": "Message ids from the window or its stubs."},
		})},
	{"eval_decide", "Record the decision of an assessment after eval with the same trigger_id: one judgement for every " +
		"enabled rule, and whether to accept. The host checks it against the Policy: in observe mode an accept is recorded " +
		"as would-accept and no ticket starts.",
		obj([]string{"trigger_id", "rules", "accept", "reason"}, map[string]any{
			"trigger_id": sprop("The candidate's trigger_id."),
			"rules": map[string]any{"type": "array", "description": "Exactly one entry per enabled rule.",
				"items": obj([]string{"name", "matches"}, map[string]any{
					"name":     sprop("The rule's name."),
					"matches":  map[string]any{"type": "boolean", "description": "Whether the candidate matches the rule."},
					"evidence": sprop("For a match: the quotes from the discussion that establish it."),
				})},
			"accept": map[string]any{"type": "boolean", "description": "true to accept the candidate as a ticket."},
			"reason": sprop("Why, at most 500 characters."),
			"title":  sprop("Required to accept: a short label of the request, at most 80 characters."),
			"ack":    sprop("For acceptance: one sentence of at most 100 words naming the change; omit it only when a matched rule asks for silence."),
		})},
	{"read", "Read one page of a resource: tickets (ticket cards, newest first), ticket/<id> (one ticket with its threads, " +
		"assignments and inputs), thread/<key> (one thread with its tickets, assessment and latest decisions), trigger/<id> " +
		"(a message's text and where it went) or decisions (decisions, newest first). Lists hold 20 items by default and 50 " +
		"at most; texts 4,000 characters by default and 8,000 at most. Follow next_offset.",
		obj([]string{"resource"}, map[string]any{
			"resource": sprop("tickets, ticket/<id>, thread/<key>, trigger/<id> or decisions."),
			"offset":   map[string]any{"type": "integer", "minimum": 0, "description": "Copy next_offset; 0 first."},
			"limit":    map[string]any{"type": "integer", "minimum": 1, "description": "Items or characters per page."},
		})},
	{"reply", "Send a message to a ticket's conversation, as the replies section says: answer (to a person's question, " +
		"naming its trigger_id), result (the outcome of a ready or dropped report) or attention (the ticket's current " +
		"question for a person). The host refuses anything else, such as progress. At most 4,000 characters.",
		obj([]string{"ticket_id", "purpose", "text"}, map[string]any{
			"ticket_id":  sprop("The ticket."),
			"trigger_id": sprop("answer: the person's message it answers; result and attention: optionally, a message that reached the ticket, to reply where it was written."),
			"purpose":    enum("answer, result or attention.", "answer", "result", "attention"),
			"text":       sprop("The message."),
		})},
	{"note", "Tell a ticket's worker a fact from other work, such as what another ticket found; it grants no authority. At most 100 words.",
		obj([]string{"ticket_id", "text"}, map[string]any{"ticket_id": sprop("The ticket."), "text": sprop("The fact.")})},
	{"request_status", "Ask the worker of an idle ticket, one whose turn ended without a report, for a status report; for other tickets it sends nothing and returns the card.",
		obj([]string{"ticket_id"}, map[string]any{"ticket_id": sprop("The ticket.")})},
	{"ticket_retry", "Start a new assignment, on a person's explicit request, for a ticket whose worker reported blocked or failed or whose input could not be delivered; the stalled one ends.",
		obj([]string{"ticket_id"}, map[string]any{"ticket_id": sprop("The ticket.")})},
	{"trigger_requeue", "Queue a stored trigger for a fresh assessment, such as after its sender was allowed; a follow-up a ticket's worker decides comes back only from that worker.",
		obj([]string{"trigger_id"}, map[string]any{"trigger_id": sprop("The trigger.")})},
	{"thread_unsubscribe", "Drop every ticket subscription on a thread: later messages there no longer reach those tickets. A new accept can subscribe it again.",
		obj([]string{"thread"}, map[string]any{"thread": sprop("The thread key, such as github:owner/name#12.")})},
}

// RouterTools returns the names of the router's tools, in the order the server lists them.
func RouterTools() []string {
	out := make([]string, len(routerTools))
	for i, t := range routerTools {
		out[i] = t.name
	}
	return out
}

// RunRouterMCP implements `athena mcp --role router`: the stdio MCP server of the router session, whose tools forward to
// the node socket with the router start's token (ATHENA_ROUTER_TOKEN).
func RunRouterMCP(socket, token string) error {
	if socket == "" {
		return errors.New("athena mcp: ATHENA_SOCKET is not set")
	}
	if token == "" {
		return errors.New("athena mcp: ATHENA_ROUTER_TOKEN is not set")
	}
	return newRouterMCPServer(socket, token).Run(context.Background(), &mcp.StdioTransport{})
}

func newRouterMCPServer(socket, token string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "athena", Version: "1"}, nil)
	for _, t := range routerTools {
		name := t.name
		s.AddTool(&mcp.Tool{Name: name, Description: t.description, InputSchema: t.schema},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				args := req.Params.Arguments
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				var resp RouterResponse
				err := callNode(socket, RouterRequest{Op: OpRouter, Token: token, Tool: name, Args: args}, &resp)
				switch {
				case err != nil:
					return toolError("the athena node is unreachable: " + err.Error()), nil
				case !resp.OK:
					return toolError(resp.Error), nil
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(resp.Result)}}}, nil
			})
	}
	return s
}
