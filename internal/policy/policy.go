package policy

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

// Strengths.
const (
	explicit = "explicit"
	inferred = "inferred"
)

// Decision kinds an assessment produces.
const (
	kindAccept = "accept"
	kindIgnore = "ignore"
)

// Reason codes of the gate and of an assessment outcome. They are stable: panels and agents key on them.
const (
	// CodeSourceUnconfigured: the Policy has no part for the Trigger's Source.
	CodeSourceUnconfigured = "source_unconfigured"
	// CodeNotAPerson: the Trigger is neither a person's, a bot's nor a CI result.
	CodeNotAPerson = "not_a_person"
	// CodeUnknownScope: the Trigger's receiving scope has no admission row.
	CodeUnknownScope = "unknown_scope"
	// CodeNotAMember: the sender is not on the allowed list and the admission table ignores them.
	CodeNotAMember = "not_a_member"
	// CodeBotNotListed: the bot is not on the Source's bot list.
	CodeBotNotListed = "bot_not_listed"
	// CodeAdmissionIgnore: the admission table ignores a listed sender's or a CI result's message.
	CodeAdmissionIgnore = "admission_ignore"
	// CodeNotReceived: the message addresses nobody and its scope does not receive every message.
	CodeNotReceived = "unaddressed_not_received"
	// CodeModeOff: the case is inferred and the accept mode is off.
	CodeModeOff = "mode_off"

	// CodeNoMatchingRule: an assessment accepted, or the candidate was assessed, but no rule matches.
	CodeNoMatchingRule = "no_matching_rule"
	// CodeNoEvidence: a rule matches without evidence of intent.
	CodeNoEvidence = "no_evidence"
	// CodeWouldAccept: observe mode recorded an ignore of a candidate that auto mode would have accepted.
	CodeWouldAccept = "would_accept"
	// CodeSaid carries the words the assessor wrote, in Values["text"].
	CodeSaid = "said"
)

// metaReceiveAll marks a Trigger from a scope that receives every message, not only mentions.
const (
	metaReceiveAll = "receiveAll"
	valueTrue      = "true"
)

// policy is an immutable Policy over a private copy of the settings.
type policy struct {
	s       Settings
	scope   core.SourceScope
	timeout time.Duration
	version string
}

// New returns the Policy for the settings. The Policy copies them, so it does not change if s does. scope tells
// revalidation whether a Source still listens to a Thread; with a nil scope nothing is listened to.
func New(s Settings, scope core.SourceScope, evalTimeout time.Duration) core.Policy {
	n := normalize(s)
	return &policy{s: n, scope: scope, timeout: evalTimeout, version: Version(n)}
}

func (p *policy) Version() string            { return p.version }
func (p *policy) EvalTimeout() time.Duration { return p.timeout }

// WIPCeiling is the repository's ceiling: its own, else the settings', else DefaultWIPCeiling; never below 1.
func (p *policy) WIPCeiling(repo string) int {
	if c := p.s.Repos[strings.ToLower(repo)].WIPCeiling; c > 0 {
		return c
	}
	if p.s.WIPCeiling > 0 {
		return p.s.WIPCeiling
	}
	return DefaultWIPCeiling
}

// Shadow reports whether the repository is in shadow mode.
func (p *policy) Shadow(repo string) bool { return p.s.Repos[strings.ToLower(repo)].Mode == RepoShadow }

// Trust is filled in by the trust classification; until then no Trigger has a class.
func (p *policy) Trust(core.Trigger) string { return "" }

// ToolPolicy is the tool policy of a trust class: the owner's setting, else DefaultTrust; an empty class is standard.
func (p *policy) ToolPolicy(class string) string {
	if v, ok := p.s.Trust[class]; ok {
		return v
	}
	if v, ok := DefaultTrust()[class]; ok {
		return v
	}
	return ToolStandard
}

// Allows reports whether an allow rule matches the tool, rule and repository exactly. A never-promoted rule is
// never allowed.
func (p *policy) Allows(tool, rule, repo string) bool {
	if neverPromoted(rule) {
		return false
	}
	repo = strings.ToLower(repo)
	for _, a := range p.s.Allow {
		if a.Tool == tool && a.Rule == rule && a.Repo == repo {
			return true
		}
	}
	return false
}

// PromoteAfter is how many approvals promote a rule.
func (p *policy) PromoteAfter() int {
	if p.s.PromoteAfter > 0 {
		return p.s.PromoteAfter
	}
	return DefaultPromoteAfter
}

func (p *policy) AdmitsInferred(r []string) bool { return admitsInferred(p.s, r) }

func (p *policy) Assess(a core.Assessment, strength string) (string, []core.Reason, bool, error) {
	return AssessmentOutcome(a, p.s, strength)
}

func rank(strength string) int {
	switch strength {
	case explicit:
		return 2
	case inferred:
		return 1
	}
	return 0
}

func cellStrength(choice string) string {
	switch choice {
	case ChoiceExplicit:
		return explicit
	case ChoiceInferred:
		return inferred
	}
	return ""
}

func inList(list []string, login string) bool {
	for _, v := range list {
		if strings.EqualFold(v, login) {
			return true
		}
	}
	return false
}

// rowFor returns the admission row of a scope in the part.
func rowFor(part SourcePart, scope string) (Row, bool) {
	r, ok := part.Admission[scope]
	return r, ok && knownScope(scope)
}

// followsATicket reports whether the Thread has a non-context Subscription to a Ticket that is open, or ended and
// not stopped by a person.
func followsATicket(th core.Thread, tickets []core.Ticket) bool {
	for _, sub := range th.Subscriptions {
		if sub.Context {
			continue
		}
		for i := range tickets {
			t := &tickets[i]
			if t.ID != sub.TicketID {
				continue
			}
			if t.Ended == nil || (t.Ended.By != core.EndByHuman && t.Ended.Outcome != core.OutcomeStopped) {
				return true
			}
		}
	}
	return false
}

func (p *policy) Gate(t core.Trigger, th core.Thread, tickets []core.Ticket) core.Gate {
	part, ok := p.s.Sources[t.Source]
	if !ok {
		return p.refuse(t, CodeSourceUnconfigured, nil)
	}
	basis := core.MandateBasis{Source: t.Source, Scope: t.Scope, Sender: t.Actor.ID}
	receiveAll := t.Meta[metaReceiveAll] == valueTrue
	addressed := t.Addressed != ""

	switch {
	case t.Kind == core.KindCI || t.Scope == ScopeCI:
		// A CI result has no sender: only the last cell of its row applies.
		row, ok := rowFor(part, ScopeCI)
		if !ok {
			return p.refuse(t, CodeUnknownScope, nil)
		}
		basis.Scope, basis.Sender = ScopeCI, ""
		return p.fromCell(t, basis, CellUnlistedUnaddressed, row.UnlistedUnaddressed, receiveAll, false)

	case t.Bot:
		// Bots use the bot list, never the cells or follow-up strength, and are never explicit.
		if !inList(part.Bots, t.Actor.ID) {
			return p.refuse(t, CodeBotNotListed, nil)
		}
		if !addressed && !receiveAll {
			return p.refuse(t, CodeNotReceived, nil)
		}
		basis.Via = core.ViaBot
		return p.admit(t, inferred, basis)

	case t.Human:
		return p.gatePerson(t, th, tickets, part, basis, addressed, receiveAll)
	}
	return p.refuse(t, CodeNotAPerson, nil)
}

func (p *policy) gatePerson(t core.Trigger, th core.Thread, tickets []core.Ticket, part SourcePart,
	basis core.MandateBasis, addressed, receiveAll bool) core.Gate {
	listed := inList(part.Allowed, t.Actor.ID)

	// An allowed person who addresses the agent makes an explicit request, in every mode.
	if listed && addressed {
		basis.Via = core.ViaAddressed
		return p.admit(t, explicit, basis)
	}
	row, rowOK := rowFor(part, t.Scope)

	// For an unlisted person an explicit cell outranks follow-up strength and works in every mode.
	if !listed && addressed && rowOK && row.UnlistedAddressed == ChoiceExplicit {
		basis.Via, basis.Row = core.ViaAdmission, CellUnlistedAddressed
		return p.admit(t, explicit, basis)
	}

	// A follow-up on a Thread with Tickets: explicit for an allowed person, inferred for anyone else.
	if followsATicket(th, tickets) {
		basis.Via = core.ViaFollowup
		if listed {
			return p.admit(t, explicit, basis)
		}
		return p.admit(t, inferred, basis)
	}

	// Anything else takes the admission table (including Threads followed only for context).
	if !rowOK {
		return p.refuse(t, CodeUnknownScope, nil)
	}
	basis.Via = core.ViaAdmission
	switch {
	case !listed && addressed:
		return p.fromCell(t, basis, CellUnlistedAddressed, row.UnlistedAddressed, receiveAll, true)
	case listed:
		return p.fromCell(t, basis, CellListedUnaddressed, row.ListedUnaddressed, receiveAll, false)
	default:
		return p.fromCell(t, basis, CellUnlistedUnaddressed, row.UnlistedUnaddressed, receiveAll, true)
	}
}

// fromCell admits a Trigger by the value of one admission cell. The cells for unaddressed messages need a scope that
// receives every message. unlisted says the sender is a person not on the list, for the refusal's code.
func (p *policy) fromCell(t core.Trigger, basis core.MandateBasis, cell, choice string, receiveAll, unlisted bool) core.Gate {
	vals := map[string]string{"cell": cell, "scope": t.Scope}
	if cell != CellUnlistedAddressed && !receiveAll {
		return p.refuse(t, CodeNotReceived, vals)
	}
	strength := cellStrength(choice)
	if strength == "" {
		if unlisted {
			return p.refuse(t, CodeNotAMember, vals)
		}
		return p.refuse(t, CodeAdmissionIgnore, vals)
	}
	basis.Via, basis.Row = core.ViaAdmission, cell
	return p.admit(t, strength, basis)
}

// inferredAllowed reports whether the accept mode lets inferred cases through as candidates.
func inferredAllowed(mode string) bool { return mode == ModeObserve || mode == ModeAuto }

// admit returns the admission of a Trigger; an inferred case is not admitted while the accept mode is off.
func (p *policy) admit(t core.Trigger, strength string, basis core.MandateBasis) core.Gate {
	if strength == inferred {
		if !inferredAllowed(p.s.Mode) {
			return p.refuse(t, CodeModeOff, nil)
		}
		// Observation marks a candidate admitted through an unaddressed cell: the Source receives such messages in
		// that scope. It does not depend on the accept mode.
		basis.Observation = basis.Via == core.ViaAdmission &&
			(basis.Row == CellListedUnaddressed || basis.Row == CellUnlistedUnaddressed)
	}
	return core.Gate{Strength: strength, Basis: basis}
}

func (p *policy) refuse(t core.Trigger, code string, values map[string]string) core.Gate {
	return core.Gate{Reason: core.Reason{Code: code, Values: values, TriggerID: t.ID}}
}

// admitsInferred: proactive work is accepted only in auto mode, for at least one rule, all of them enabled.
func admitsInferred(s Settings, rules []string) bool {
	if s.Mode != ModeAuto || len(rules) == 0 {
		return false
	}
	for _, name := range rules {
		if r := findRule(s.Rules, name); r == nil || !r.Enabled {
			return false
		}
	}
	return true
}

func findRule(rules []Rule, name string) *Rule {
	for i := range rules {
		if rules[i].Name == name {
			return &rules[i]
		}
	}
	return nil
}

func (p *policy) Revalidate(m core.Mandate) bool {
	if m.Basis.Via == core.ViaOwner {
		return true // an owner task needs no Source
	}
	if p.scope == nil || m.Thread == "" || !p.scope.Listens(m.Thread) {
		return false
	}
	if !requesterHolds(p.s, m) {
		return false
	}
	// Anything that is not an explicit Mandate rests on inference.
	return m.Strength == explicit || admitsInferred(p.s, m.Basis.Rules)
}

// requesterHolds reports whether the Mandate's basis about its requester still holds under s: the sender is still
// allowed, the table still gives at least the original strength, the bot is still listed. It ignores the accept mode
// and the rules.
func requesterHolds(s Settings, m core.Mandate) bool {
	if m.Basis.Via == core.ViaOwner {
		return true
	}
	src := m.Basis.Source
	if src == "" {
		src = m.Actor.Source
	}
	part, ok := s.Sources[src]
	if !ok {
		return false
	}
	sender := m.Basis.Sender
	if sender == "" {
		sender = m.Actor.ID
	}
	switch m.Basis.Via {
	case core.ViaFollowup:
		return true
	case core.ViaAddressed:
		return inList(part.Allowed, sender)
	case core.ViaBot:
		return inList(part.Bots, sender)
	case core.ViaAdmission:
		need := rank(m.Strength)
		return need > 0 && rank(cellToday(part, m.Basis, sender)) >= need
	}
	return false
}

// cellToday is the strength the admission table gives today to the kind of message a Mandate was admitted by.
func cellToday(part SourcePart, b core.MandateBasis, sender string) string {
	row, ok := rowFor(part, b.Scope)
	if !ok {
		return ""
	}
	if b.Scope == ScopeCI {
		return cellStrength(row.UnlistedUnaddressed)
	}
	listed := inList(part.Allowed, sender)
	switch b.Row {
	case CellUnlistedAddressed:
		if listed {
			return explicit // an allowed person addressing the agent
		}
		return cellStrength(row.UnlistedAddressed)
	case CellListedUnaddressed, CellUnlistedUnaddressed:
		if listed {
			return cellStrength(row.ListedUnaddressed)
		}
		return cellStrength(row.UnlistedUnaddressed)
	}
	return ""
}

// AssessmentOutcome converts an Assessment of a candidate of the given strength into a Decision kind (accept or
// ignore), its reasons and whether an ignore records "would accept". An error means the assessment failed and the
// Eval records unavailable.
//
// An explicit candidate follows the assessor. For an inferred one every enabled rule must be judged exactly once
// (and no other), a proactive accept needs a matching rule with evidence of intent, and only auto mode accepts:
// observe mode turns a positive assessment into an ignore marked would-accept, and off mode ignores it.
func AssessmentOutcome(a core.Assessment, s Settings, strength string) (string, []core.Reason, bool, error) {
	said := func() []core.Reason {
		if strings.TrimSpace(a.Reason) == "" {
			return nil
		}
		return []core.Reason{{Code: CodeSaid, Values: map[string]string{"text": a.Reason}}}
	}
	switch strength {
	case explicit:
		if a.Accept {
			return kindAccept, said(), false, nil
		}
		return kindIgnore, said(), false, nil
	case inferred:
	default:
		return "", nil, false, fmt.Errorf("policy: cannot assess a candidate of strength %q", strength)
	}

	if err := checkJudgements(a, s); err != nil {
		return "", nil, false, err
	}
	if !a.Accept {
		return kindIgnore, said(), false, nil
	}
	matched := false
	for _, j := range a.Rules {
		if !j.Matches {
			continue
		}
		matched = true
		if strings.TrimSpace(j.Evidence) == "" {
			return kindIgnore, []core.Reason{{Code: CodeNoEvidence, Values: map[string]string{"rule": j.Name}}}, false, nil
		}
	}
	if !matched {
		return kindIgnore, append([]core.Reason{{Code: CodeNoMatchingRule}}, said()...), false, nil
	}
	switch s.Mode {
	case ModeAuto:
		return kindAccept, said(), false, nil
	case ModeObserve:
		return kindIgnore, append([]core.Reason{{Code: CodeWouldAccept}}, said()...), true, nil
	}
	return kindIgnore, []core.Reason{{Code: CodeModeOff}}, false, nil
}

// checkJudgements requires exactly one judgement for each enabled rule and none for any other name.
func checkJudgements(a core.Assessment, s Settings) error {
	judged := map[string]bool{}
	for _, j := range a.Rules {
		r := findRule(s.Rules, j.Name)
		if r == nil || !r.Enabled {
			return fmt.Errorf("policy: the assessment judges %q, which is not an enabled rule", j.Name)
		}
		if judged[j.Name] {
			return fmt.Errorf("policy: the assessment judges rule %q twice", j.Name)
		}
		judged[j.Name] = true
	}
	var missing []string
	for _, r := range s.Rules {
		if r.Enabled && !judged[r.Name] {
			missing = append(missing, r.Name)
		}
	}
	if len(missing) > 0 {
		return errors.New("policy: the assessment does not judge rule " + strings.Join(missing, ", "))
	}
	return nil
}

// Effects returns, for each open Ticket that a settings change affects, the notice to send it: a rule its Mandate
// matched changed or no longer applies, its requester may no longer start work, or proactive work is no longer
// accepted because the accept mode left auto. Unaffected and ended Tickets are absent. It does not look at whether a
// Source still listens (a Source's configuration is separate from the Policy).
func Effects(tickets []core.Ticket, old, cur Settings) map[string]string {
	out := map[string]string{}
	old, cur = normalize(old), normalize(cur)
	if Version(old) == Version(cur) {
		return out
	}
	for i := range tickets {
		t := &tickets[i]
		if t.Ended != nil {
			continue
		}
		var notes []string
		for _, name := range t.Mandate.Basis.Rules {
			or := findRule(old.Rules, name)
			if or == nil || !or.Enabled {
				continue
			}
			nr := findRule(cur.Rules, name)
			switch {
			case nr == nil || !nr.Enabled:
				notes = append(notes, fmt.Sprintf("The rule %q that matched your request no longer applies.", name))
			case nr.Instructions != or.Instructions:
				notes = append(notes, fmt.Sprintf("The rule %q that matched your request changed. Its new text: %s", name, nr.Instructions))
			}
		}
		if requesterHolds(old, t.Mandate) && !requesterHolds(cur, t.Mandate) {
			notes = append(notes, "The person who requested this work may no longer start work under the current Policy.")
		}
		if t.Mandate.Strength != explicit && old.Mode == ModeAuto && cur.Mode != ModeAuto {
			notes = append(notes, fmt.Sprintf("Proactive work is no longer accepted: the accept mode changed from auto to %s.", cur.Mode))
		}
		if len(notes) > 0 {
			out[t.ID] = strings.Join(notes, "\n")
		}
	}
	return out
}
