package rt

import (
	"encoding/json"
	"time"
)

// Event is one thing a session reported.
type Event interface{ isEvent() }

// Init is the session's start: the runtime's session id (the one to resume) and the model.
type Init struct{ SessionID, Model string }

// AssistantText is a block of text from the model.
type AssistantText struct{ Text string }

// ToolUse is the model asking to run a tool.
type ToolUse struct {
	ID, Name string
	Input    json.RawMessage
}

// ToolResult reports the outcome of the ToolUse with the same ID.
type ToolResult struct {
	ID      string
	IsError bool
}

// UserEcho is a user message the session took into its conversation: its text and the uuid it was sent with (empty
// when none came back).
type UserEcho struct{ Text, UUID string }

// TurnEnd ends a turn. OK is false for a turn that failed (an API error, an expired login, a limit, too many turns);
// Subtype, Text and Errors say why.
type TurnEnd struct {
	SessionID string
	OK        bool
	Subtype   string
	// Text is the turn's final message; Errors what the runtime said went wrong.
	Text   string
	Errors []string
	// CostUSD is the session's cumulative cost so far (it carries across resumed processes).
	CostUSD float64
	// InputTokens, OutputTokens and CacheReadTokens are this turn's, not the session's. InputTokens leaves out
	// CacheReadTokens (what was read from the cache is counted once, there), whichever runtime ran: Codex reports its
	// cached tokens inside its input, so its driver subtracts them.
	InputTokens, OutputTokens, CacheReadTokens int64
	NumTurns                                   int
	Duration                                   time.Duration
	// Limit is set when the turn failed because a usage limit was reached.
	Limit *LimitHit
}

// QuotaUpdate is what the runtime said of its usage limits.
type QuotaUpdate struct{ Windows []Window }

// Window is one usage-limit window.
type Window struct {
	// Bucket names the limit the window belongs to when the runtime keeps several (Codex's limitId: "codex", or a
	// per-model limit's id); empty for a runtime with one (Claude Code). Two buckets' windows of the same Name are two
	// windows.
	Bucket string
	// Name is "5h", "7d" or "<n>m" (a window of n minutes); "primary" or "secondary" for a window whose length the
	// runtime did not say.
	Name string
	// Used is the fraction of the window used, in [0, 1].
	Used float64
	// ResetsAt is when the window resets, nil when not known.
	ResetsAt *time.Time
	// Limited: the runtime refuses work until the window resets.
	Limited bool
}

// LimitHit is a usage limit that stopped a turn.
type LimitHit struct {
	Runtime  string
	ResetsAt *time.Time
	Message  string
	// Transient: the limit is short (a rate of requests), not a used-up window.
	Transient bool
}

// Unknown is anything the runtime reported that the node does not model; Kind names it.
type Unknown struct{ Kind string }

func (Init) isEvent()          {}
func (AssistantText) isEvent() {}
func (ToolUse) isEvent()       {}
func (ToolResult) isEvent()    {}
func (UserEcho) isEvent()      {}
func (TurnEnd) isEvent()       {}
func (QuotaUpdate) isEvent()   {}
func (Unknown) isEvent()       {}
