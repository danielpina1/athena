package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

func TestTicketSummaryFlags(t *testing.T) {
	tk := ticket("T-1")
	tk.Attention = &core.Attention{ID: "q", Question: "which branch?"}
	tk.Repo = "o/r"
	tk.Follows = []core.Follow{{Thread: "github:o/other#9"}, {Thread: "github:o/r#12", Own: true}}
	tk.Execution = &core.Execution{Directory: "/work/T-1", Model: "sonnet"}
	st := core.TicketStage(&tk)
	v := ticketSummary(&tk, st)
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
	if v := ticketSummary(&w, core.TicketStage(&w)); v.NeedsYou || v.Model != "opus" {
		t.Errorf("working summary = %v", v)
	}
	e := ended("T-3", 1)
	if v := ticketSummary(&e, core.TicketStage(&e)); v.Outcome != "completed" || v.EndedAt == nil || v.CanRetry {
		t.Errorf("ended summary = %v", v)
	}
}

func TestCanRetryOnDeliveryFailure(t *testing.T) {
	tk := ticket("T-1")
	tk.Delivery = &core.DeliveryFailure{Error: "boom", At: epoch0}
	if v := ticketSummary(&tk, core.TicketStage(&tk)); !v.CanRetry || v.Action != "delivery" {
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
	gitDir, plainDir := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(gitDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		mod    func(*core.Ticket)
		branch string
		rt     string
	}{
		{"a GitHub repository", func(tk *core.Ticket) { tk.Repo = "o/r" }, "athena/t-0042", "claude"},
		{"a git directory", func(tk *core.Ticket) { tk.Execution = &core.Execution{Directory: gitDir} }, "athena/t-0042", "claude"},
		{"a plain folder", func(tk *core.Ticket) { tk.Execution = &core.Execution{Directory: plainDir, Runtime: "claude"} }, "", "claude"},
		{"no placement", func(*core.Ticket) {}, "", "claude"},
		{"another runtime", func(tk *core.Ticket) { tk.Repo, tk.Execution = "o/r", &core.Execution{Runtime: "codex"} }, "athena/t-0042", "codex"},
	} {
		tk := ticket("T-0042")
		c.mod(&tk)
		v := ticketSummary(&tk, core.TicketStage(&tk))
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
	var got []string
	for _, e := range d.Timeline {
		got = append(got, e.Kind+" "+e.Text)
	}
	want := "reply use the other branch|input overheard|input system note|input a bot said|approval Approved: Bash|approval Denied: Bash (too risky)"
	if strings.Join(got, "|") != want {
		t.Errorf("timeline:\n got %q\nwant %q", strings.Join(got, "|"), want)
	}
}
