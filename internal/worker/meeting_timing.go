package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/meetingpolicy"
)

type meetingTimingProvider interface {
	MeetingProfile() (meetingpolicy.Profile, error)
}
type meetingBudgetKey struct{}

func (a *LegacyHandlerAdapter) MeetingProfile() (meetingpolicy.Profile, error) {
	if p, ok := a.handler.(meetingTimingProvider); ok {
		return p.MeetingProfile()
	}
	return meetingpolicy.Default(), nil
}

// errMeetingRedelivered refuses a MEETING_JOIN delivery that is not its first
// (#1505: no automatic rejoin).
var errMeetingRedelivered = errors.New("meeting join was redelivered; automatic rejoin is disabled")

func meetingAuthority(b meetingpolicy.Budget, raw any, present bool) error {
	if !selfHealEnabled() {
		return fmt.Errorf("meeting timing requires the configured self-heal backstop")
	}
	return b.CheckAuthority(raw, present, strings.TrimSpace(os.Getenv(jobTimeoutLongEnvVar)), strings.TrimSpace(os.Getenv(selfHealStuckEnvVar)))
}

func (a *LegacyHandlerAdapter) meetingReadiness() error {
	if a.jobType != JobTypeMeetingJoin {
		return nil
	}
	p, err := a.MeetingProfile()
	if err != nil {
		return err
	}
	b, err := p.Maximum()
	if err != nil {
		return err
	}
	return meetingAuthority(b, nil, false)
}

func (a *LegacyHandlerAdapter) RefusalFor(jobType string) *jobs.ShellRefusal {
	if jobType != a.jobType || jobType != JobTypeMeetingJoin || a.enabled != nil && !a.enabled() {
		return nil
	}
	if a.meetingReadiness() != nil {
		return &jobs.ShellRefusal{Reason: "meeting_timing_incompatible", Message: "meeting timing policy is invalid or exceeds the node's explicit watchdog/STUCK limits; configure compatible limits before joining"}
	}
	return nil
}

func meetingSeconds(payload map[string]any, key string) (string, bool, error) {
	v, exists := payload[key]
	if !exists {
		return "", false, nil
	}
	if s, ok := v.(string); ok {
		return s, true, nil
	}
	switch v.(type) {
	case int, int64, float64, json.Number:
		return fmt.Sprint(v), true, nil
	default:
		return "", true, fmt.Errorf("invalid meeting duration payload")
	}
}

// resolveExecutionBudget is the production deadline entrypoint. Only meetings
// use the ratified finite profile; other jobs retain their existing tiers.
func (r *Runner) resolveExecutionBudget(ctx context.Context, handler JobHandler, job *Job) (context.Context, time.Duration, bool, error) {
	if job.Type != JobTypeMeetingJoin {
		d, ok := r.resolveJobTimeout(job)
		return ctx, d, ok, nil
	}
	p := meetingpolicy.Default()
	if provider, ok := handler.(meetingTimingProvider); ok {
		var err error
		p, err = provider.MeetingProfile()
		if err != nil {
			return ctx, 0, true, err
		}
	}
	a, ap, err := meetingSeconds(job.Payload, "admit_timeout_seconds")
	if err != nil {
		return ctx, 0, true, err
	}
	record, rp, err := meetingSeconds(job.Payload, "max_duration_seconds")
	if err != nil {
		return ctx, 0, true, err
	}
	b, err := p.Budget(a, ap, record, rp)
	if err != nil {
		return ctx, 0, true, err
	}
	raw, present := job.Payload[jobTimeoutPayloadKey]
	if err = meetingAuthority(b, raw, present); err != nil {
		return ctx, 0, true, err
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < b.Execution() {
		return ctx, 0, true, fmt.Errorf("meeting profile exceeds the caller execution deadline")
	}
	ctx = context.WithValue(ctx, meetingBudgetKey{}, b)
	ctx = jobs.WithMeetingCleanup(ctx, jobs.NewMeetingCleanup(time.Duration(p.Cleanup)*time.Second))
	return ctx, b.Execution(), true, nil
}

func (r *Runner) executeObserved(ctx context.Context, handler JobHandler, job *Job, stream StreamWriter) (*JobResult, error) {
	ticket, _ := ctx.Value(executionTicketKey{}).(*executionTicket)
	if ticket == nil {
		ticket = r.state.beginExecution(job.Type, time.Now(), 0)
		defer ticket.pipelineFinished()
	}
	if job.Type == JobTypeMeetingJoin && ctx.Err() != nil {
		ticket.handlerFinished(false)
		return nil, ctx.Err()
	}
	result, err := handler.Execute(ctx, job, stream)
	ticket.handlerFinished(errors.Is(err, jobs.ErrMeetingTeardownUnconfirmed))
	return result, err
}
