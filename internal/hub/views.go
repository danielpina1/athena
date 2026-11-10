package hub

import (
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
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

// defaultRuntime is the runtime of a Ticket whose Execution does not name one (from before runtimes): Claude Code.
const defaultRuntime = core.RuntimeClaude

// WhyMaxRunes cuts a runtime's why (RuntimeStatus.why) on the wire.
const WhyMaxRunes = 200

// runtimeChoice is what the Ticket asked for: claude, codex or auto.
func runtimeChoice(t *core.Ticket) string {
	if x := t.Execution; x != nil && x.Runtime != "" {
		return x.Runtime
	}
	return defaultRuntime
}

// runtimeOf is the runtime of the latest Assignment that started (a review's too), else the Ticket's choice.
func runtimeOf(t *core.Ticket) string {
	for i := len(t.Assignments) - 1; i >= 0; i-- {
		if a := &t.Assignments[i]; a.StartedAt != nil && a.Runtime != "" {
			return a.Runtime
		}
	}
	return runtimeChoice(t)
}

// ticketSummary maps a Ticket; gitFolder tells its branch (see node.WorktreeBranch; nil: no folder has a .git entry).
func ticketSummary(t *core.Ticket, st core.Stage, gitFolder func(dir string) bool) *athenav1.TicketSummary {
	v := &athenav1.TicketSummary{
		Id: t.ID, Title: t.Title, Stage: st.Stage, Action: st.Action, Repo: t.Repo,
		CreatedAt: ts(t.CreatedAt), UpdatedAt: ts(t.UpdatedAt),
		NeedsYou: st.Stage == "person", CanRetry: core.CanRetry(t),
		PendingApprovalId: t.PendingApproval, PrUrl: prURL(t),
	}
	v.Runtime, v.RuntimeChoice = runtimeOf(t), runtimeChoice(t)
	v.Pairing, v.ReviewRounds = t.Pairing, uint32(max(t.Stats.ReviewRounds, 0))
	if t.Execution != nil {
		v.Dir, v.Model = t.Execution.Directory, t.Execution.Model
	}
	v.Trust = t.Mandate.TrustClass()
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

// triggerView maps a Trigger; trust tells the sender's trust class under the current Policy (nil: none).
func triggerView(t *core.Trigger, trust func(core.Trigger) string) *athenav1.TriggerView {
	v := &athenav1.TriggerView{Id: t.ID, At: ts(t.At), Source: t.Source, Actor: t.Actor.ID, Kind: t.Kind,
		Addressed: t.Addressed, Text: clip(oneLine(redact.String(t.Text)), previewRunes)}
	if trust != nil {
		v.Trust = trust(*t)
	}
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
	v := &athenav1.NodeStatus{Name: s.Name, Slots: uint32(s.Slots), SlotsInUse: uint32(s.SlotsInUse),
		LiveTicketIds: slices.Clone(s.LiveTicketIDs), Queued: uint32(s.Queued), ClaudeVersion: s.ClaudeVersion,
		Id: "local", State: "online", Os: runtime.GOOS, Arch: runtime.GOARCH, Local: true}
	for _, r := range s.Runtimes {
		v.Runtimes = append(v.Runtimes, runtimeStatus(r))
	}
	return v
}

// runtimeStatus maps one runtime of the node. Its why is redacted, flattened and cut: it may carry what a CLI said.
func runtimeStatus(r node.RuntimeStatus) *athenav1.RuntimeStatus {
	q := r.Quota
	v := &athenav1.RuntimeStatus{Name: r.Name, Version: r.Version, Ready: r.Ready, Tested: r.Tested,
		Why: clip(oneLine(redact.String(r.Why)), WhyMaxRunes), Live: uint32(max(r.Live, 0)),
		LimitedUntil: tsp(q.LimitedUntil), ReadAt: ts(q.ReadAt), Measured: q.Measured}
	for _, m := range r.Models {
		v.Models = append(v.Models, &athenav1.RuntimeModel{Id: m.ID, Name: m.Name, Default: m.Default})
	}
	// A window is named by its bucket too only when the runtime reports several (a Codex per-model limit).
	buckets := map[string]bool{}
	for _, w := range q.Windows {
		if w.Bucket != "" {
			buckets[w.Bucket] = true
		}
	}
	for _, w := range q.Windows {
		name := w.Name
		if w.Bucket != "" && len(buckets) > 1 {
			name = w.Bucket + " " + name
		}
		v.Windows = append(v.Windows, &athenav1.QuotaWindow{Name: name, Used: float32(w.Used), ResetsAt: tsp(w.ResetsAt), Stale: w.Stale})
	}
	return v
}

// athenaMarker matches Athena's hidden markers, such as the receipt line of a comment it posted on GitHub.
var athenaMarker = regexp.MustCompile(`<!--\s*athena:[^>]*?-->`)

// subjectRunes cuts an entry's subject; reportBytes cuts a Report's summary in the detail.
const (
	subjectRunes = 160
	reportBytes  = 16 << 10
	// questionBytes cuts a Report's question in the detail.
	questionBytes = 4 << 10
	// reportsKept is how many Reports a detail carries, the newest.
	reportsKept = 5
)

// clean redacts s and drops Athena's markers.
func clean(s string) string { return athenaMarker.ReplaceAllString(redact.String(s), "") }

// timelineEntry builds one entry; its text and subject are redacted, rid of markers, flattened and cut.
func timelineEntry(at time.Time, kind, subkind, subject, text, ref string) *athenav1.TimelineEntry {
	return &athenav1.TimelineEntry{At: ts(at), Kind: kind, Subkind: subkind, Subject: clip(oneLine(clean(subject)), subjectRunes),
		Text: clip(oneLine(clean(text)), timelineRunes), RefId: ref}
}

// withRuntime sets the runtime of an assignment or review entry.
func withRuntime(e *athenav1.TimelineEntry, runtime string) *athenav1.TimelineEntry {
	e.Runtime = runtime
	return e
}

// firstLine is the first line of s with text on it.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return ""
}

// triggerSource says where a Trigger came from, for "Started from …".
func triggerSource(tr *core.Trigger) string {
	pull := tr.Meta["kind"] == "pull"
	either := func(issue, pr string) string {
		if pull {
			return pr
		}
		return issue
	}
	switch {
	case tr.Source == "owner":
		return "owner"
	case tr.Scope == "ci":
		return "ci"
	case strings.HasPrefix(tr.ID, "github:issue:"):
		return either("issue", "pull")
	case strings.HasPrefix(tr.ID, "github:issuecomment:"):
		return either("issue-comment", "pull-comment")
	case strings.HasPrefix(tr.ID, "github:reviewcomment:"):
		return "review-comment"
	case strings.HasPrefix(tr.ID, "github:review:"):
		return "review"
	}
	return ""
}

// approvalWhat is what an Approval is for, the tool and its command (Bash: npm test): its summary is
// "<why> — <tool>: <what>", or the tool alone.
func approvalWhat(a *core.Approval) string {
	what := a.Summary
	if _, after, ok := strings.Cut(what, " — "); ok {
		what = after
	}
	switch {
	case what == "":
		return a.Tool
	case a.Tool == "" || strings.HasPrefix(what, a.Tool):
		return what
	}
	return a.Tool + ": " + what
}

// approvalWhy is the reason part of an Approval's summary, if it has one.
func approvalWhy(a *core.Approval) string {
	if before, _, ok := strings.Cut(a.Summary, " — "); ok {
		return before
	}
	return ""
}

// assignmentEnd is how an ended Assignment's end reads: its subkind, subject and text. latest says it is the Ticket's
// last, the one a Ticket's end stops. An end at a usage limit is "limited", its subject the runtime and its text the
// RFC 3339 reset (empty when unknown).
func assignmentEnd(t *core.Ticket, a *core.Assignment, latest bool) (sub, subject, text string) {
	reason := a.EndedReason
	switch {
	case reason == core.ReasonUsageLimit:
		// Its runtime, and when the limit resets (empty when unknown).
		runtime := a.Runtime
		if l := a.Limit; l != nil {
			if l.Runtime != "" {
				runtime = l.Runtime
			}
			if l.ResetsAt != nil {
				text = l.ResetsAt.UTC().Format(time.RFC3339)
			}
		}
		return "limited", runtime, text
	case reason == "retried":
		return "retried", "", ""
	case strings.Contains(reason, "restart"):
		return "interrupted", "", reason
	case a.Report != nil && a.Report.State == core.ReportFailed:
		// The failed Report says why on its own row: the end is only the end.
		return "ended", "", ""
	case latest && t.Ended != nil && t.Ended.By == core.EndByHuman:
		return "stopped", "", t.Ended.Reason
	}
	return "ended", "", reason
}

// reviewEntry is the end of a review Assignment: its verdict (approved, changes or failed, the outcome's), the first
// finding as its text (why, for a review that could not finish and says why) and the runtime that reviewed as its subject.
func reviewEntry(at time.Time, a *core.Assignment, o *core.ReviewOutcome) *athenav1.TimelineEntry {
	text := o.First
	if o.Verdict == core.ReviewFailed && o.Why != "" {
		text = firstLine(clean(o.Why))
	}
	return withRuntime(timelineEntry(at, "review", o.Verdict, a.Runtime, text, a.ID), a.Runtime)
}

// headBytes is how much of a Report's summary the timeline reads for its first line: enough for any line it shows.
const headBytes = 8 << 10

// headOf is the start of s, cut on a rune boundary; the timeline's first line comes from it, so a long summary is not
// redacted whole once per detail. A secret the cut splits lies past the first line's 500 runes, which the entry drops.
func headOf(s string) string {
	if len(s) <= headBytes {
		return s
	}
	cut := headBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// originEcho is the index of the Input that is the origin Trigger recorded again, or -1: the first Input on the
// origin Trigger itself, whatever its text or time (a writer's clock may be a moment off); else the first person's
// Input at or after the Trigger, on no other Trigger, with the Trigger's text. Only that one: the owner saying the
// same words again later is a message of its own.
func originEcho(t *core.Ticket, origin *core.Trigger) int {
	if origin == nil {
		return -1
	}
	for i := range t.Inputs {
		if t.Inputs[i].TriggerID == origin.ID {
			return i
		}
	}
	for i := range t.Inputs {
		in := &t.Inputs[i]
		if !in.Human || in.At.Before(origin.At) || (in.TriggerID != "" && in.TriggerID != origin.ID) {
			continue
		}
		if in.Text == origin.Text {
			return i
		}
		return -1
	}
	return -1
}

// ticketTimeline lists what happened to a Ticket, oldest first, from the Ticket itself plus its origin Trigger and
// its Approvals (the reads the caller makes: the pending one and the answered ones). A person's message that reached
// the Ticket (the owner's reply or a person's on a Channel) is a reply; any other Input is an input. actors names the
// GitHub login of a reply's Trigger, by Trigger id, where the caller knows it.
//
// It reads as a story: an Assignment has a start entry, a restart entry for each time its session was cut short (and a
// resumed one when it started again) and, once it ended, an end entry at its end; an Input that is
// the origin Trigger recorded again is left out; an owner's own task has no router Decision. A ready report a review
// holds reads only once applied, on the Assignment that continues the implementer's session, whose start is when an
// Input first reached it (none before).
func ticketTimeline(t *core.Ticket, origin *core.Trigger, pending *core.Approval, answered []core.Approval,
	actors map[string]string) []*athenav1.TimelineEntry {
	var out []*athenav1.TimelineEntry
	if origin != nil {
		out = append(out, timelineEntry(origin.At, "trigger", triggerSource(origin), "", origin.Text, origin.ID))
	}
	if t.DecisionID != "" && (origin == nil || origin.Source != "owner") {
		out = append(out, timelineEntry(t.CreatedAt, "decision", "accepted", "", "", t.DecisionID))
	}
	for i := range t.Assignments {
		a := &t.Assignments[i]
		start, sub := a.CreatedAt, "started"
		if a.StartedAt != nil {
			start = *a.StartedAt
		}
		if a.State == core.AssignmentPending {
			sub = "queued"
		}
		if a.Role == core.RoleReview && sub == "started" {
			sub = "review"
		}
		switch {
		case a.Continues == "":
			out = append(out, withRuntime(timelineEntry(start, "assignment", sub, a.Model, "", a.ID), a.Runtime))
		case a.DeliveredAt != nil:
			// It applied a held report in the implementer's session; that session starts again once an Input reaches it.
			out = append(out, withRuntime(timelineEntry(*a.DeliveredAt, "assignment", sub, a.Model, "", a.ID), a.Runtime))
		}
		if r := a.Report; r != nil && !heldForReview(a) {
			subject := ""
			if r.Pull > 0 {
				subject = "PR #" + strconv.Itoa(r.Pull)
			}
			out = append(out, timelineEntry(r.At, "report", string(r.State), subject, firstLine(clean(headOf(r.Summary))), a.ID))
		}
		for _, in := range a.Interruptions {
			cut, resumed := "restart", "resumed"
			if in.Reason == core.InterruptPaused {
				cut, resumed = "paused", "unpaused"
			}
			out = append(out, withRuntime(timelineEntry(in.At, "assignment", cut, "", "", a.ID), a.Runtime))
			if in.ResumedAt != nil {
				out = append(out, withRuntime(timelineEntry(*in.ResumedAt, "assignment", resumed, "", "", a.ID), a.Runtime))
			}
		}
		if a.State == core.AssignmentEnded && a.EndedAt != nil {
			if o := a.ReviewOutcome; a.Role == core.RoleReview && a.EndedReason == core.ReasonReviewed && o != nil {
				out = append(out, reviewEntry(*a.EndedAt, a, o))
				continue
			}
			sub, subject, text := assignmentEnd(t, a, i == len(t.Assignments)-1)
			out = append(out, withRuntime(timelineEntry(*a.EndedAt, "assignment", sub, subject, text, a.ID), a.Runtime))
		}
	}
	echo := originEcho(t, origin)
	for i, in := range t.Inputs {
		if i == echo {
			continue
		}
		text := in.Text
		if text == "" {
			text = in.Note
		}
		kind, sub, subject := "input", string(in.Kind), ""
		switch {
		case !in.Human && athenaMarker.MatchString(text):
			// Athena's own comment, read back from GitHub; a person who pastes the marker is still a person.
			sub = "receipt"
		case in.Context:
			sub = "context"
		case in.Human && in.Kind != core.InputNote && in.Kind != core.InputResumed:
			kind, sub = "reply", "owner"
			if login, ok := actors[in.TriggerID]; ok || strings.HasPrefix(in.TriggerID, "github:") {
				sub, subject = "github", login
			}
		}
		out = append(out, timelineEntry(in.At, kind, sub, subject, text, in.ID))
	}
	for i := range answered {
		a := &answered[i]
		sub := "expired"
		switch a.State {
		case core.ApprovalApproved:
			sub = "approved"
		case core.ApprovalDenied:
			sub = "denied"
		}
		at := a.CreatedAt
		if a.AnsweredAt != nil {
			at = *a.AnsweredAt
		}
		out = append(out, timelineEntry(at, "approval", sub, approvalWhat(a), a.Answer, a.ID))
	}
	if pending != nil {
		out = append(out, timelineEntry(pending.CreatedAt, "approval", "pending", approvalWhat(pending), approvalWhy(pending), pending.ID))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.AsTime().Before(out[j].At.AsTime()) })
	return out
}

// clipBytes cuts s to at most n bytes on a rune boundary, with an ellipsis when it cut.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// heldForReview reports whether a's Report is a ready report a review held: it is not the Ticket's until the review
// applies it, on the Assignment that continues a (core.Assignment.Continues), which then shows it once.
func heldForReview(a *core.Assignment) bool {
	return a.Role != core.RoleReview && a.EndedReason == core.ReasonReview
}

// ticketReports are the Ticket's Reports in full, newest last, the last reportsKept of them, less a report a review
// holds (heldForReview). The latest one's question is the Ticket's open question when the Report has none of its own
// (a Report stored before Reports kept it).
func ticketReports(t *core.Ticket) []*athenav1.ReportView {
	// The last reportsKept first, so only those are redacted.
	var with []*core.Assignment
	for i := range t.Assignments {
		if a := &t.Assignments[i]; a.Report != nil && !heldForReview(a) {
			with = append(with, a)
		}
	}
	if len(with) > reportsKept {
		with = with[len(with)-reportsKept:]
	}
	out := make([]*athenav1.ReportView, 0, len(with))
	for _, a := range with {
		r := a.Report
		out = append(out, &athenav1.ReportView{AssignmentId: a.ID, State: string(r.State),
			// Redact first, then cut: a cut must not split a secret into a piece redact no longer recognizes.
			Summary: clipBytes(clean(r.Summary), reportBytes), Pull: uint32(max(r.Pull, 0)),
			Question: clipBytes(clean(r.Question), questionBytes), At: ts(r.At)})
	}
	if n := len(out); n > 0 && out[n-1].Question == "" && t.Attention != nil {
		out[n-1].Question = clipBytes(clean(t.Attention.Question), questionBytes)
	}
	return out
}

func ticketDetail(t *core.Ticket, st core.Stage, gitFolder func(string) bool, origin *core.Trigger, pending *core.Approval,
	answered []core.Approval, actors map[string]string) *athenav1.TicketDetail {
	d := &athenav1.TicketDetail{Summary: ticketSummary(t, st, gitFolder), Timeline: ticketTimeline(t, origin, pending, answered, actors),
		Reports: ticketReports(t)}
	for _, f := range t.Follows {
		d.Threads = append(d.Threads, f.Thread)
	}
	return d
}
