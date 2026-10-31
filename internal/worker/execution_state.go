package worker

import "time"

// One ticket spans actual handler exit AND terminal publication. Its address
// distinguishes same-ID/retried work. The state's mutex guards every field.
type executionTicket struct {
	state                                *WorkerState
	jobType                              string
	started                              time.Time
	stuckAfter                           time.Duration
	handlerDone, pipelineDone, uncertain bool
}
type executionTicketKey struct{}

func (s *WorkerState) beginExecution(jobType string, started time.Time, stuckAfter time.Duration) *executionTicket {
	if s == nil {
		return nil
	}
	t := &executionTicket{state: s, jobType: jobType, started: started, stuckAfter: stuckAfter}
	s.executionMu.Lock()
	defer s.executionMu.Unlock()
	if s.executions == nil {
		s.executions = make(map[*executionTicket]struct{})
	}
	s.executions[t] = struct{}{}
	return t
}
func (t *executionTicket) handlerFinished(uncertain bool) {
	if t == nil {
		return
	}
	s := t.state
	s.executionMu.Lock()
	defer s.executionMu.Unlock()
	t.handlerDone = true
	t.uncertain = t.uncertain || uncertain
	if t.pipelineDone && !t.uncertain {
		delete(s.executions, t)
	}
}
func (t *executionTicket) pipelineFinished() {
	if t == nil {
		return
	}
	s := t.state
	s.executionMu.Lock()
	defer s.executionMu.Unlock()
	t.pipelineDone = true
	if t.handlerDone && !t.uncertain {
		delete(s.executions, t)
	}
}
func (s *WorkerState) trackedExecutionState(now time.Time, ordinaryLimit time.Duration) (tracked, stuck bool) {
	if s == nil {
		return false, false
	}
	s.executionMu.Lock()
	defer s.executionMu.Unlock()
	for t := range s.executions {
		tracked = true
		limit := ordinaryLimit
		if t.stuckAfter > 0 {
			limit = t.stuckAfter
		}
		if limit > 0 && (t.stuckAfter > 0 && !now.Before(t.started.Add(limit)) || t.stuckAfter == 0 && now.Sub(t.started) > limit) {
			return true, true
		}
	}
	return tracked, false
}
