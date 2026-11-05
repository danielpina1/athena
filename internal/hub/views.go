package hub

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/redact"
)

// The mapping of the core model to the wire's views. A view carries what a list needs and never the Channel
// addresses, Trigger Meta or tokens the model holds.

const (
	// previewRunes cuts a Trigger's text and a Reason's values in a list.
	previewRunes = 80
	reasonRunes  = 200
	// timelineRunes cuts the text of a timeline entry.
	timelineRunes = 500
	// inputRunes cuts an Approval's redacted tool input.
	inputRunes = 8192
)

// clip cuts s to n runes, with an ellipsis when it cut.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// oneLine flattens whitespace so a preview is one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func tsp(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return ts(*t)
}

var githubPullRe = regexp.MustCompile(`^github:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([0-9]+)$`)

// prURL is the URL of the pull request the Ticket opened itself (an own Follow on a github:owner/name#n Thread), or "".
func prURL(t *core.Ticket) string {
	for _, f := range t.Follows {
		if !f.Own {
			continue
		}
		if m := githubPullRe.FindStringSubmatch(f.Thread); m != nil {
			return "https://github.com/" + m[1] + "/pull/" + m[2]
		}
	}
	return ""
}

// defaultRuntime is the runtime of a Ticket whose Execution does not name one: Claude Code is the only one.
const defaultRuntime = "claude"

// ticketSummary maps a Ticket; gitFolder tells its branch (see node.WorktreeBranch; nil: no folder has a .git entry).
func ticketSummary(t *core.Ticket, st core.Stage, gitFolder func(dir string) bool) *athenav1.TicketSummary {
	v := &athenav1.TicketSummary{
		Id: t.ID, Title: t.Title, Stage: st.Stage, Action: st.Action, Repo: t.Repo,
		CreatedAt: ts(t.CreatedAt), UpdatedAt: ts(t.UpdatedAt),
		NeedsYou: st.Stage == "person", CanRetry: core.CanRetry(t),
		PendingApprovalId: t.PendingApproval, PrUrl: prURL(t),
	}
	v.Runtime = defaultRuntime
	if t.Execution != nil {
		v.Dir, v.Model = t.Execution.Directory, t.Execution.Model
		if t.Execution.Runtime != "" {
			v.Runtime = t.Execution.Runtime
		}
	}
	v.Branch = node.WorktreeBranch(*t, gitFolder)
	if a := t.LatestAssignment(); a != nil && a.Model != "" {
		v.Model = a.Model
	}
	if t.Ended != nil {
		v.EndedAt, v.Outcome = ts(t.Ended.At), string(t.Ended.Outcome)
	}
	if t.Attention != nil {
		v.AttentionQuestion = t.Attention.Question
	}
	return v
}

func approvalView(a *core.Approval) *athenav1.ApprovalView {
	return &athenav1.ApprovalView{
		Id: a.ID, TicketId: a.TicketID, Tool: a.Tool, Summary: a.Summary, Rule: a.Rule,
		// Redact first, then cut: a cut must not split a secret into a piece redact no longer recognizes.
		InputRedacted: clip(redact.String(string(a.Input)), inputRunes),
		State:         a.State, CreatedAt: ts(a.CreatedAt), AnsweredAt: tsp(a.AnsweredAt),
	}
}

func triggerView(t *core.Trigger) *athenav1.TriggerView {
	v := &athenav1.TriggerView{Id: t.ID, At: ts(t.At), Source: t.Source, Actor: t.Actor.ID, Kind: t.Kind,
		Addressed: t.Addressed, Text: clip(oneLine(redact.String(t.Text)), previewRunes)}
	if len(t.Threads) > 0 {
		v.Thread = t.Threads[0]
	}
	d := t.Disposition
	var parts []string
	if d.Filtered != "" {
		parts = append(parts, "filtered:"+d.Filtered)
	}
	if len(d.Reached) > 0 {
		parts = append(parts, "reached:"+strings.Join(d.Reached, ","))
	}
	if len(d.Attached) > 0 {
		parts = append(parts, "attached:"+strings.Join(d.Attached, ","))
	}
	if len(d.HandedBack) > 0 {
		ids := make([]string, 0, len(d.HandedBack))
		for id := range d.HandedBack {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		parts = append(parts, "handedBack:"+strings.Join(ids, ","))
	}
	if d.Requeued != nil {
		parts = append(parts, "requeued")
	}
	v.Outcome = strings.Join(parts, " ")
	return v
}

func threadView(th *core.Thread) *athenav1.ThreadView {
	v := &athenav1.ThreadView{Key: th.Key, Queued: uint32(len(th.Queued)), Decisions: uint32(len(th.Decisions))}
	if ev := th.Eval; ev != nil {
		v.Eval = &athenav1.EvalView{Id: ev.ID, Strength: ev.Strength, StartedAt: ts(ev.StartedAt), DeliveredAt: tsp(ev.DeliveredAt)}
	}
	for _, s := range th.Subscriptions {
		var flags []string
		if s.Own {
			flags = append(flags, "own")
		}
		if s.Bound {
			flags = append(flags, "bound")
		}
		if s.Context {
			flags = append(flags, "context")
		}
		id := s.TicketID
		if len(flags) > 0 {
			id += "(" + strings.Join(flags, ",") + ")"
		}
		v.Tickets = append(v.Tickets, id)
	}
	if th.Closed != nil {
		v.Closed = string(th.Closed.Outcome)
	}
	return v
}

func decisionView(d *core.Decision) *athenav1.DecisionView {
	v := &athenav1.DecisionView{Id: d.ID, At: ts(d.At), Kind: d.Kind, Thread: d.Thread, TicketId: d.TicketID,
		Triggers: slices.Clone(d.TriggerIDs), WouldAccept: d.WouldAccept, DismissedAt: tsp(d.Dismissed)}
	for _, r := range d.Reasons {
		rv := &athenav1.Reason{Code: r.Code}
		if len(r.Values) > 0 {
			rv.Values = make(map[string]string, len(r.Values))
			for k, val := range r.Values {
				rv.Values[k] = clip(redact.String(val), reasonRunes)
			}
		}
		v.Reasons = append(v.Reasons, rv)
	}
	return v
}

func nodeStatus(s node.Status) *athenav1.NodeStatus {
	return &athenav1.NodeStatus{Name: s.Name, Slots: uint32(s.Slots), SlotsInUse: uint32(s.SlotsInUse),
		LiveTicketIds: slices.Clone(s.LiveTicketIDs), Queued: uint32(s.Queued), ClaudeVersion: s.ClaudeVersion}
}

// timelineEntry builds one entry; its text is redacted, flattened and cut.
func timelineEntry(at time.Time, kind, text, ref string) *athenav1.TimelineEntry {
	return &athenav1.TimelineEntry{At: ts(at), Kind: kind, Text: clip(oneLine(redact.String(text)), timelineRunes), RefId: ref}
}

// ticketTimeline lists what happened to a Ticket, oldest first, from the Ticket itself plus its origin Trigger and
// its Approvals (the reads the caller makes: the pending one and the answered ones). A person's message that reached
// the Ticket (the owner's reply or a person's on a Channel) is a reply; any other Input is an input.
func ticketTimeline(t *core.Ticket, origin *core.Trigger, pending *core.Approval, answered []core.Approval) []*athenav1.TimelineEntry {
	var out []*athenav1.TimelineEntry
	if origin != nil {
		out = append(out, timelineEntry(origin.At, "trigger", origin.Text, origin.ID))
	}
	if t.DecisionID != "" {
		out = append(out, timelineEntry(t.CreatedAt, "decision", "accepted", t.DecisionID))
	}
	for _, a := range t.Assignments {
		text := string(a.State)
		if a.EndedReason != "" {
			text += ": " + a.EndedReason
		}
		out = append(out, timelineEntry(a.CreatedAt, "assignment", text, a.ID))
		if a.Report != nil {
			out = append(out, timelineEntry(a.Report.At, "report", string(a.Report.State)+": "+a.Report.Summary, a.ID))
		}
	}
	for _, in := range t.Inputs {
		text := in.Text
		if text == "" {
			text = in.Note
		}
		if text == "" {
			text = string(in.Kind)
		}
		kind := "input"
		if in.Human && !in.Context && in.Kind != core.InputNote && in.Kind != core.InputResumed {
			kind = "reply"
		}
		out = append(out, timelineEntry(in.At, kind, text, in.ID))
	}
	for i := range answered {
		a := &answered[i]
		verdict := "Approved"
		if a.State == core.ApprovalDenied {
			verdict = "Denied"
		} else if a.State != core.ApprovalApproved {
			verdict = "Expired"
		}
		text := verdict + ": " + a.Tool
		if a.Answer != "" {
			text += " (" + a.Answer + ")"
		}
		at := a.CreatedAt
		if a.AnsweredAt != nil {
			at = *a.AnsweredAt
		}
		out = append(out, timelineEntry(at, "approval", text, a.ID))
	}
	if pending != nil {
		out = append(out, timelineEntry(pending.CreatedAt, "approval", pending.Tool+": "+pending.Summary, pending.ID))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.AsTime().Before(out[j].At.AsTime()) })
	return out
}

func ticketDetail(t *core.Ticket, st core.Stage, gitFolder func(string) bool, origin *core.Trigger, pending *core.Approval,
	answered []core.Approval) *athenav1.TicketDetail {
	d := &athenav1.TicketDetail{Summary: ticketSummary(t, st, gitFolder), Timeline: ticketTimeline(t, origin, pending, answered)}
	for _, f := range t.Follows {
		d.Threads = append(d.Threads, f.Thread)
	}
	return d
}
