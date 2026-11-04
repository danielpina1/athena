package hub

import (
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
