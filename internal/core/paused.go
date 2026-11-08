package core

// AssignmentPaused is the Executor's fact that the owner's pause parked the session of the Assignment that runs the open
// Ticket at the end of its turn: it records an Interruption with reason InterruptPaused, and reportOwed when that turn
// ended without the Report the worker owed. Like AssignmentInterrupted, it changes nothing while an Interruption is
// open or about any other Assignment.
func (e *Engine) AssignmentPaused(ticketID, assignmentID string, reportOwed bool) error {
	return e.command(cmdAssignmentPaused, func(o *op) error {
		t, err := o.running(ticketID, assignmentID)
		if t == nil || err != nil {
			return err
		}
		a := runningAssignment(t)
		if a.OpenInterruption() != nil {
			return nil
		}
		a.Interruptions = append(a.Interruptions, Interruption{At: o.at, Reason: InterruptPaused, ReportOwed: reportOwed})
		t.UpdatedAt = o.at
		return o.tx.PutTicket(t)
	})
}

// PauseLifted closes, on resume, the open pause of an Assignment whose session is not started again because nothing
// waits for it: the Interruption is over (ResumedAt, as a start closes it) and the Assignment stays idle with its
// session, which its next Input resumes. Anything else changes nothing.
func (e *Engine) PauseLifted(ticketID, assignmentID string) error {
	return e.command(cmdPauseLifted, func(o *op) error {
		t, err := o.running(ticketID, assignmentID)
		if t == nil || err != nil {
			return err
		}
		a := runningAssignment(t)
		open := a.OpenInterruption()
		if open == nil || open.Reason != InterruptPaused {
			return nil
		}
		open.ResumedAt = o.timestamp()
		t.UpdatedAt = o.at
		return o.tx.PutTicket(t)
	})
}
