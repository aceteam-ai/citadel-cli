package workercontrol

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Exchange is authenticated transport only. A nil transport error is never
// sufficient: Session validates the exact decision and fresh observation.
type Exchange func(context.Context, Request) (Response, error)

var processID = uuid.NewString()
var sessionRegistry struct {
	sync.Mutex
	sessions map[string]*Session
}

// Session is a single process's non-restorable control pipeline. Cancellation
// is irreversible even when a late reply or read_status reports authorization.
type Session struct {
	mu                sync.Mutex
	envelope          Envelope
	process           string
	exchange          Exchange
	now               func() time.Time
	deadline          time.Time
	version           int64
	claim             *Receipt
	completion        *Receipt
	outcome, code     string
	cancelled, permit bool
	gateClosed        bool
	lifetime          context.Context
}

func boundedMillis(ms int64) time.Duration {
	// A very distant wire expiry must not overflow Duration or Time's
	// monotonic arithmetic. This conservative local cap can only shorten the
	// authority deadline; it is never renewed by retry or wall-clock movement.
	const max = int64((24 * time.Hour) / time.Millisecond)
	if ms < 0 {
		return 0
	}
	if ms > max {
		ms = max
	}
	return time.Duration(ms) * time.Millisecond
}
func NewSession(e Envelope, exchange Exchange) *Session {
	sessionRegistry.Lock()
	defer sessionRegistry.Unlock()
	if sessionRegistry.sessions == nil {
		sessionRegistry.sessions = map[string]*Session{}
	}
	key := e.Identity.OrgID + ":" + e.Identity.AttemptID
	if s := sessionRegistry.sessions[key]; s != nil {
		return s
	}
	s := newSession(e, exchange, time.Now, processID)
	if len(sessionRegistry.sessions) >= 1024 {
		s.cancelled = true
		return s
	}
	sessionRegistry.sessions[key] = s
	return s
}
func newSession(e Envelope, exchange Exchange, now func() time.Time, process string) *Session {
	n := now()
	s := &Session{envelope: e, exchange: exchange, now: now, process: process, lifetime: context.Background()}
	// A skewed delivery wall clock may shorten this initial bound; subsequent
	// fresh server observations may only shorten it further.
	s.deadline = n.Add(boundedMillis(e.Identity.ExpiresAt - n.UnixMilli()))
	if e.Identity.Validate() != nil || e.Delivery.Validate(e.Identity) != nil || exchange == nil || !RandomUUID(process) {
		s.cancelled = true
	}
	return s
}
func (s *Session) live() bool {
	return !s.cancelled && s.lifetime.Err() == nil && s.now().Before(s.deadline)
}
func (s *Session) Matches(e Envelope) bool { return s != nil && s.envelope == e }
func (s *Session) Cancel() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cancelled = true
	s.permit = false
	s.mu.Unlock()
}
func (s *Session) CanPrepare() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live() && !s.gateClosed && s.claim != nil
}
func (s *Session) CanCommit() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live() && !s.gateClosed && s.claim != nil && s.permit
}
func (s *Session) ReservationKey() string { return s.envelope.Identity.AttemptID + ":" + s.process }
func (s *Session) AttemptID() string      { return s.envelope.Identity.AttemptID }
func (s *Session) ProcessID() string      { return s.process }
func (s *Session) operate(ctx context.Context, operation, outcome, code string) (Response, error) {
	if !s.live() || ctx.Err() != nil {
		s.cancelled = true
		return Response{}, ErrDenied
	}
	r := Request{RequestID: uuid.NewString(), Operation: operation, Identity: s.envelope.Identity, Delivery: s.envelope.Delivery, ProcessID: s.process, Outcome: outcome, Code: code}
	if s.claim != nil {
		r.ClaimEpoch = s.claim.ClaimEpoch
	}
	if operation == "complete" {
		r.OutcomeDigest = OutcomeDigest(outcome, code)
	}
	r.Seal()
	if r.Validate() != nil {
		s.cancelled = true
		return Response{}, ErrInvalid
	}
	started := s.now()
	budgetCtx, cancel := context.WithDeadline(ctx, s.deadline)
	defer cancel()
	// Also bound a peer that never returns the first fresh observation.
	requestCtx, requestCancel := context.WithTimeout(budgetCtx, 10*time.Second)
	defer requestCancel()
	response, err := s.exchange(requestCtx, r)
	if err == nil {
		err = ValidateResponse(r, response)
	}
	if err != nil {
		s.cancelled = true
		s.permit = false
		return response, err
	}
	o := response.Observation
	if o == nil || o.StateVersion < s.version {
		s.cancelled = true
		s.permit = false
		return response, ErrInvalid
	}
	candidate := started.Add(boundedMillis(r.Identity.ExpiresAt - o.ServerTimeMS))
	if candidate.Before(s.deadline) {
		s.deadline = candidate
	}
	if !s.live() || requestCtx.Err() != nil {
		s.cancelled = true
		s.permit = false
		return response, ErrDenied
	}
	s.version = o.StateVersion
	if operation == "read_status" && s.permit && o.State != "commit_authorized" {
		s.cancelled = true
		s.permit = false
	}
	return response, nil
}
func (s *Session) Claim(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outcome != "" {
		return ErrDenied
	}
	if s.claim != nil {
		if s.live() {
			return nil
		}
		return ErrDenied
	}
	s.lifetime = ctx
	response, err := s.operate(ctx, "claim_and_ack", "", "")
	if err != nil {
		return err
	}
	copy := *response.Receipt
	s.claim = &copy
	return nil
}
func (s *Session) Complete(ctx context.Context, outcome, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claim == nil || !s.live() || (outcome == "prepared" && s.gateClosed) || (s.outcome != "" && (s.outcome != outcome || s.code != code)) {
		s.cancelled = true
		s.permit = false
		return ErrDenied
	}
	s.outcome, s.code = outcome, code
	if outcome == "refused_no_effect" {
		s.gateClosed = true
	}
	response, err := s.operate(ctx, "complete", outcome, code)
	if err != nil {
		return err
	}
	if response.Receipt.StateVersion <= s.claim.StateVersion {
		s.cancelled = true
		s.permit = false
		return ErrInvalid
	}
	if s.completion != nil && *s.completion != *response.Receipt {
		s.cancelled = true
		s.permit = false
		return ErrInvalid
	}
	copy := *response.Receipt
	s.completion = &copy
	s.permit = outcome == "prepared" && response.Observation.PermitUsable
	return nil
}

// ReadStatus never restores execution permission or reopens a cancelled gate.
func (s *Session) ReadStatus(ctx context.Context) (Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := Request{RequestID: uuid.NewString(), Operation: "read_status", Identity: s.envelope.Identity, Delivery: s.envelope.Delivery, ProcessID: s.process}
	if s.claim != nil {
		r.ClaimEpoch = s.claim.ClaimEpoch
	}
	r.Seal()
	if r.Validate() != nil || s.exchange == nil {
		return Response{}, ErrInvalid
	}
	started := s.now()
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	response, err := s.exchange(bounded, r)
	if err == nil {
		err = ValidateResponse(r, response)
	}
	if err != nil {
		s.cancelled = true
		s.permit = false
		return response, err
	}
	o := response.Observation
	if o == nil || o.StateVersion < s.version {
		s.cancelled = true
		s.permit = false
		return response, ErrInvalid
	}
	candidate := started.Add(boundedMillis(r.Identity.ExpiresAt - o.ServerTimeMS))
	if candidate.Before(s.deadline) {
		s.deadline = candidate
	}
	s.version = o.StateVersion
	if o.State != "commit_authorized" {
		s.permit = false
	}
	// Historical reconciliation never clears cancelled/gateClosed or grants a
	// permit. A caller may learn the server committed even after local expiry.
	return response, nil
}
