package node

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

// A review session (implement + review pairing, core/review.go) is a read-only session of a RoleReview Assignment, on
// the other runtime when the chooser has one ready (HeadroomChooser). It starts fresh with the reviewer prompt and its
// own first message (reviewBrief), never the worker preamble, a handoff note or the Ticket's Inputs; its sandbox and
// the hook rules let it write nothing (RuleContext.ReadOnly), and it asks no person for approval. Its MCP server
// offers only the review tool (`athena mcp --ticket <id> --role review`), which files its verdict (handleReview).

const (
	// OpReview files a review session's verdict; `athena mcp --role review` sends it for the review tool.
	OpReview = "review"
	// reviewNudgeText is sent, once, when a review session's turn ends without a verdict; reviewBlockedSummary is the
	// report filed when the next turn ends without one too, which the Engine takes as a review that could not finish.
	reviewNudgeText      = "Your turn ended without a review. Call the review tool now."
	reviewBlockedSummary = "reviewer ended its turn twice without a review"
	// reviewStatMax caps the diff stat in a review's first message.
	reviewStatMax = 8 << 10
	// reviewReportMax caps the implementer's report summary in it.
	reviewReportMax = 4 << 10
)

// ReviewRequest files the verdict and findings of the review session of Ticket and Assignment.
type ReviewRequest struct {
	Op         string `json:"op"`
	Ticket     string `json:"ticket"`
	Assignment string `json:"assignment"`
	Verdict    string `json:"verdict"`
	Findings   string `json:"findings"`
}

// reviewing reports whether the session reviews: its Assignment's role is RoleReview.
func (s *session) reviewing() bool { return s.a.Role == core.RoleReview }

// nudge is the message that asks the session for its report (a worker's) or its verdict (a review's), and the summary
// the node files when a second turn ends without one.
func (s *session) nudge() (text, summary string) {
	if s.reviewing() {
		return reviewNudgeText, reviewBlockedSummary
	}
	return nudgeText, blockedSummary
}

// reviewBrief is a review session's first message: the Ticket, the round, where the change is (its branch and base,
// and git diff --stat <base>...HEAD), and the implementer's ready report that waits for the verdict. Everything the
// worker or git wrote is quoted, so none of it reads as a line the node writes.
func (r *runner) reviewBrief(t core.Ticket) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the reviewer of Athena Ticket %s: %s\n\n", t.ID, oneLine(t.Title))
	round, summary := 0, ""
	if rv := t.Review; rv != nil {
		round, summary = rv.Round, rv.Held.Summary
	}
	fmt.Fprintf(&b, "Review round: %d\nDirectory: %s\n", round, r.s.place.workDir)
	base, branch, stat := r.n.reviewGit(r.s)
	if branch == "" {
		b.WriteString("The directory is no git worktree: review its files as they are.\n")
	} else {
		fmt.Fprintf(&b, "Branch: %s\nBase: %s\n\nThe change (git diff --stat %s...HEAD):\n%s\n", oneLine(branch), orNone(base),
			orNone(base), quoteText(orNone(keepStart(stat, reviewStatMax))))
	}
	fmt.Fprintf(&b, "\nThe implementer's ready report:\n%s\n\nRead the change, then call the review tool with your verdict "+
		"and findings.\n", quoteText(orNone(keepStart(summary, reviewReportMax))))
	return b.String()
}

// reviewGit is what a review's first message says of the session's worktree: the commit its branch is measured from,
// the branch, and the diff stat of the branch against that base; all "" outside a git worktree, and a part git cannot
// tell is "" (logged).
func (n *Node) reviewGit(s *session) (base, branch, stat string) {
	p := s.place
	if p.repo == "" {
		return "", "", ""
	}
	env, err := n.worktreeEnv(p, s.t)
	if err != nil {
		s.log.Warn("review: no git state", "error", err)
		return "", "", ""
	}
	ctx, cancel := context.WithTimeout(n.ctx, handoffGitTimeout)
	defer cancel()
	git := func(args ...string) string {
		out, err := gitInCtx(ctx, p.workDir, env, args...)
		if err != nil {
			s.log.Warn("review: git", "args", strings.Join(args, " "), "error", err)
		}
		return out
	}
	branch = git("rev-parse", "--abbrev-ref", "HEAD")
	if base = n.branchBase(ctx, p, env); base != "" {
		stat = git("diff", "--stat", base+"...HEAD")
	}
	return base, branch, stat
}

// handleReview files a review session's verdict (the review tool): only the live review session of the Ticket and
// Assignment gives one, and not in a step whose other tool call failed unseen. The Engine checks the verdict and
// findings (core.CheckReview); its refusal comes back as the tool's error, for the reviewer to correct.
func (n *Node) handleReview(req ReviewRequest) Response {
	if n.reg != nil {
		start := time.Now()
		defer func() { n.reg.Histogram("mcp_tool_seconds", "tool", "review").Observe(time.Since(start)) }()
	}
	s := n.lookup(req.Ticket, req.Assignment)
	switch {
	case s == nil:
		return Response{Error: fmt.Sprintf("%s: ticket %q assignment %q", denyNoSession, req.Ticket, req.Assignment)}
	case !s.reviewing():
		return Response{Error: "only a review session gives a review verdict"}
	case s.hasUnseenFailure():
		s.log.Warn("review refused", "error", "tool call failed in the same step")
		return Response{Error: "a tool call of yours failed in the step that filed this review, so you cannot have seen its " +
			"result; read the result, then review again"}
	}
	if err := n.engine().FileReview(req.Ticket, req.Assignment, req.Verdict, req.Findings); err != nil {
		s.log.Warn("review refused", "verdict", req.Verdict, "error", err)
		return Response{Error: err.Error()}
	}
	s.noteReport()
	s.log.Info("review filed", "verdict", req.Verdict)
	return Response{OK: true}
}
