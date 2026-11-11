package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/prompts"
)

// Completer runs one model turn: system is the system prompt, user the one message; it returns the reply's text.
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// LLM is the Strategy that asks a model for the split in one turn. The model belongs to the Completer.
type LLM struct{ C Completer }

// Name is "llm".
func (LLM) Name() string { return "llm" }

// Plan asks C for the split under core.PlanTimeout and parses the first JSON object of the reply. A split of 2 or more
// sub-tasks must pass core.CheckSpecs; one of 0 or 1 is returned as is, for the Engine to run as one Ticket. Overlaps
// are left to the Engine.
func (l LLM) Plan(ctx context.Context, r Request) (Breakdown, error) {
	ctx, cancel := context.WithTimeout(ctx, core.PlanTimeout)
	defer cancel()
	text, err := l.C.Complete(ctx, prompts.Planner, userMessage(r))
	if err != nil {
		return Breakdown{}, fmt.Errorf("planner: %w", err)
	}
	rep, err := parseReply(text)
	if err != nil {
		return Breakdown{}, unusable(err)
	}
	tasks := rep.Tasks
	if len(tasks) >= 2 {
		if tasks, err = core.CheckSpecs(tasks, nil, r.MaxTasks); err != nil {
			return Breakdown{}, unusable(err)
		}
	}
	return Breakdown{Tasks: tasks, Note: rep.Note, Strategy: l.Name()}, nil
}

// unusable is the error of a reply that gives no split the Engine can use.
func unusable(why error) error { return fmt.Errorf("the planner's split is not usable: %v", why) }

// userMessage is the one message of a planning turn: the limit, the hints a sub-task may name and the task, quoted
// between <task> tags (the prompt calls it data).
func userMessage(r Request) string {
	list := func(xs []string) string {
		if len(xs) == 0 {
			return "none offered"
		}
		return strings.Join(xs, ", ")
	}
	var b strings.Builder
	b.WriteString("Split the task below into at most " + strconv.Itoa(r.MaxTasks) + " sub-tasks.\n")
	b.WriteString("Runtimes: " + list(r.Runtimes) + "\n")
	b.WriteString("Nodes: " + list(r.Nodes) + "\n")
	if r.Repo != "" {
		b.WriteString("Repository: " + r.Repo + "\n")
	}
	b.WriteString("\n<task>\n")
	b.WriteString(quote(r.Title) + "\n\n" + quote(r.Text) + "\n")
	b.WriteString("</task>\n")
	return b.String()
}

// quote keeps the task's own text from closing or opening the <task> quote.
func quote(s string) string {
	return strings.NewReplacer("</task>", "</ task>", "<task>", "< task>").Replace(strings.TrimSpace(s))
}

// reply is the JSON shape the prompt asks for.
type reply struct {
	Tasks []core.ChildSpec `json:"tasks"`
	Note  string           `json:"note"`
}

// parseReply decodes the first JSON object in text that has a "tasks" field, fenced or bare.
func parseReply(text string) (reply, error) {
	data := []byte(text)
	for i := bytes.IndexByte(data, '{'); i >= 0; {
		var raw map[string]json.RawMessage
		if json.NewDecoder(bytes.NewReader(data[i:])).Decode(&raw) == nil {
			if _, ok := raw["tasks"]; ok {
				var rep reply
				if err := json.NewDecoder(bytes.NewReader(data[i:])).Decode(&rep); err != nil {
					return reply{}, fmt.Errorf("its JSON does not have the asked shape: %v", err)
				}
				return rep, nil
			}
		}
		j := bytes.IndexByte(data[i+1:], '{')
		if j < 0 {
			break
		}
		i += 1 + j
	}
	return reply{}, errors.New("the reply holds no JSON object with tasks")
}
