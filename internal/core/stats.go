package core

import (
	"time"

	"github.com/danielpina1/athena/internal/metrics"
)

// Names of the Engine's commands, the values of the `command` label of engine_command_seconds.
const (
	cmdSubmit            = "submit"
	cmdAssignmentStarted = "assignment_started"
	cmdInputsDelivered   = "inputs_delivered"
	cmdDeliveryFailed    = "delivery_failed"
	cmdInputsRead        = "inputs_read"
	cmdTurnEnded         = "turn_ended"
	cmdFileReport        = "file_report"
	cmdPersonInput       = "person_input"
	cmdStop              = "stop"
	cmdRetry             = "retry"
	cmdRequestApproval   = "request_approval"
	cmdAnswerApproval    = "answer_approval"
	cmdExpireApproval    = "expire_approval"
	cmdRecordUsage       = "record_usage"
)

var commandNames = []string{
	cmdSubmit, cmdAssignmentStarted, cmdInputsDelivered, cmdDeliveryFailed, cmdInputsRead, cmdTurnEnded,
	cmdFileReport, cmdPersonInput, cmdStop, cmdRetry, cmdRequestApproval, cmdAnswerApproval, cmdExpireApproval,
	cmdRecordUsage,
}

// TicketStats are a Ticket's performance numbers (spec §10.3). The Engine updates them in the same transaction as the
// command that causes them; what only the executor can see reaches it through RecordUsage.
type TicketStats struct {
	// QueueWait is the time from each Assignment's creation to its session starting, summed: the wait for a slot plus
	// the launch.
	QueueWait time.Duration `json:"queueWait"`
	// TimeToFirstOutput is how long the worker took from its first message to its first output (the first one only).
	TimeToFirstOutput time.Duration `json:"timeToFirstOutput"`
	// TimeToFirstReport is the time from the Ticket's creation to the first Report of any Assignment.
	TimeToFirstReport time.Duration `json:"timeToFirstReport"`
	// ActiveTime is the sum of the worker's turn durations as its runtime reports them (an Approval awaited inside a
	// turn is part of it, and also of PersonWaitTime).
	ActiveTime time.Duration `json:"activeTime"`
	// PersonWaitTime is the time the Ticket waited for a person: a question from its being asked to its answer, an
	// Approval from its request to its answer or expiry.
	PersonWaitTime time.Duration `json:"personWaitTime"`

	Turns          int `json:"turns"`
	ToolCalls      int `json:"toolCalls"`
	ToolErrors     int `json:"toolErrors"`
	HookDenials    int `json:"hookDenials"`
	Escalations    int `json:"escalations"`
	Questions      int `json:"questions"`
	PersonMessages int `json:"personMessages"`
	Retries        int `json:"retries"`
	Crashes        int `json:"crashes"`

	InputTokens     int64 `json:"inputTokens"`
	OutputTokens    int64 `json:"outputTokens"`
	CacheReadTokens int64 `json:"cacheReadTokens"`

	CostUSD float64 `json:"costUsd"`
}

// CacheHitRatio is the share of input tokens served from the prompt cache: cache reads over cache reads plus uncached
// input; 0 without input.
func (s TicketStats) CacheHitRatio() float64 {
	total := s.InputTokens + s.CacheReadTokens
	if total <= 0 {
		return 0
	}
	return float64(s.CacheReadTokens) / float64(total)
}

// FirstTrySuccess reports whether the Ticket ended completed or merged without a retry, a crash or a human
// correction. A person's message counts as a correction unless it answered a question of the worker; messages are not
// matched to questions, so more messages than questions are corrections.
func FirstTrySuccess(t *Ticket) bool {
	if t.Ended == nil || (t.Ended.Outcome != OutcomeCompleted && t.Ended.Outcome != OutcomeMerged) {
		return false
	}
	s := t.Stats
	return s.Retries == 0 && s.Crashes == 0 && s.PersonMessages <= s.Questions
}

// UsageDelta is what an executor observed about a Ticket since its last report; RecordUsage adds it to the stats.
type UsageDelta struct {
	// TimeToFirstOutput counts only the first time it is set on a Ticket.
	TimeToFirstOutput time.Duration
	ActiveTime        time.Duration

	Turns       int
	ToolCalls   int
	ToolErrors  int
	HookDenials int
	Crashes     int

	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64

	CostUSD float64
}

// RecordUsage adds an executor's observations to the Ticket's stats in one transaction. It records them whether or not
// the Ticket has ended (a session spends tokens until its last turn), changes nothing else of the Ticket and emits
// no Event. assignmentID names the Assignment they came from.
func (e *Engine) RecordUsage(ticketID, assignmentID string, u UsageDelta) error {
	return e.command(cmdRecordUsage, func(o *op) error {
		t, err := o.require(ticketID)
		if err != nil {
			return err
		}
		s := &t.Stats
		if u.TimeToFirstOutput > 0 && s.TimeToFirstOutput == 0 {
			s.TimeToFirstOutput = u.TimeToFirstOutput
		}
		s.ActiveTime += u.ActiveTime
		s.Turns += u.Turns
		s.ToolCalls += u.ToolCalls
		s.ToolErrors += u.ToolErrors
		s.HookDenials += u.HookDenials
		s.Crashes += u.Crashes
		s.InputTokens += u.InputTokens
		s.OutputTokens += u.OutputTokens
		s.CacheReadTokens += u.CacheReadTokens
		s.CostUSD += u.CostUSD
		runtime, model := runtimeModel(t)
		if o.reg != nil {
			o.observe(func(r *metrics.Registry) {
				for _, k := range []struct {
					kind string
					n    int64
				}{{"input", u.InputTokens}, {"output", u.OutputTokens}, {"cache_read", u.CacheReadTokens}} {
					if k.n > 0 {
						r.Counter("tokens_total", "kind", k.kind, "runtime", runtime, "model", model).Add(k.n)
					}
				}
				if u.CostUSD > 0 {
					r.Counter("cost_usd_total", "runtime", runtime, "model", model).AddFloat(u.CostUSD)
				}
			})
		}
		return o.tx.PutTicket(t)
	})
}

// SetMetrics makes the Engine record engine_command_seconds, dispatch_seconds, tickets_ended_total, tokens_total and
// cost_usd_total in r. It must be called before the first command; a nil r (the default) records nothing.
func (e *Engine) SetMetrics(r *metrics.Registry) {
	e.reg = r
	e.cmdHist = nil
	if r == nil {
		return
	}
	e.cmdHist = make(map[string]*metrics.Histogram, len(commandNames))
	for _, name := range commandNames {
		e.cmdHist[name] = r.Histogram("engine_command_seconds", "command", name)
	}
}
