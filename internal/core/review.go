package core

import (
	"fmt"
	"regexp"
	"strings"
)

// Implement + review pairing (spec §6.1a): on a Ticket paired with review, the implementer's ready report is held
// while a read-only review session (RoleReview, on the other runtime when the Executor has one) reads the branch and
// gives a closed verdict with findings. approved applies the held report as if the implementer had filed it then;
// changes sends the findings back to the implementer's own session, for at most PairingDefaults.MaxRounds rounds,
// after which the held report applies anyway. A review that cannot finish applies it too: review never asks a person.

const (
	// PairingReview pairs a Ticket's implementer with a review of each ready report (Ticket.Pairing).
	PairingReview = "review"
	// PairingOff, in TaskOptions.Pairing, pairs a task with nothing whatever the default says.
	PairingOff = "off"
	// defaultReviewRounds is PairingDefaults.MaxRounds when it names none.
	defaultReviewRounds = 2
)

// The verdicts of a review: the reviewer's approved or changes, and failed for a review that could not finish.
const (
	ReviewApproved = "approved"
	ReviewChanges  = "changes"
	ReviewFailed   = "failed"
)

const (
	// ReasonReview is the EndedReason of an implement Assignment whose ready report waits for a review.
	ReasonReview = "review"
	// ReasonReviewed is the EndedReason of a review Assignment once its verdict, or its failure, is in.
	ReasonReviewed = "reviewed"
)

// PairingDefaults are the Policy's pairing of the Tickets that name none: Mode is PairingReview or "" (none), and
// MaxRounds bounds a review's rounds (2 when not positive).
type PairingDefaults struct {
	Mode      string
	MaxRounds int
}

// ReviewState is a review in progress: its round (1 for the first), the implement Assignment whose ready report it
// holds, the runtime that Assignment ran on, and the report itself.
type ReviewState struct {
	Round       int    `json:"round"`
	Implementer string `json:"implementer"`
	Runtime     string `json:"runtime,omitempty"`
	Held        Report `json:"held"`
}

// ReviewOutcome is what a review concluded: its Verdict (approved, changes or failed) and its first finding line
// (ReviewFailed for a review that could not finish).
type ReviewOutcome struct {
	Verdict string `json:"verdict"`
	First   string `json:"first"`
}

// findingRe is one line of a review's findings: a severity-tagged finding at a file and line, or "- none".
var findingRe = regexp.MustCompile(`^- (\[(high|medium|low)\] \S+:\d+ — .+|none)$`)

// CheckReview reports why a reviewer's verdict and findings cannot be filed, as a UserError the reviewer reads and
// corrects; nil when they can. The verdict is approved or changes; findings are lines "- [high|medium|low] <file>:<line>
// — <what>", or the one line "- none". A high or medium finding needs changes, and "- none" needs approved.
func CheckReview(verdict, findings string) error {
	if verdict != ReviewApproved && verdict != ReviewChanges {
		return userErrorf("verdict %q is not %s or %s", verdict, ReviewApproved, ReviewChanges)
	}
	lines := findingLines(findings)
	if len(lines) == 0 {
		return userErrorf("findings are empty: list each as \"- [high|medium|low] <file>:<line> — <what>\", or write \"- none\"")
	}
	serious, none := false, false
	for _, l := range lines {
		m := findingRe.FindStringSubmatch(l)
		if m == nil {
			return userErrorf("finding %q is not \"- [high|medium|low] <file>:<line> — <what>\" or \"- none\" (one finding per line, no blank lines)", l)
		}
		switch m[2] {
		case "high", "medium":
			serious = true
		case "":
			none = true
		}
	}
	switch {
	case none && len(lines) > 1:
		return userErrorf("\"- none\" stands alone: with findings, list only them")
	case none && verdict != ReviewApproved:
		return userErrorf("no findings is an %s verdict", ReviewApproved)
	case serious && verdict != ReviewChanges:
		return userErrorf("a high or medium finding needs the %s verdict", ReviewChanges)
	}
	return nil
}

// findingLines are the lines of findings, the whole trimmed and each line's ends trimmed.
func findingLines(findings string) []string {
	f := strings.TrimSpace(findings)
	if f == "" {
		return nil
	}
	lines := strings.Split(f, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return lines
}

// validPairing reports whether name may be a TaskOptions.Pairing: "", PairingReview or PairingOff.
func validPairing(name string) bool { return name == "" || name == PairingReview || name == PairingOff }

// ticketPairing is the Ticket.Pairing of a task that asked for name: the default's mode for "", none for PairingOff.
func (o *op) ticketPairing(name string) string {
	switch name {
	case "":
		return o.pairingDefaults().Mode
	case PairingOff:
		return ""
	}
	return name
}

// pairingDefaults are the Engine's Options.Pairing, with a MaxRounds of at least 1; none without them.
func (o *op) pairingDefaults() PairingDefaults {
	var d PairingDefaults
	if o.pairing != nil {
		d = o.pairing()
	}
	if d.Mode != PairingReview {
		d.Mode = ""
	}
	if d.MaxRounds <= 0 {
		d.MaxRounds = defaultReviewRounds
	}
	return d
}

// holdsForReview reports whether a's ready report waits for a review: a is the Ticket's implementer, the Ticket is
// paired with review and its rounds are not used up.
func (o *op) holdsForReview(t *Ticket, a *Assignment) bool {
	return a.Role != RoleReview && t.Pairing == PairingReview && t.Stats.ReviewRounds < o.pairingDefaults().MaxRounds
}

// startReview holds the ready report just recorded on the implementer a and hands it to a review: a ends with
// ReasonReview and stops, and a pending review Assignment starts, for the Executor to place (unpinned: it prefers the
// runtime the implementer did not run on). Nothing is said to anyone yet: no Event of the report, no relay, no end.
func (o *op) startReview(t *Ticket, a *Assignment) {
	t.Review = &ReviewState{Round: t.Stats.ReviewRounds + 1, Implementer: a.ID, Runtime: implementerRuntime(t, a),
		Held: *a.Report}
	o.endAssignment(t, a, ReasonReview)
	review := o.newAssignment(t)
	findAssignment(t, review).Role = RoleReview
	o.stop(t, a.ID, ReasonReview)
	o.start(t, review)
}

// implementerRuntime is the runtime the implement Assignment a ran on: its own, else the Ticket's when it names one.
func implementerRuntime(t *Ticket, a *Assignment) string {
	if a.Runtime != "" {
		return a.Runtime
	}
	if x := t.Execution; x != nil && (x.Runtime == RuntimeClaude || x.Runtime == RuntimeCodex) {
		return x.Runtime
	}
	return ""
}

// FileReview records the verdict of the review Assignment that runs the open Ticket: verdict approved or changes and
// its findings (CheckReview; a violation is a UserError the reviewer corrects). The review ends with ReasonReviewed and
// its outcome, and its round counts. On approved, or once the rounds are used up, the held ready report is applied as
// if the implementer filed it now (applyHeld); on changes, a new pending implement Assignment resumes the implementer's
// session with the findings as an InputReview Input, and its next ready report starts the next round.
func (e *Engine) FileReview(ticketID, assignmentID, verdict, findings string) error {
	if err := CheckReview(verdict, findings); err != nil {
		return err
	}
	lines := findingLines(findings)
	return e.command(cmdFileReview, func(o *op) error {
		t, err := o.require(ticketID)
		if err != nil {
			return err
		}
		if t.Ended != nil {
			return userErrorf("ticket %s has ended", ticketID)
		}
		a := findAssignment(t, assignmentID)
		switch {
		case a == nil:
			return userErrorf("ticket %s has no assignment %s", ticketID, assignmentID)
		case a.Role != RoleReview || t.Review == nil:
			return userErrorf("assignment %s of ticket %s is no review", assignmentID, ticketID)
		case a.State != AssignmentRunning:
			return userErrorf("review %s of ticket %s is not running", assignmentID, ticketID)
		}
		held := o.endReview(t, a, ReviewOutcome{Verdict: verdict, First: lines[0]})
		if verdict == ReviewApproved || t.Stats.ReviewRounds >= o.pairingDefaults().MaxRounds {
			if err := o.applyHeld(t, held); err != nil {
				return err
			}
			return o.tx.PutTicket(t)
		}
		next := o.newAssignment(t)
		n := findAssignment(t, next)
		if x := t.Execution; x != nil && (x.Runtime == RuntimeClaude || x.Runtime == RuntimeCodex) {
			// An auto Ticket's is left to the Executor, whose warm rule keeps the implementer's runtime (F-26).
			n.Runtime = held.Runtime
		}
		runtime := a.Runtime
		if runtime == "" {
			runtime = "unknown runtime"
		}
		o.requeueUntaken(t, held.Implementer)
		in := Input{ID: newID(), Kind: InputReview, At: o.at,
			Text: fmt.Sprintf("Review (round %d, %s) requested changes:\n%s", held.Round, runtime, strings.Join(lines, "\n"))}
		t.Inputs = append(t.Inputs, in)
		t.Undelivered = append(t.Undelivered, in.ID)
		o.start(t, next)
		return o.tx.PutTicket(t)
	})
}

// endReview ends the review Assignment a with outcome: it records the outcome, counts the round, clears the review in
// progress and stops a's session; it returns the review that was in progress.
func (o *op) endReview(t *Ticket, a *Assignment, outcome ReviewOutcome) ReviewState {
	a.ReviewOutcome = &outcome
	held := *t.Review
	t.Review = nil
	t.Stats.ReviewRounds++
	t.UpdatedAt = o.at
	o.endAssignment(t, a, ReasonReviewed)
	o.event(EventReviewFiled, t, a.ID, map[string]any{"verdict": outcome.Verdict, "round": held.Round})
	o.stop(t, a.ID, ReasonReviewed)
	return held
}

// reviewFailed is a review that could not finish: its session reported failed or blocked (it crashed past its
// restarts, its launch failed, or it ended its turn twice without a verdict). The review ends with the failed outcome
// and the held ready report is applied, as on approved: review never asks a person.
func (o *op) reviewFailed(t *Ticket, a *Assignment) error {
	if t.Review == nil {
		return userErrorf("assignment %s of ticket %s reviews nothing", a.ID, t.ID)
	}
	held := o.endReview(t, a, ReviewOutcome{Verdict: ReviewFailed, First: ReviewFailed})
	return o.applyHeld(t, held)
}

// applyHeld applies a review's held ready report as if the implementer had filed it now: on a new implement Assignment
// that continues the implementer's session (its runtime, session and model) already running and idle, the report filed
// now, then what a ready report brings (settleReport): the report's Event and relay, and the Ticket's end when it
// follows no Threads or is done with them. Otherwise the Ticket waits handed off (HandedOff) for its Threads, and the
// next Input resumes the session as a paused one; one that waits already is delivered now.
func (o *op) applyHeld(t *Ticket, held ReviewState) error {
	next := o.newAssignment(t)
	n := findAssignment(t, next)
	n.State = AssignmentRunning
	n.StartedAt, n.IdleAt = o.timestamp(), o.timestamp()
	n.Runtime = held.Runtime
	if impl := findAssignment(t, held.Implementer); impl != nil {
		n.SessionID, n.Model = impl.SessionID, impl.Model
	}
	report := held.Held
	report.At = o.at
	n.Report = &report
	o.requeueUntaken(t, held.Implementer)
	if err := o.settleReport(t, n, nil); err != nil {
		return err
	}
	if t.Ended == nil && len(t.Undelivered) > 0 {
		o.deliver(t, n.ID)
	}
	return nil
}
