package hub

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/quota"
	"github.com/danielpina1/athena/internal/runtime/rt"
)

func TestTicketSummaryFlags(t *testing.T) {
	tk := ticket("T-1")
	tk.Attention = &core.Attention{ID: "q", Question: "which branch?"}
	tk.Repo = "o/r"
	tk.Follows = []core.Follow{{Thread: "github:o/other#9"}, {Thread: "github:o/r#12", Own: true}}
	tk.Execution = &core.Execution{Directory: "/work/T-1", Model: "sonnet"}
	st := core.TicketStage(&tk)
	v := ticketSummary(&tk, st, nil)
	if !v.NeedsYou || v.Stage != "person" || v.Action != "answer" || v.AttentionQuestion != "which branch?" {
		t.Errorf("summary = %v", v)
	}
	if v.PrUrl != "https://github.com/o/r/pull/12" || v.Dir != "/work/T-1" || v.Model != "sonnet" || v.Repo != "o/r" {
		t.Errorf("summary = %v", v)
	}
	if v.EndedAt != nil || v.PendingApprovalId != "" {
		t.Errorf("summary = %v", v)
	}

	// A followed (not own) pull request is not the Ticket's PR.
	tk.Follows = []core.Follow{{Thread: "github:o/r#12"}}
	if u := prURL(&tk); u != "" {
		t.Errorf("pr_url of a followed PR = %q", u)
	}
	// A working Ticket does not need you; an ended one carries its outcome.
	w := ticket("T-2")
	w.Assignments = []core.Assignment{{ID: "a", State: core.AssignmentRunning, Model: "opus"}}
	if v := ticketSummary(&w, core.TicketStage(&w), nil); v.NeedsYou || v.Model != "opus" {
		t.Errorf("working summary = %v", v)
	}
	e := ended("T-3", 1)
	if v := ticketSummary(&e, core.TicketStage(&e), nil); v.Outcome != "completed" || v.EndedAt == nil || v.CanRetry {
		t.Errorf("ended summary = %v", v)
	}
}

func TestCanRetryOnDeliveryFailure(t *testing.T) {
	tk := ticket("T-1")
	tk.Delivery = &core.DeliveryFailure{Error: "boom", At: epoch0}
	if v := ticketSummary(&tk, core.TicketStage(&tk), nil); !v.CanRetry || v.Action != "delivery" {
		t.Errorf("summary = %v", v)
	}
}

func TestTriggerViewClipsText(t *testing.T) {
	tr := core.Trigger{ID: "R-1", Text: strings.Repeat("é", 200), Meta: map[string]string{"secret": "x"},
		Channels: []core.ChannelRef{{Address: map[string]string{"k": "v"}}}}
	v := triggerView(&tr, nil)
	if n := len([]rune(v.Text)); n != previewRunes+1 {
		t.Errorf("text has %d runes", n)
	}
}

func TestDetailTimeline(t *testing.T) {
	f := newFake()
	tk := ticket("T-1")
	tk.DecisionID = "D-1"
	tk.Origin = core.Origin{TriggerID: "R-1"}
	tk.PendingApproval = "A-1"
	tk.Follows = []core.Follow{{Thread: "github:o/r#1", Own: true}}
	tk.Assignments = []core.Assignment{{ID: "as1", State: core.AssignmentRunning, CreatedAt: epoch0.Add(time.Minute),
		Report: &core.Report{State: core.ReportProgress, Summary: "going", At: epoch0.Add(2 * time.Minute)}}}
	tk.Inputs = []core.Input{{ID: "i1", Kind: core.InputText, At: epoch0.Add(3 * time.Minute), Text: "use token ghs_abcdef123456"}}
	f.put(tk)
	f.triggers["R-1"] = core.Trigger{ID: "R-1", At: epoch0, Text: "fix it"}
	f.approvals["A-1"] = core.Approval{ID: "A-1", Tool: "Bash", Summary: "rm", CreatedAt: epoch0.Add(4 * time.Minute)}
	d, err := New(f, quiet()).Detail("T-1")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range d.Timeline {
		kinds = append(kinds, e.Kind)
		if strings.Contains(e.Text, "ghs_") {
			t.Errorf("unredacted timeline text %q", e.Text)
		}
	}
	if got := strings.Join(kinds, ","); got != "trigger,decision,assignment,report,input,approval" {
		t.Errorf("timeline kinds = %s", got)
	}
	if len(d.Threads) != 1 || d.Summary.Id != "T-1" {
		t.Errorf("detail = %v", d)
	}
	if _, err := New(f, quiet()).Detail("nope"); err == nil {
		t.Error("detail of a missing Ticket must fail")
	}
}

func TestTicketSummaryBranchAndRuntime(t *testing.T) {
	// What the facts pass found: only gitDir has a .git entry. The summary asks it, never the filesystem.
	gitDir, plainDir := "/src/repo", "/src/plain"
	gitFolder := func(dir string) bool { return dir == gitDir }
	for _, c := range []struct {
		name   string
		mod    func(*core.Ticket)
		branch string
		rt     string
	}{
		{"a GitHub repository", func(tk *core.Ticket) { tk.Repo = "o/r" }, "athena/t-0042", "claude"},
		{"a git directory", func(tk *core.Ticket) { tk.Execution = &core.Execution{Directory: gitDir} }, "athena/t-0042", "claude"},
		{"a repository with a plain folder", func(tk *core.Ticket) { tk.Repo, tk.Execution = "o/r", &core.Execution{Directory: plainDir} }, "", "claude"},
		{"a subdirectory of a repository", func(tk *core.Ticket) { tk.Execution = &core.Execution{Directory: filepath.Join(gitDir, "pkg")} }, "", "claude"},
		{"a plain folder", func(tk *core.Ticket) { tk.Execution = &core.Execution{Directory: plainDir, Runtime: "claude"} }, "", "claude"},
		{"no placement", func(*core.Ticket) {}, "", "claude"},
		{"another runtime", func(tk *core.Ticket) { tk.Repo, tk.Execution = "o/r", &core.Execution{Runtime: "codex"} }, "athena/t-0042", "codex"},
	} {
		tk := ticket("T-0042")
		c.mod(&tk)
		v := ticketSummary(&tk, core.TicketStage(&tk), gitFolder)
		if v.Branch != c.branch || v.Runtime != c.rt {
			t.Errorf("%s: branch %q runtime %q, want %q %q", c.name, v.Branch, v.Runtime, c.branch, c.rt)
		}
	}
}

func TestTimelineHasRepliesAndAnsweredApprovals(t *testing.T) {
	f := newFake()
	tk := ticket("T-1")
	tk.Inputs = []core.Input{
		{ID: "i1", Kind: core.InputTrigger, At: epoch0.Add(time.Minute), Human: true, Text: "use the other branch"},
		{ID: "i2", Kind: core.InputTrigger, At: epoch0.Add(2 * time.Minute), Human: true, Context: true, Text: "overheard"},
		{ID: "i3", Kind: core.InputText, At: epoch0.Add(3 * time.Minute), Text: "system note"},
		{ID: "i4", Kind: core.InputTrigger, At: epoch0.Add(4 * time.Minute), Text: "a bot said"},
	}
	f.put(tk)
	answered := func(id, state, note string, at time.Duration) core.Approval {
		when := epoch0.Add(at)
		return core.Approval{ID: id, TicketID: "T-1", Tool: "Bash", State: state, Answer: note, CreatedAt: epoch0, AnsweredAt: &when}
	}
	f.mu.Lock()
	f.approvals["A-1"] = answered("A-1", core.ApprovalApproved, "", 5*time.Minute)
	f.approvals["A-2"] = answered("A-2", core.ApprovalDenied, "too risky", 6*time.Minute)
	f.approvals["A-3"] = core.Approval{ID: "A-3", TicketID: "other", Tool: "Edit", State: core.ApprovalApproved, CreatedAt: epoch0}
	f.mu.Unlock()
	d, err := New(f, quiet()).Detail("T-1")
	if err != nil {
		t.Fatal(err)
	}
	want := "reply/owner  · use the other branch|input/context  · overheard|input/text  · system note|input/trigger  · a bot said|" +
		"approval/approved Bash · |approval/denied Bash · too risky"
	if got := entries(d.Timeline); got != want {
		t.Errorf("timeline:\n got %q\nwant %q", got, want)
	}
}

func TestTimelineShowsInterruptionsAndResumes(t *testing.T) {
	f := newFake()
	tk := ticket("T-1")
	started, resumed := epoch0.Add(time.Minute), epoch0.Add(3*time.Minute)
	tk.Assignments = []core.Assignment{{ID: "a1", State: core.AssignmentRunning, CreatedAt: epoch0, StartedAt: &started, Model: "sonnet",
		Interruptions: []core.Interruption{
			{At: epoch0.Add(2 * time.Minute), Reason: core.InterruptRestart, ResumedAt: &resumed},
			{At: epoch0.Add(4 * time.Minute), Reason: core.InterruptRestart},
		}}}
	f.put(tk)
	d, err := New(f, quiet()).Detail("T-1")
	if err != nil {
		t.Fatal(err)
	}
	want := "assignment/started sonnet · |assignment/restart  · |assignment/resumed  · |assignment/restart  · "
	if got := entries(d.Timeline); got != want {
		t.Errorf("timeline:\n got %q\nwant %q", got, want)
	}
}

// The owner's pause reads as its own interruption, and the start after the resume as resumed.
func TestTimelineShowsPauseAndResume(t *testing.T) {
	f := newFake()
	tk := ticket("T-1")
	started, resumed := epoch0.Add(time.Minute), epoch0.Add(3*time.Minute)
	tk.Assignments = []core.Assignment{{ID: "a1", State: core.AssignmentRunning, CreatedAt: epoch0, StartedAt: &started, Model: "sonnet",
		Interruptions: []core.Interruption{
			{At: epoch0.Add(2 * time.Minute), Reason: core.InterruptPaused, ResumedAt: &resumed},
			{At: epoch0.Add(4 * time.Minute), Reason: core.InterruptPaused},
		}}}
	f.put(tk)
	d, err := New(f, quiet()).Detail("T-1")
	if err != nil {
		t.Fatal(err)
	}
	want := "assignment/started sonnet · |assignment/paused  · |assignment/unpaused  · |assignment/paused  · "
	if got := entries(d.Timeline); got != want {
		t.Errorf("timeline:\n got %q\nwant %q", got, want)
	}
}

// entries is a timeline as kind/subkind subject · text, one entry per |.
func entries(tl []*athenav1.TimelineEntry) string {
	var got []string
	for _, e := range tl {
		got = append(got, e.Kind+"/"+e.Subkind+" "+e.Subject+" · "+e.Text)
	}
	return strings.Join(got, "|")
}

func TestTimelineInPlainTerms(t *testing.T) {
	f := newFake()
	tk := ticket("T-6")
	tk.DecisionID = "D-1"
	tk.Origin = core.Origin{TriggerID: "github:issuecomment:5"}
	at := func(d time.Duration) time.Time { return epoch0.Add(d) }
	ptr := func(d time.Duration) *time.Time { v := at(d); return &v }
	tk.Assignments = []core.Assignment{
		{ID: "as1", State: core.AssignmentEnded, CreatedAt: at(time.Minute), StartedAt: ptr(90 * time.Second),
			EndedAt: ptr(5 * time.Minute), EndedReason: "retried", Model: "sonnet"},
		{ID: "as2", State: core.AssignmentEnded, CreatedAt: at(5 * time.Minute), EndedAt: ptr(40 * time.Minute), Model: "opus",
			Report: &core.Report{State: core.ReportReady, Summary: "\n## Review done\n\n1. Login doesn't type-check `password`\n2. More",
				Pull: 12, At: at(39 * time.Minute)}},
	}
	tk.Inputs = []core.Input{
		// The triggering comment, recorded as an Input too: the Trigger already says it.
		{ID: "i0", Kind: core.InputTrigger, At: at(10 * time.Second), TriggerID: "github:issuecomment:5", Human: true,
			Text: "@athena-gh please validate"},
		// Athena's own acknowledgement, read back from GitHub.
		{ID: "i1", Kind: core.InputTrigger, At: at(2 * time.Minute), TriggerID: "github:issuecomment:6",
			Text: "Validating and reviewing\n\n<!-- athena:receipt:5e527134fc22f9b9 -->"},
		{ID: "i2", Kind: core.InputTrigger, At: at(3 * time.Minute), TriggerID: "github:issuecomment:7", Human: true, Text: "looks good"},
		{ID: "i3", Kind: core.InputTrigger, At: at(4 * time.Minute), TriggerID: "R-9", Human: true, Text: "continue"},
	}
	tk.Ended = &core.Ended{At: at(40 * time.Minute), By: core.EndByExecutor, Outcome: core.OutcomeCompleted}
	f.put(tk)
	f.triggers["github:issuecomment:5"] = core.Trigger{ID: "github:issuecomment:5", Source: "github", At: epoch0,
		Text: "@athena-gh please validate", Meta: map[string]string{"kind": "pull"}}
	f.triggers["github:issuecomment:7"] = core.Trigger{ID: "github:issuecomment:7", Source: "github",
		Actor: core.ActorRef{Source: "github", ID: "alice"}, Human: true, Text: "looks good"}
	approved := func(id string, at time.Duration, summary string) core.Approval {
		when := epoch0.Add(at)
		return core.Approval{ID: id, TicketID: "T-6", Tool: "Bash", Summary: summary, State: core.ApprovalApproved,
			CreatedAt: when.Add(-time.Second), AnsweredAt: &when}
	}
	f.mu.Lock()
	f.approvals["A-1"] = approved("A-1", 10*time.Minute, "Needs approval — Bash: npm test")
	f.approvals["A-2"] = approved("A-2", 11*time.Minute, "Bash: go vet ./...")
	f.approvals["A-3"] = approved("A-3", 12*time.Minute, "")
	f.mu.Unlock()
	d, err := New(f, quiet()).Detail("T-6")
	if err != nil {
		t.Fatal(err)
	}
	want := "trigger/pull-comment  · @athena-gh please validate|decision/accepted  · |" +
		"assignment/started sonnet · |input/receipt  · Validating and reviewing|reply/github alice · looks good|" +
		"reply/owner  · continue|assignment/retried  · |assignment/started opus · |" +
		"approval/approved Bash: npm test · |approval/approved Bash: go vet ./... · |approval/approved Bash · |" +
		"report/ready PR #12 · ## Review done|assignment/ended  · "
	if got := entries(d.Timeline); got != want {
		t.Errorf("timeline:\n got %q\nwant %q", got, want)
	}
	// The start entry is at the start, the end entry at the end.
	if s := d.Timeline[2].At.AsTime(); !s.Equal(at(90 * time.Second)) {
		t.Errorf("start entry at %v", s)
	}
	if e := d.Timeline[len(d.Timeline)-1].At.AsTime(); !e.Equal(at(40 * time.Minute)) {
		t.Errorf("end entry at %v", e)
	}
	for _, e := range d.Timeline {
		if strings.Contains(e.Text, "<!--") {
			t.Errorf("a marker leaked: %q", e.Text)
		}
	}
}

func TestTimelineAssignmentEnds(t *testing.T) {
	at := func(d time.Duration) *time.Time { v := epoch0.Add(d); return &v }
	for _, c := range []struct {
		name string
		mod  func(*core.Ticket)
		want string
	}{
		{"running", func(tk *core.Ticket) {
			tk.Assignments = []core.Assignment{{ID: "a", State: core.AssignmentRunning, StartedAt: at(time.Minute)}}
		}, "assignment/started  · "},
		{"queued", func(tk *core.Ticket) {
			tk.Assignments = []core.Assignment{{ID: "a", State: core.AssignmentPending}}
		}, "assignment/queued  · "},
		{"stopped", func(tk *core.Ticket) {
			tk.Assignments = []core.Assignment{{ID: "a", State: core.AssignmentEnded, EndedAt: at(2 * time.Minute)}}
			tk.Ended = &core.Ended{At: *at(2 * time.Minute), By: core.EndByHuman, Outcome: core.OutcomeStopped, Reason: "Stopped from the Ticket page"}
		}, "assignment/started  · |assignment/stopped  · Stopped from the Ticket page"},
		{"failed", func(tk *core.Ticket) {
			tk.Assignments = []core.Assignment{{ID: "a", State: core.AssignmentEnded, EndedAt: at(2 * time.Minute),
				Report: &core.Report{State: core.ReportFailed, Summary: "session crashed repeatedly: exit 1", At: *at(time.Minute)}}}
		}, "assignment/started  · |report/failed  · session crashed repeatedly: exit 1|assignment/ended  · "},
		{"interrupted", func(tk *core.Ticket) {
			tk.Assignments = []core.Assignment{{ID: "a", State: core.AssignmentEnded, EndedAt: at(2 * time.Minute), EndedReason: "node restart"}}
		}, "assignment/started  · |assignment/interrupted  · node restart"},
		{"limited", func(tk *core.Ticket) {
			reset := time.Date(2026, 11, 6, 15, 0, 0, 0, time.FixedZone("x", 3600))
			tk.Assignments = []core.Assignment{
				{ID: "a", State: core.AssignmentEnded, Runtime: core.RuntimeClaude, EndedAt: at(2 * time.Minute), EndedReason: core.ReasonUsageLimit,
					Limit: &core.UsageLimit{Runtime: core.RuntimeClaude, ResetsAt: &reset, Message: "usage limit reached"}},
				{ID: "b", State: core.AssignmentPending, CreatedAt: *at(2 * time.Minute)},
			}
		}, "assignment/started  · |assignment/limited claude · 2026-11-06T14:00:00Z|assignment/queued  · "},
		{"review failed", func(tk *core.Ticket) {
			tk.Assignments = []core.Assignment{{ID: "a", State: core.AssignmentEnded, Role: core.RoleReview, EndedAt: at(2 * time.Minute),
				EndedReason: core.ReasonReviewed, ReviewOutcome: &core.ReviewOutcome{Verdict: core.ReviewFailed, First: core.ReviewFailed,
					Why: "no runtime is ready: claude: a Claude Code review needs sandbox.required: true"}}}
		}, "assignment/review  · |review/failed  · no runtime is ready: claude: a Claude Code review needs sandbox.required: true"},
		{"limited, reset unknown", func(tk *core.Ticket) {
			tk.Assignments = []core.Assignment{{ID: "a", State: core.AssignmentEnded, Runtime: core.RuntimeCodex, EndedAt: at(2 * time.Minute),
				EndedReason: core.ReasonUsageLimit, Limit: &core.UsageLimit{Runtime: core.RuntimeCodex}}}
		}, "assignment/started  · |assignment/limited codex · "},
	} {
		f := newFake()
		tk := ticket("T-1")
		c.mod(&tk)
		f.put(tk)
		d, err := New(f, quiet()).Detail("T-1")
		if err != nil {
			t.Fatal(err)
		}
		if got := entries(d.Timeline); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestOwnerTaskHasNoRouterDecision(t *testing.T) {
	f := newFake()
	tk := ticket("T-1")
	tk.DecisionID, tk.Origin = "D-1", core.Origin{TriggerID: "R-1"}
	tk.Inputs = []core.Input{{ID: "i1", Kind: core.InputTrigger, At: epoch0, TriggerID: "R-1", Human: true, Text: "fix the login"}}
	f.put(tk)
	f.triggers["R-1"] = core.Trigger{ID: "R-1", Source: "owner", Human: true, At: epoch0, Text: "fix the login"}
	d, err := New(f, quiet()).Detail("T-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := entries(d.Timeline); got != "trigger/owner  · fix the login" {
		t.Errorf("timeline = %q", got)
	}
}

func TestDetailReports(t *testing.T) {
	f := newFake()
	tk := ticket("T-1")
	for i := range 6 {
		tk.Assignments = append(tk.Assignments, core.Assignment{ID: fmt.Sprintf("a%d", i), State: core.AssignmentEnded,
			Report: &core.Report{State: core.ReportProgress, Summary: fmt.Sprintf("report %d", i), At: epoch0.Add(time.Duration(i) * time.Minute)}})
	}
	tk.Assignments = append(tk.Assignments, core.Assignment{ID: "no-report", State: core.AssignmentEnded})
	last := &tk.Assignments[5]
	last.State = core.AssignmentRunning
	last.Report = &core.Report{State: core.ReportWaiting, Summary: "# Done\n\n- one\n- token ghs_abcdef123456\n" + strings.Repeat("x", 20000),
		Pull: 7, At: epoch0.Add(time.Hour)}
	tk.Assignments[4].Report.Question = "Which one?"
	tk.Attention = &core.Attention{ID: "q", Question: "Redis or in-process?"}
	f.put(tk)
	d, err := New(f, quiet()).Detail("T-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Reports) != 5 {
		t.Fatalf("%d reports, want the last 5", len(d.Reports))
	}
	if d.Reports[0].AssignmentId != "a1" || d.Reports[0].Summary != "report 1" || d.Reports[3].Question != "Which one?" {
		t.Errorf("reports = %v", d.Reports)
	}
	r := d.Reports[4]
	if r.AssignmentId != "a5" || r.State != "waiting" || r.Pull != 7 || !r.At.AsTime().Equal(epoch0.Add(time.Hour)) {
		t.Errorf("latest = %v", r)
	}
	// The latest Report's question is the Ticket's open one when the Report itself has none (stored before it kept one).
	if r.Question != "Redis or in-process?" {
		t.Errorf("question = %q", r.Question)
	}
	if !strings.HasPrefix(r.Summary, "# Done\n\n- one\n- token ") || strings.Contains(r.Summary, "ghs_") {
		t.Errorf("summary keeps its lines and is redacted: %q", r.Summary[:40])
	}
	if n := len(r.Summary); n > 16<<10+4 || n < 16<<10-4 || !utf8.ValidString(r.Summary) || !strings.HasSuffix(r.Summary, "…") {
		t.Errorf("summary is %d bytes, want it cut at 16 KiB", n)
	}
}

func TestEchoOfTheOriginIsDroppedOnce(t *testing.T) {
	f := newFake()
	tk := ticket("T-1")
	tk.Origin = core.Origin{TriggerID: "R-1"}
	at := func(d time.Duration) time.Time { return epoch0.Add(d) }
	tk.Inputs = []core.Input{
		// Before the Trigger: not its echo.
		{ID: "i0", Kind: core.InputTrigger, At: at(-time.Second), Human: true, Text: "continue"},
		// The owner's task recorded again as its first Input: the echo.
		{ID: "i1", Kind: core.InputTrigger, At: at(0), TriggerID: "R-1", Human: true, Text: "continue"},
		// The owner says it again, 20 s later: a real message, on its own Trigger.
		{ID: "i2", Kind: core.InputTrigger, At: at(20 * time.Second), TriggerID: "R-2", Human: true, Text: "continue"},
		// And once more without a Trigger id: only the first echo goes.
		{ID: "i3", Kind: core.InputTrigger, At: at(30 * time.Second), Human: true, Text: "continue"},
		// Not a person's: never the echo.
		{ID: "i4", Kind: core.InputNote, At: at(40 * time.Second), Text: "continue"},
	}
	f.put(tk)
	f.triggers["R-1"] = core.Trigger{ID: "R-1", Source: "owner", Human: true, At: epoch0, Text: "continue"}
	d, err := New(f, quiet()).Detail("T-1")
	if err != nil {
		t.Fatal(err)
	}
	want := "reply/owner  · continue|trigger/owner  · continue|reply/owner  · continue|reply/owner  · continue|input/note  · continue"
	if got := entries(d.Timeline); got != want {
		t.Errorf("timeline:\n got %q\nwant %q", got, want)
	}
}

func TestReceiptOnlyFromAthena(t *testing.T) {
	f := newFake()
	tk := ticket("T-1")
	tk.Inputs = []core.Input{
		{ID: "i1", Kind: core.InputTrigger, At: epoch0, TriggerID: "github:issuecomment:1", Text: "On it <!-- athena:receipt:abc -->"},
		// A person who pastes the marker is still a person replying.
		{ID: "i2", Kind: core.InputTrigger, At: epoch0.Add(time.Minute), TriggerID: "github:issuecomment:2", Human: true,
			Text: "fake <!-- athena:receipt:abc -->"},
	}
	f.put(tk)
	d, err := New(f, quiet()).Detail("T-1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := entries(d.Timeline), "input/receipt  · On it|reply/github  · fake"; got != want {
		t.Errorf("timeline:\n got %q\nwant %q", got, want)
	}
}

func TestReplyActorsReadsAreBounded(t *testing.T) {
	f := newFake()
	tk := ticket("T-1")
	for i := range 300 {
		// Many replies on few Triggers, most of them missing from the store.
		tk.Inputs = append(tk.Inputs, core.Input{ID: fmt.Sprintf("i%d", i), Kind: core.InputTrigger, At: epoch0,
			TriggerID: fmt.Sprintf("github:issuecomment:%d", i%120), Human: true, Text: "hi"})
	}
	f.put(tk)
	// The newest reply (i299) is on Trigger 299 % 120 = 59.
	f.triggers["github:issuecomment:59"] = core.Trigger{ID: "github:issuecomment:59", Actor: core.ActorRef{ID: "alice"}}
	h := New(f, quiet())
	d, err := h.Detail("T-1")
	if err != nil {
		t.Fatal(err)
	}
	if f.triggerReads > replyActorsMax {
		t.Errorf("%d Trigger reads for one detail, want at most %d", f.triggerReads, replyActorsMax)
	}
	last := d.Timeline[len(d.Timeline)-1]
	if last.Subject != "alice" {
		t.Errorf("the newest reply names %q", last.Subject)
	}
	// A Trigger never changes: later details read only the ones not read yet, each once, and then none.
	for range 3 {
		if _, err := h.Detail("T-1"); err != nil {
			t.Fatal(err)
		}
	}
	if f.triggerReads != 120 {
		t.Errorf("%d Trigger reads over four details, want each of the 120 Triggers read once", f.triggerReads)
	}
}

func TestReportQuestionIsCutAndSecretsAtTheCutStayRedacted(t *testing.T) {
	f := newFake()
	tk := ticket("T-1")
	token := "ghs_abcdefghij1234567890ABCDEFGHIJ"
	// The token straddles the 16 KiB cut: it is redacted whole before the cut, so no piece of it is left.
	summary := strings.Repeat("x", reportBytes-11) + " " + token + " tail"
	tk.Assignments = []core.Assignment{{ID: "a", State: core.AssignmentRunning,
		Report: &core.Report{State: core.ReportWaiting, Summary: summary, Question: strings.Repeat("q", 10000), At: epoch0}}}
	f.put(tk)
	d, err := New(f, quiet()).Detail("T-1")
	if err != nil {
		t.Fatal(err)
	}
	r := d.Reports[0]
	if strings.Contains(r.Summary, "ghs_") || strings.Contains(r.Summary, "abcdefghij") {
		t.Errorf("a piece of the token survived the cut: …%q", r.Summary[len(r.Summary)-40:])
	}
	if n := len(r.Question); n > questionBytes+4 || n < questionBytes-4 {
		t.Errorf("question is %d bytes, want it cut at %d", n, questionBytes)
	}
}

// Only what the store answered is remembered: a login, or a Trigger it does not have. A store that failed is asked
// again on the next detail.
func TestReplyActorsDoNotCacheStoreErrors(t *testing.T) {
	f := newFake()
	tk := ticket("T-1")
	tk.Inputs = []core.Input{
		{ID: "i1", Kind: core.InputTrigger, At: epoch0, TriggerID: "github:issuecomment:1", Human: true, Text: "hi"},
		{ID: "i2", Kind: core.InputTrigger, At: epoch0.Add(time.Minute), TriggerID: "github:issuecomment:2", Human: true, Text: "yo"},
	}
	f.put(tk)
	f.triggers["github:issuecomment:1"] = core.Trigger{ID: "github:issuecomment:1", Actor: core.ActorRef{ID: "alice"}}
	h := New(f, quiet())
	f.triggerErr = errors.New("database is locked")
	d, err := h.Detail("T-1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Timeline[0].Subject == "alice" {
		t.Fatal("named from a failed read")
	}
	f.triggerErr = nil
	f.triggerReads = 0
	if d, err = h.Detail("T-1"); err != nil {
		t.Fatal(err)
	}
	if d.Timeline[0].Subject != "alice" || f.triggerReads != 2 {
		t.Fatalf("after the store recovered: subject %q, %d reads (want alice, both read again)", d.Timeline[0].Subject, f.triggerReads)
	}
	// Trigger 2 is not stored: that answer is kept, and neither is read again.
	if _, err = h.Detail("T-1"); err != nil {
		t.Fatal(err)
	}
	if f.triggerReads != 2 {
		t.Errorf("%d reads after both were answered, want none more", f.triggerReads-2)
	}
}

// The first Input on the origin Trigger itself is its echo, whatever its text (the router may trim or quote it) and
// even a moment before the Trigger (clocks of two writers); only that first one.
func TestEchoOnTheOriginTriggerIsDroppedWhateverItsText(t *testing.T) {
	f := newFake()
	tk := ticket("T-1")
	tk.Origin = core.Origin{TriggerID: "github:issue:7"}
	at := func(d time.Duration) time.Time { return epoch0.Add(d) }
	tk.Inputs = []core.Input{
		{ID: "i1", Kind: core.InputTrigger, At: at(-300 * time.Millisecond), TriggerID: "github:issue:7", Human: true, Text: "Fix the login (trimmed)"},
		{ID: "i2", Kind: core.InputTrigger, At: at(time.Minute), TriggerID: "github:issue:7", Human: true, Text: "Fix the login, edited"},
	}
	f.put(tk)
	f.triggers["github:issue:7"] = core.Trigger{ID: "github:issue:7", Source: "github", Kind: "issue", Human: true, At: epoch0,
		Actor: core.ActorRef{Source: "github", ID: "alice"}, Text: "Fix the login\n\nIt 500s."}
	d, err := New(f, quiet()).Detail("T-1")
	if err != nil {
		t.Fatal(err)
	}
	got := entries(d.Timeline)
	if strings.Contains(got, "(trimmed)") || !strings.Contains(got, "Fix the login, edited") || strings.Count(got, "|") != 1 {
		t.Errorf("timeline: %q, want the origin and the later edit only", got)
	}
}

func TestSummaryRuntimeIsLatestStarted(t *testing.T) {
	at := func(d time.Duration) *time.Time { v := epoch0.Add(d); return &v }
	for _, c := range []struct {
		name string
		mod  func(*core.Ticket)
		want string
	}{
		{"no assignment: the choice", func(tk *core.Ticket) { tk.Execution = &core.Execution{Runtime: core.RuntimeCodex} }, "codex"},
		{"auto before any start", func(tk *core.Ticket) {
			tk.Execution = &core.Execution{Runtime: core.RuntimeAuto}
			tk.Assignments = []core.Assignment{{ID: "a", State: core.AssignmentPending}}
		}, "auto"},
		{"auto, started on codex", func(tk *core.Ticket) {
			tk.Execution = &core.Execution{Runtime: core.RuntimeAuto}
			tk.Assignments = []core.Assignment{{ID: "a", Runtime: core.RuntimeCodex, StartedAt: at(time.Minute)}}
		}, "codex"},
		{"failover: the later start wins", func(tk *core.Ticket) {
			tk.Execution = &core.Execution{Runtime: core.RuntimeAuto}
			tk.Assignments = []core.Assignment{
				{ID: "a", Runtime: core.RuntimeClaude, StartedAt: at(time.Minute)},
				{ID: "b", Runtime: core.RuntimeCodex, StartedAt: at(2 * time.Minute)},
			}
		}, "codex"},
		{"a queued one is not started", func(tk *core.Ticket) {
			tk.Execution = &core.Execution{Runtime: core.RuntimeAuto}
			tk.Assignments = []core.Assignment{
				{ID: "a", Runtime: core.RuntimeClaude, StartedAt: at(time.Minute)},
				{ID: "b", Runtime: core.RuntimeCodex, State: core.AssignmentPending},
			}
		}, "claude"},
	} {
		tk := ticket("T-1")
		c.mod(&tk)
		if got := ticketSummary(&tk, core.TicketStage(&tk), nil).Runtime; got != c.want {
			t.Errorf("%s: runtime %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSummaryRuntimeChoiceAndPairing(t *testing.T) {
	tk := ticket("T-1")
	if v := ticketSummary(&tk, core.TicketStage(&tk), nil); v.RuntimeChoice != "claude" || v.Pairing != "" || v.ReviewRounds != 0 {
		t.Errorf("a Ticket from before runtimes: %v", v)
	}
	tk.Execution, tk.Pairing = &core.Execution{Runtime: core.RuntimeAuto}, core.PairingReview
	tk.Stats.ReviewRounds = 2
	tk.Assignments = []core.Assignment{{ID: "a", Runtime: core.RuntimeCodex, StartedAt: &epoch0}}
	v := ticketSummary(&tk, core.TicketStage(&tk), nil)
	if v.RuntimeChoice != "auto" || v.Runtime != "codex" || v.Pairing != "review" || v.ReviewRounds != 2 {
		t.Errorf("summary = %v", v)
	}
}

func TestTimelineRestartResumeLimitReviewEntries(t *testing.T) {
	at := func(d time.Duration) *time.Time { v := epoch0.Add(d); return &v }
	reset := epoch0.Add(5 * time.Hour)
	tk := ticket("T-1")
	tk.Assignments = []core.Assignment{
		{ID: "a1", State: core.AssignmentEnded, Runtime: core.RuntimeClaude, Model: "sonnet", CreatedAt: epoch0, StartedAt: at(time.Minute),
			Interruptions: []core.Interruption{{At: epoch0.Add(2 * time.Minute), Reason: core.InterruptRestart, ResumedAt: at(3 * time.Minute)}},
			EndedAt:       at(4 * time.Minute), EndedReason: core.ReasonUsageLimit,
			Limit: &core.UsageLimit{Runtime: core.RuntimeClaude, ResetsAt: &reset}},
		{ID: "a2", State: core.AssignmentEnded, Runtime: core.RuntimeCodex, Model: "gpt", CreatedAt: epoch0.Add(4 * time.Minute),
			StartedAt: at(5 * time.Minute), EndedAt: at(10 * time.Minute), EndedReason: core.ReasonReview},
		{ID: "r1", State: core.AssignmentEnded, Role: core.RoleReview, Runtime: core.RuntimeClaude, Model: "opus", CreatedAt: epoch0.Add(10 * time.Minute),
			StartedAt: at(11 * time.Minute), EndedAt: at(12 * time.Minute), EndedReason: core.ReasonReviewed,
			ReviewOutcome: &core.ReviewOutcome{Verdict: core.ReviewChanges, First: "- [high] main.go:3 — leaks a goroutine"}},
		{ID: "r2", State: core.AssignmentEnded, Role: core.RoleReview, Runtime: core.RuntimeClaude, CreatedAt: epoch0.Add(13 * time.Minute),
			StartedAt: at(14 * time.Minute), EndedAt: at(15 * time.Minute), EndedReason: core.ReasonReviewed,
			ReviewOutcome: &core.ReviewOutcome{Verdict: core.ReviewApproved, First: "- none"}},
		{ID: "r3", State: core.AssignmentEnded, Role: core.RoleReview, Runtime: core.RuntimeCodex, CreatedAt: epoch0.Add(16 * time.Minute),
			StartedAt: at(17 * time.Minute), EndedAt: at(18 * time.Minute), EndedReason: core.ReasonReviewed,
			ReviewOutcome: &core.ReviewOutcome{Verdict: core.ReviewFailed, First: core.ReviewFailed, Why: "codex exited 2\nsecond line"}},
	}
	f := newFake()
	f.put(tk)
	d, err := New(f, quiet()).Detail("T-1")
	if err != nil {
		t.Fatal(err)
	}
	want := "assignment/started sonnet · |assignment/restart  · |assignment/resumed  · |assignment/limited claude · 2026-11-04T17:00:00Z|" +
		"assignment/started gpt · |assignment/ended  · review|" +
		"assignment/review opus · |review/changes claude · - [high] main.go:3 — leaks a goroutine|" +
		"assignment/review  · |review/approved claude · - none|" +
		"assignment/review  · |review/failed codex · codex exited 2"
	if got := entries(d.Timeline); got != want {
		t.Errorf("timeline:\n got %q\nwant %q", got, want)
	}
	var runtimes []string
	for _, e := range d.Timeline {
		runtimes = append(runtimes, e.Runtime)
	}
	if got := strings.Join(runtimes, ","); got != "claude,claude,claude,claude,codex,codex,claude,claude,claude,claude,codex,codex" {
		t.Errorf("entry runtimes = %s", got)
	}
}

func TestNodeStatusRuntimesWithQuota(t *testing.T) {
	reset := epoch0.Add(2 * time.Hour)
	until := epoch0.Add(time.Hour)
	v := nodeStatus(NodeInfo{Name: "n", Status: node.Status{ClaudeVersion: "2.1", Runtimes: []node.RuntimeStatus{
		{Name: "claude", Ready: true, Tested: true, Live: 1, Quota: quota.State{},
			Models: []rt.ModelInfo{{ID: "opus", Name: "Opus"}, {ID: "sonnet", Name: "Sonnet", Default: true}}},
		{Name: "codex", Version: "0.160.1", Ready: true, Tested: true, Live: 2, Quota: quota.State{
			Measured: true, ReadAt: epoch0, LimitedUntil: &until,
			Windows: []quota.Window{{Bucket: "codex", Name: "5h", Used: 0.5, ResetsAt: &reset}, {Name: "7d", Used: 0.25, Stale: true}}}},
	}}})
	if v.ClaudeVersion != "2.1" || len(v.Runtimes) != 2 {
		t.Fatalf("node = %v", v)
	}
	c, x := v.Runtimes[0], v.Runtimes[1]
	if c.Name != "claude" || !c.Ready || c.Measured || c.Live != 1 || len(c.Windows) != 0 || c.LimitedUntil != nil || c.ReadAt != nil {
		t.Errorf("claude = %v", c)
	}
	if m := c.Models; len(m) != 2 || m[0].Id != "opus" || m[0].Name != "Opus" || m[0].Default || m[1].Id != "sonnet" || !m[1].Default {
		t.Errorf("claude models = %v", m)
	}
	if x.Name != "codex" || x.Version != "0.160.1" || !x.Ready || !x.Tested || !x.Measured || x.Live != 2 || len(x.Models) != 0 ||
		!x.LimitedUntil.AsTime().Equal(until) || !x.ReadAt.AsTime().Equal(epoch0) || len(x.Windows) != 2 {
		t.Fatalf("codex = %v", x)
	}
	// One bucket: its windows are named by themselves.
	if w := x.Windows[0]; w.Name != "5h" || w.Used != 0.5 || !w.ResetsAt.AsTime().Equal(reset) || w.Stale {
		t.Errorf("window 0 = %v", w)
	}
	if w := x.Windows[1]; w.Name != "7d" || w.Used != 0.25 || w.ResetsAt != nil || !w.Stale {
		t.Errorf("window 1 = %v", w)
	}
}

// A runtime that reports windows of more than one bucket (a Codex per-model limit) names each by its bucket too.
func TestWindowBucketOnlyWhenSeveral(t *testing.T) {
	v := runtimeStatus(node.RuntimeStatus{Name: "codex", Quota: quota.State{Measured: true, Windows: []quota.Window{
		{Bucket: "codex", Name: "5h"}, {Bucket: "codex", Name: "7d"}, {Bucket: "codex_mini", Name: "5h"}}}})
	var names []string
	for _, w := range v.Windows {
		names = append(names, w.Name)
	}
	if got := strings.Join(names, ","); got != "codex 5h,codex 7d,codex_mini 5h" {
		t.Errorf("names = %s", got)
	}
}

func TestRuntimeWhyRedactedAndCut(t *testing.T) {
	v := runtimeStatus(node.RuntimeStatus{Name: "codex", Why: "login failed: GITHUB_TOKEN=ghp_abc123 was\nrejected"})
	if strings.Contains(v.Why, "ghp_abc123") || strings.Contains(v.Why, "\n") || !strings.Contains(v.Why, "login failed") {
		t.Errorf("why = %q", v.Why)
	}
	long := runtimeStatus(node.RuntimeStatus{Name: "codex", Why: strings.Repeat("é", 500)})
	if n := utf8.RuneCountInString(long.Why); n != WhyMaxRunes+1 || !strings.HasSuffix(long.Why, "…") {
		t.Errorf("why has %d runes", n)
	}
}

// A review holds the implementer's ready report: it is not "ready" while the review runs, nor after a review that sent
// it back, and once applied (applyHeld) it reads once, from the Assignment that applied it, which itself started no
// session and so has no start of its own.
func TestHeldReportShowsOnceWhenApplied(t *testing.T) {
	at := func(d time.Duration) *time.Time { v := epoch0.Add(d); return &v }
	held := &core.Report{State: core.ReportReady, Summary: "Login done", At: epoch0.Add(5 * time.Minute)}
	impl := core.Assignment{ID: "a1", State: core.AssignmentEnded, Runtime: core.RuntimeCodex, Model: "gpt", CreatedAt: epoch0,
		StartedAt: at(time.Minute), EndedAt: at(5 * time.Minute), EndedReason: core.ReasonReview, Report: held}
	review := core.Assignment{ID: "r1", State: core.AssignmentRunning, Role: core.RoleReview, Runtime: core.RuntimeClaude,
		CreatedAt: epoch0.Add(5 * time.Minute), StartedAt: at(6 * time.Minute)}

	detail := func(as ...core.Assignment) *athenav1.TicketDetail {
		t.Helper()
		tk := ticket("T-1")
		tk.Assignments = as
		f := newFake()
		f.put(tk)
		d, err := New(f, quiet()).Detail("T-1")
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	// During the review: no ready anywhere.
	d := detail(impl, review)
	if got := entries(d.Timeline); strings.Contains(got, "report/") {
		t.Errorf("during the review the timeline shows a report: %q", got)
	}
	if len(d.Reports) != 0 {
		t.Errorf("during the review the reports are %v", d.Reports)
	}

	// After changes were requested: still none; the next round has not reported.
	sent := review
	sent.State, sent.EndedAt, sent.EndedReason = core.AssignmentEnded, at(7*time.Minute), core.ReasonReviewed
	sent.ReviewOutcome = &core.ReviewOutcome{Verdict: core.ReviewChanges, First: "- [high] a.go:1 — racy"}
	next := core.Assignment{ID: "a2", State: core.AssignmentPending, CreatedAt: epoch0.Add(7 * time.Minute)}
	d = detail(impl, sent, next)
	if got := entries(d.Timeline); strings.Contains(got, "report/") {
		t.Errorf("after changes the timeline shows a report: %q", got)
	}
	if len(d.Reports) != 0 {
		t.Errorf("after changes the reports are %v", d.Reports)
	}

	// Approved: the report applies once, on the continuing Assignment, which started no session of its own.
	approved := review
	approved.State, approved.EndedAt, approved.EndedReason = core.AssignmentEnded, at(7*time.Minute), core.ReasonReviewed
	approved.ReviewOutcome = &core.ReviewOutcome{Verdict: core.ReviewApproved, First: "- none"}
	applied := *held
	applied.At = epoch0.Add(7 * time.Minute)
	cont := core.Assignment{ID: "a2", State: core.AssignmentRunning, Runtime: core.RuntimeCodex, Model: "gpt",
		CreatedAt: epoch0.Add(7 * time.Minute), StartedAt: at(7 * time.Minute), IdleAt: at(7 * time.Minute),
		Continues: "a1", Report: &applied}
	d = detail(impl, approved, cont)
	want := "assignment/started gpt · |assignment/ended  · review|assignment/review  · |review/approved claude · - none|" +
		"report/ready  · Login done"
	if got := entries(d.Timeline); got != want {
		t.Errorf("applied timeline:\n got %q\nwant %q", got, want)
	}
	if len(d.Reports) != 1 || d.Reports[0].AssignmentId != "a2" || d.Reports[0].State != "ready" {
		t.Errorf("applied reports = %v", d.Reports)
	}

	// Once a later message resumes that session, its start reads at the delivery.
	cont.DeliveredAt = at(30 * time.Minute)
	d = detail(impl, approved, cont)
	if got := entries(d.Timeline); got != want+"|assignment/started gpt · " {
		t.Errorf("resumed timeline: %q", got)
	}
	if e := d.Timeline[len(d.Timeline)-1]; !e.At.AsTime().Equal(epoch0.Add(30 * time.Minute)) {
		t.Errorf("resumed start at %v, want the delivery", e.At.AsTime())
	}
}

func TestViewsCarryTrust(t *testing.T) {
	tk := ticket("T-1")
	tk.Mandate = core.Mandate{Trust: core.TrustCollaborator}
	if v := ticketSummary(&tk, core.TicketStage(&tk), nil); v.Trust != "collaborator" {
		t.Errorf("summary trust = %q", v.Trust)
	}
	// A Ticket whose Mandate predates trust classes reads its class from the basis.
	tk.Mandate = core.Mandate{Basis: core.MandateBasis{Via: core.ViaOwner}}
	if v := ticketSummary(&tk, core.TicketStage(&tk), nil); v.Trust != "owner" {
		t.Errorf("legacy summary trust = %q", v.Trust)
	}
	tr := core.Trigger{ID: "R-1", Source: "github"}
	if v := triggerView(&tr, func(core.Trigger) string { return core.TrustStranger }); v.Trust != "stranger" {
		t.Errorf("trigger trust = %q", v.Trust)
	}
}

func TestTicketSummaryNeedsYouForProposal(t *testing.T) {
	tk := core.Ticket{ID: "T-1", Title: "x", PendingProposals: 1}
	v := ticketSummary(&tk, core.TicketStage(&tk), nil)
	if !v.NeedsYou || v.Stage != "person" || v.Action != "proposal" {
		t.Errorf("summary = %v, want needs_you with stage person and action proposal", v)
	}
}
