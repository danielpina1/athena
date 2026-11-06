package node

import (
	"context"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/policy"
)

// allowAllPolicy is a Policy whose allow rules match every action, as a Policy would that was handed rules Validate
// refuses; its tool policy for the owner's class is as given.
type allowAllPolicy struct{ tools string }

func (p allowAllPolicy) ToolPolicy(string) string { return p.tools }
func (allowAllPolicy) Allows(_, _, _ string) bool { return true }
func (allowAllPolicy) Shadow(string) bool         { return false }
func (allowAllPolicy) Trust(core.Trigger) string  { return core.TrustOwner }

func setRepo(s *session, repo string) {
	s.mu.Lock()
	s.t.Repo = repo
	s.mu.Unlock()
}

func TestAllowRuleSkipsRestrictedAndCredential(t *testing.T) {
	deploy := map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "make deploy"}}

	// A standard session: the standing rule lets the escalation through with no Approval, and says so on the Ticket.
	h := newHarness(t, func(c *Config) { c.Policy = allowAllPolicy{tools: policy.ToolStandard} })
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	setRepo(h.n.liveSession(tk.ID), "acme/web")
	r := h.hook(EventPreToolUse, tk.ID, a.ID, deploy)
	if r.Verdict != VerdictAllow {
		t.Fatalf("allowed escalation = %+v, want allow", r)
	}
	if pending, _ := h.eng.PendingApprovals(); len(pending) != 0 {
		t.Fatalf("pending approvals = %+v, want none", pending)
	}
	if cur, _ := h.ticket(tk.ID); cur.Stats.Escalations != 0 {
		t.Errorf("escalations = %d, want 0 for an allowed call", cur.Stats.Escalations)
	}

	// A rule no allow rule may name still asks, even if the Policy says otherwise.
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	s := h.n.liveSession(tk.ID)
	got := make(chan HookResponse, 1)
	go func() {
		got <- h.n.escalate(ctx, s, EventPreToolUse, approvalAsk{tool: "Bash", d: escalate(ruleCredential, "reads a credential"), summary: "cat key"}, time.Now())
	}()
	var pending []core.Approval
	h.eventually("credential-read asks", func() bool {
		pending, _ = h.eng.PendingApprovals()
		return len(pending) == 1
	})
	if pending[0].Rule != ruleCredential {
		t.Errorf("approval = %+v, want credential-read", pending[0])
	}
	if err := h.eng.AnswerApproval(pending[0].ID, false, ""); err != nil {
		t.Fatal(err)
	}
	if r := <-got; r.Verdict != VerdictDeny {
		t.Errorf("credential-read answer = %+v, want deny", r)
	}

	// A restricted session never takes an allow rule: its escalation is denied and nobody is asked.
	hr := newHarness(t, func(c *Config) { c.Policy = allowAllPolicy{tools: policy.ToolRestricted} })
	rtk := hr.submit("Stranger's task", "")
	_, ra := hr.running(rtk.ID)
	setRepo(hr.n.liveSession(rtk.ID), "acme/web")
	if r := hr.hook(EventPreToolUse, rtk.ID, ra.ID, deploy); r.Verdict != VerdictDeny {
		t.Errorf("restricted escalation = %+v, want deny", r)
	}
	rs := hr.n.liveSession(rtk.ID)
	ask := approvalAsk{tool: "Bash", d: escalate("bash-escalate:deploy", "x"), summary: "make deploy"}
	if ok, _ := hr.n.allowedByRule(rs, ask); ok {
		t.Error("allowedByRule on a restricted session = true")
	}
	if r := hr.n.escalateShared(ctx, rs, EventPreToolUse, "", "", ask, time.Now()); r.Verdict != VerdictDeny || r.Reason != restrictedEscalateReason {
		t.Errorf("escalateShared on a restricted session = %+v, want the restricted deny", r)
	}
	if pending, _ := hr.eng.PendingApprovals(); len(pending) != 0 {
		t.Errorf("restricted session left approvals: %+v", pending)
	}
}

// allowSet is a Policy whose allow rules are exactly the listed rule names, for Bash and every other tool.
type allowSet map[string]bool

func (allowSet) ToolPolicy(string) string        { return policy.ToolStandard }
func (a allowSet) Allows(_, rule, _ string) bool { return a[rule] }
func (allowSet) Shadow(string) bool              { return false }
func (allowSet) Trust(core.Trigger) string       { return core.TrustOwner }

// A call that triggers several escalations is let through only when a standing rule allows every one of them.
func TestAllowRuleNeedsEveryEscalation(t *testing.T) {
	e := newRuleEnv(t)
	sess := &session{t: core.Ticket{Repo: "acme/web"}, toolPolicy: policy.ToolStandard}
	bash := func(cmd string) (Decision, ToolCall) {
		return EvaluateCalls(e.ctx, core.RuntimeClaude, []ToolCall{{Tool: "Bash", Input: in(map[string]any{"command": cmd})}})
	}
	cases := []struct {
		name    string
		d       Decision
		allowed map[string]bool
		want    bool
	}{}
	add := func(name string, d Decision, allowed map[string]bool, want bool) {
		cases = append(cases, struct {
			name    string
			d       Decision
			allowed map[string]bool
			want    bool
		}{name, d, allowed, want})
	}
	settings, _ := bash("echo x > ../elsewhere; echo '{}' > .claude/settings.json")
	add("write outside plus session-config", settings, map[string]bool{ruleWrite: true}, false)
	force, _ := bash("echo x > ../y && make deploy")
	add("write outside plus deploy", force, map[string]bool{ruleWrite: true}, false)
	add("write outside plus deploy, only the push allowed", force, map[string]bool{"bash-escalate:deploy": true}, false)
	add("write outside plus deploy, both allowed", force, map[string]bool{ruleWrite: true, "bash-escalate:deploy": true}, true)
	two, _ := bash("make deploy && pip install requests")
	add("two risky commands, the first allowed", two, map[string]bool{"bash-escalate:deploy": true}, false)
	add("two risky commands, both allowed", two, map[string]bool{"bash-escalate:deploy": true, "bash-escalate:pip-install": true}, true)
	patchIn := in(map[string]any{"command": "*** Begin Patch\n*** Add File: ../a.txt\n+x\n*** Update File: .claude/settings.json\n@@\n-x\n+y\n*** End Patch\n"})
	pd, _ := EvaluateCalls(e.ctx, core.RuntimeCodex, NormalizeTool(core.RuntimeCodex, "apply_patch", patchIn))
	add("apply_patch with two paths, one class allowed", pd, map[string]bool{ruleWrite: true}, false)
	add("apply_patch with two paths, both allowed", pd, map[string]bool{ruleWrite: true, ruleSessionConfig: true}, false) // never promoted
	for _, c := range cases {
		if c.d.Verdict != VerdictEscalate {
			t.Errorf("%s: decision %+v, want an escalation", c.name, c.d)
			continue
		}
		n := New(Config{Policy: allowSet(c.allowed)}, nil)
		if got, _ := n.allowedByRule(sess, approvalAsk{tool: "Bash", d: c.d}); got != c.want {
			t.Errorf("%s: allowed = %v, want %v (decision %+v)", c.name, got, c.want, c.d)
		}
	}
}
