package hub

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/pb/athenav1"
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
	v := triggerView(&tr)
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
		}, "assignment/started  · |report/failed  · session crashed repeatedly: exit 1|assignment/failed  · session crashed repeatedly: exit 1"},
		{"interrupted", func(tk *core.Ticket) {
			tk.Assignments = []core.Assignment{{ID: "a", State: core.AssignmentEnded, EndedAt: at(2 * time.Minute), EndedReason: "node restart"}}
		}, "assignment/started  · |assignment/interrupted  · node restart"},
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
