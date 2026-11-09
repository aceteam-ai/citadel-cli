// Package localcobrowse is Citadel local mode's co-browse owner (aceteam#13503
// design sections 2.1 to 2.5 and 4): one process per OS user that holds the
// browser sessions, the per-session write consent, the abuse limits and the
// navigation address filter, and serves the local_cobrowse_* MCP tools.
//
// Tool classes follow the platform's split. W (write: navigate, click, type,
// select, key, scroll) needs a live, human-given grant for the session. R (read:
// status, extract, screenshot) and L (lifecycle: start, handoff, resume, stop,
// wait) do not. While a human has taken control, every agent browser action,
// including screenshot and extract, is refused by the session manager.
//
// Consent can only be given by the human in the local viewer; nothing reachable
// through the MCP tool path can grant it. The viewer side lives in a later
// slice and calls Approve, Deny, Revoke, HumanTakeControl and HumanHandBack.
package localcobrowse

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// Default local-mode limits (design section 4, "Abuse limits").
const (
	DefaultMaxSessions        = 2
	DefaultWritesPerMinute    = 120
	DefaultNavigationsPerHour = 600
	DefaultReadsPerMinute     = 120
	DefaultIdleLifetime       = 8 * time.Hour
	DefaultGrantDuration      = 24 * time.Hour
	// MaxWait caps one local_cobrowse_wait call so it stays inside common
	// harness tool timeouts.
	MaxWait = 50 * time.Second
	// DefaultExtractChars and MaxExtractChars bound extract output.
	DefaultExtractChars = 4000
	MaxExtractChars     = 20000
)

// LifetimePolicy is reported in status so an agent knows when a session ends.
const LifetimePolicy = "until local_cobrowse_stop, the Citadel MCP server exiting, or 8 hours without activity"

// Browser is the slice of platform.CobrowseSessionManager the service drives.
type Browser interface {
	StartSession(startURL string) (platform.CobrowseSessionStatus, error)
	SessionStatus(id string) (platform.CobrowseSessionStatus, bool)
	Stop(id string) error
	StopAll() error
	Navigate(id, url string) (platform.CobrowseSessionStatus, error)
	NavigateAsHuman(id, url string) (platform.CobrowseSessionStatus, error)
	Click(id, selector string, x, y *float64) (platform.CobrowseSessionStatus, error)
	Type(id, text string) (platform.CobrowseSessionStatus, error)
	Select(id, selector, value, label string) (platform.SelectResult, error)
	Key(id, key string) (platform.CobrowseSessionStatus, error)
	Scroll(id, selector string, x, y *float64, dx, dy float64) (platform.CobrowseSessionStatus, error)
	ExtractBounded(id, selector string, attrs []string, maxChars int) (platform.ExtractResult, error)
	Screenshot(id string) (string, error)
	PageState(id string) (platform.PageState, error)
	Handoff(id string) (platform.CobrowseSessionStatus, error)
	Resume(id string) (platform.CobrowseSessionStatus, error)
}

// Typed refusals. Each carries a plain next step for the agent.
var (
	ErrApprovalRequired = errors.New("approval_required: ask the user to approve AI actions for this session in the Citadel viewer, then call local_cobrowse_wait with until=approved")
	ErrApprovalDenied   = errors.New("approval_denied: the user denied AI actions in this session; do not retry writes unless the user asks")
	ErrApprovalExpired  = errors.New("approval_expired: the user's approval for this session expired; ask them to approve again in the Citadel viewer, then call local_cobrowse_wait with until=approved")
	ErrApprovalRevoked  = errors.New("approval_revoked: the user revoked AI actions for this session; ask them before trying again")
	ErrHumanDriving     = errors.New("human_driving: the user has control of this browser; call local_cobrowse_wait with until=resumed, or ask them to press Hand back")
	ErrRateLimited      = errors.New("rate_limited")
	ErrTooManySessions  = errors.New("too_many_sessions")
	ErrNoSession        = errors.New("no such co-browse session")
	ErrProfileNotReady  = errors.New("persistent profiles are not available in this version of local mode; omit profile to use a throwaway session")
)

// Event is one tool call outcome, for the audit log.
type Event struct {
	SessionID string
	Action    string // tool action, e.g. "navigate"
	Class     string // "W", "R" or "L"
	Goal      string
	URL       string // target URL for start/navigate
	Selector  string
	TextLen   int // type: length only, never the text
	Outcome   string
	Error     string
}

// Config configures a Service. Zero values take the local-mode defaults.
type Config struct {
	Browser            Browser
	MaxSessions        int
	WritesPerMinute    int
	NavigationsPerHour int
	ReadsPerMinute     int
	IdleLifetime       time.Duration
	GrantDuration      time.Duration
	// URLPolicy decides which URLs may be opened. Nil uses NewURLPolicy with the
	// system resolver and ViewerPorts.
	URLPolicy URLPolicy
	// ViewerPorts reports ports the browser must never be pointed at.
	ViewerPorts func() []int
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
	// AcquireOwner is called before the first session starts. A non-nil error
	// (another process already owns co-browse for this user) refuses every
	// local_cobrowse_* call. Nil means no single-instance check.
	AcquireOwner func() error
	// OnAttention is told when the human is needed (reason "approval" or
	// "handoff") or a session starts ("start"), so the viewer can be shown.
	OnAttention func(sessionID, reason string)
	// OnEvent receives every call outcome, including refusals.
	OnEvent func(Event)
}

// grantState is one session's consent record.
type grantState struct {
	expires time.Time
	denied  bool
	revoked bool
}

type session struct {
	id            string
	goal          string
	startedAt     time.Time
	lastActive    time.Time
	grant         grantState
	handoffAt     time.Time
	handoffReason string
	stoppedReason string
}

// Service is the local co-browse owner.
type Service struct {
	cfg Config

	ownerMu sync.Mutex
	owned   bool

	mu       sync.Mutex
	sessions map[string]*session
	// Budgets are owner-wide, so stopping and restarting sessions never
	// resets them (design section 4 abuse limits).
	writes []time.Time
	navs   []time.Time
	reads  []time.Time
	// changed is closed and replaced on every consent, driver or lifecycle
	// change, waking local_cobrowse_wait callers.
	changed chan struct{}
	agent   string
}

// New returns a Service.
func New(cfg Config) *Service {
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = DefaultMaxSessions
	}
	if cfg.WritesPerMinute <= 0 {
		cfg.WritesPerMinute = DefaultWritesPerMinute
	}
	if cfg.NavigationsPerHour <= 0 {
		cfg.NavigationsPerHour = DefaultNavigationsPerHour
	}
	if cfg.ReadsPerMinute <= 0 {
		cfg.ReadsPerMinute = DefaultReadsPerMinute
	}
	if cfg.IdleLifetime <= 0 {
		cfg.IdleLifetime = DefaultIdleLifetime
	}
	if cfg.GrantDuration <= 0 {
		cfg.GrantDuration = DefaultGrantDuration
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.URLPolicy == nil {
		cfg.URLPolicy = NewURLPolicy(DefaultResolver, cfg.ViewerPorts)
	}
	return &Service{cfg: cfg, sessions: map[string]*session{}, changed: make(chan struct{})}
}

// SetAgent records the connected MCP client's name (from initialize) for the
// audit log and the viewer's "connected agent" line.
func (s *Service) SetAgent(name string) {
	s.mu.Lock()
	s.agent = strings.TrimSpace(name)
	s.mu.Unlock()
}

// Agent returns the connected MCP client's name, or "".
func (s *Service) Agent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agent
}

func (s *Service) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Service) emit(ev Event) {
	if s.cfg.OnEvent != nil {
		s.cfg.OnEvent(ev)
	}
}

func (s *Service) attention(id, reason string) {
	if s.cfg.OnAttention != nil {
		s.cfg.OnAttention(id, reason)
	}
}

// ensureOwner acquires the single-instance owner lock once.
func (s *Service) ensureOwner() error {
	// ownerMu, not mu: AcquireOwner may call back into the service (Agent).
	s.ownerMu.Lock()
	defer s.ownerMu.Unlock()
	if s.owned {
		return nil
	}
	if s.cfg.AcquireOwner != nil {
		if err := s.cfg.AcquireOwner(); err != nil {
			return err
		}
	}
	s.owned = true
	return nil
}

// reapIdleLocked stops sessions idle past the lifetime. Caller holds s.mu; the
// browser stop runs after unlocking via the returned ids.
func (s *Service) reapIdleLocked(now time.Time) []string {
	var ids []string
	for id, ss := range s.sessions {
		if now.Sub(ss.lastActive) >= s.cfg.IdleLifetime {
			ids = append(ids, id)
			delete(s.sessions, id)
		}
	}
	if len(ids) > 0 {
		s.notifyLocked()
	}
	return ids
}

// ReapIdle stops sessions idle past the lifetime. The owner calls it on a timer.
func (s *Service) ReapIdle() {
	s.mu.Lock()
	ids := s.reapIdleLocked(s.cfg.Now())
	s.mu.Unlock()
	for _, id := range ids {
		_ = s.cfg.Browser.Stop(id)
		s.emit(Event{SessionID: id, Action: "stop", Class: "L", Outcome: "stopped_idle"})
	}
}

// StartResult is local_cobrowse_start's result.
type StartResult struct {
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	Driver    string `json:"driver"`
	URL       string `json:"url,omitempty"`
	Browser   string `json:"browser,omitempty"`
	Lifetime  string `json:"lifetime"`
	Writes    string `json:"writes"`
	Viewer    string `json:"viewer,omitempty"`
}

// Start opens a new throwaway session at startURL (empty means about:blank).
// Navigating to the start URL needs no grant (platform decision 3); it still
// passes the address filter and counts toward the navigation budget.
func (s *Service) Start(ctx context.Context, startURL, goal, profile string) (StartResult, error) {
	ev := Event{Action: "start", Class: "L", Goal: goal, URL: startURL}
	res, err := s.start(ctx, startURL, goal, profile)
	ev.SessionID = res.SessionID
	s.finish(ev, err)
	return res, err
}

func (s *Service) start(ctx context.Context, startURL, goal, profile string) (StartResult, error) {
	if err := s.ensureOwner(); err != nil {
		return StartResult{}, err
	}
	if strings.TrimSpace(profile) != "" {
		return StartResult{}, ErrProfileNotReady
	}
	if strings.TrimSpace(startURL) == "" {
		startURL = "about:blank"
	}
	if err := s.cfg.URLPolicy(ctx, startURL); err != nil {
		return StartResult{}, err
	}
	s.mu.Lock()
	reaped := s.reapIdleLocked(s.cfg.Now())
	if len(s.sessions) >= s.cfg.MaxSessions {
		n := len(s.sessions)
		s.mu.Unlock()
		s.stopReaped(reaped)
		return StartResult{}, fmt.Errorf("%w: local mode allows %d co-browse sessions at once and %d are open; stop one with local_cobrowse_stop", ErrTooManySessions, s.cfg.MaxSessions, n)
	}
	s.mu.Unlock()
	s.stopReaped(reaped)

	st, err := s.cfg.Browser.StartSession(startURL)
	if err != nil {
		return StartResult{}, err
	}
	now := s.cfg.Now()
	s.mu.Lock()
	ss := &session{id: st.ID, goal: goal, startedAt: now, lastActive: now}
	if startURL != "about:blank" {
		s.navs = append(s.navs, now)
	}
	s.sessions[st.ID] = ss
	s.notifyLocked()
	s.mu.Unlock()
	// A start URL can redirect. Check where the browser landed before any
	// read can see it.
	if startURL != "about:blank" {
		res := s.settle(ctx, st.ID, "")
		landed := res.URL
		if landed == "" {
			landed = s.currentURL(st.ID)
		}
		if landed != "" && !strings.EqualFold(landed, "about:blank") {
			if err := s.cfg.URLPolicy(ctx, landed); err != nil {
				_ = s.stop(st.ID, "start url refused")
				return StartResult{}, fmt.Errorf("the start page redirected to a refused address, so the session was closed: %w", err)
			}
		}
		if cur, ok := s.cfg.Browser.SessionStatus(st.ID); ok {
			st.URL = cur.URL
		}
	}
	s.attention(st.ID, "start")
	return StartResult{
		SessionID: st.ID,
		State:     string(st.State),
		Driver:    string(st.Driver),
		URL:       st.URL,
		Browser:   st.Browser,
		Lifetime:  LifetimePolicy,
		Writes:    "navigate, click, type, select, key and scroll need the user's approval in the Citadel viewer",
	}, nil
}

func (s *Service) stopReaped(ids []string) {
	for _, id := range ids {
		_ = s.cfg.Browser.Stop(id)
		s.emit(Event{SessionID: id, Action: "stop", Class: "L", Outcome: "stopped_idle"})
	}
}

func (s *Service) finish(ev Event, err error) {
	if err != nil {
		ev.Outcome = "refused"
		if !isRefusal(err) {
			ev.Outcome = "error"
		}
		ev.Error = err.Error()
	} else if ev.Outcome == "" {
		ev.Outcome = "ok"
	}
	s.emit(ev)
}

func isRefusal(err error) bool {
	for _, e := range []error{ErrApprovalRequired, ErrApprovalDenied, ErrApprovalExpired, ErrApprovalRevoked,
		ErrHumanDriving, ErrOnlyUserHandsBack, ErrRateLimited, ErrTooManySessions, ErrNavigationRefused, ErrProfileNotReady,
		platform.ErrHandedOff, platform.ErrPasswordValue} {
		if errors.Is(err, e) {
			return true
		}
	}
	var oe *OwnerError
	return errors.As(err, &oe)
}

// lookup returns a tracked session, reaping idle ones first and touching it.
func (s *Service) lookup(id string) (*session, error) {
	if err := s.ensureOwner(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	reaped := s.reapIdleLocked(s.cfg.Now())
	ss, ok := s.sessions[id]
	if ok {
		ss.lastActive = s.cfg.Now()
	}
	s.mu.Unlock()
	s.stopReaped(reaped)
	if !ok {
		return nil, fmt.Errorf("%w %q (it was stopped, timed out, or belongs to a Citadel process that has exited; start a new one)", ErrNoSession, id)
	}
	return ss, nil
}

// humanDriving reports whether the user has taken control of the browser.
func (s *Service) humanDriving(id string) bool {
	st, ok := s.cfg.Browser.SessionStatus(id)
	return ok && st.Driver == platform.DriverHuman
}

// gateWrite enforces, in order: the user is not driving, a live grant exists,
// and the per-session write (and, for navigations, navigation) budget.
func (s *Service) gateWrite(id string, navigation bool) error {
	ss, err := s.lookup(id)
	if err != nil {
		return err
	}
	if s.humanDriving(id) {
		return ErrHumanDriving
	}
	now := s.cfg.Now()
	s.mu.Lock()
	g := ss.grant
	var gateErr error
	switch {
	case g.denied:
		gateErr = ErrApprovalDenied
	case g.revoked:
		gateErr = ErrApprovalRevoked
	case g.expires.IsZero():
		gateErr = ErrApprovalRequired
	case !now.Before(g.expires):
		gateErr = ErrApprovalExpired
	}
	if gateErr == nil {
		s.writes = pruneBefore(s.writes, now.Add(-time.Minute))
		if len(s.writes) >= s.cfg.WritesPerMinute {
			gateErr = fmt.Errorf("%w: at most %d actions per minute; slow down", ErrRateLimited, s.cfg.WritesPerMinute)
		}
	}
	if gateErr == nil && navigation {
		s.navs = pruneBefore(s.navs, now.Add(-time.Hour))
		if len(s.navs) >= s.cfg.NavigationsPerHour {
			gateErr = fmt.Errorf("%w: at most %d page loads per hour", ErrRateLimited, s.cfg.NavigationsPerHour)
		}
	}
	if gateErr == nil {
		s.writes = append(s.writes, now)
		if navigation {
			s.navs = append(s.navs, now)
		}
	}
	s.mu.Unlock()
	if errors.Is(gateErr, ErrApprovalRequired) || errors.Is(gateErr, ErrApprovalExpired) {
		s.attention(id, "approval")
	}
	return gateErr
}

func pruneBefore(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	return ts[i:]
}

// gateRead checks the session exists, the user is not driving, the owner-wide
// read budget, and that the page the agent is about to read is still on an
// allowed address (a script or delayed redirect may have moved it after the
// last check).
func (s *Service) gateRead(ctx context.Context, id string) error {
	if _, err := s.lookup(id); err != nil {
		return err
	}
	if s.humanDriving(id) {
		return ErrHumanDriving
	}
	now := s.cfg.Now()
	s.mu.Lock()
	s.reads = pruneBefore(s.reads, now.Add(-time.Minute))
	if len(s.reads) >= s.cfg.ReadsPerMinute {
		s.mu.Unlock()
		return fmt.Errorf("%w: at most %d reads per minute; slow down", ErrRateLimited, s.cfg.ReadsPerMinute)
	}
	s.reads = append(s.reads, now)
	s.mu.Unlock()
	if u := s.currentURL(id); u != "" && !strings.EqualFold(u, "about:blank") {
		if err := s.cfg.URLPolicy(ctx, u); err != nil {
			_, _ = s.cfg.Browser.Navigate(id, "about:blank")
			return fmt.Errorf("the page moved to a refused address and was closed: %w", err)
		}
	}
	return nil
}

// ActionResult is the result of a write action (#13495 shape): dispatched is
// true once the input reached the browser; settled and navigated describe what
// was observed afterwards. A slow page after dispatch is never a failure.
type ActionResult struct {
	Dispatched bool   `json:"dispatched"`
	Settled    bool   `json:"settled"`
	Navigated  bool   `json:"navigated"`
	URL        string `json:"url,omitempty"`
	Value      string `json:"value,omitempty"`
	Label      string `json:"label,omitempty"`
}

// settleWindow is how long a write waits to observe navigation and load;
// settleMinimum is how long it waits before trusting "complete" on the old
// document, so a click-triggered navigation has time to begin.
var (
	settleWindow  = 3 * time.Second
	settleMinimum = 400 * time.Millisecond
	settlePoll    = 150 * time.Millisecond
)

func (s *Service) settle(ctx context.Context, id, before string) ActionResult {
	res := ActionResult{Dispatched: true}
	start := time.Now()
	for {
		if ps, err := s.cfg.Browser.PageState(id); err == nil {
			res.URL = ps.URL
			if before != "" && ps.URL != before {
				res.Navigated = true
			}
			if ps.ReadyState == "complete" && (res.Navigated || time.Since(start) >= settleMinimum) {
				res.Settled = true
				return res
			}
		}
		if time.Since(start) >= settleWindow {
			return res
		}
		select {
		case <-ctx.Done():
			return res
		case <-time.After(settlePoll):
		}
	}
}

func (s *Service) currentURL(id string) string {
	ps, err := s.cfg.Browser.PageState(id)
	if err != nil {
		return ""
	}
	return ps.URL
}

// enforceLanding refuses after the fact when a navigation (for example an
// HTTP redirect, which the pre-check cannot see) landed on a refused address,
// and returns the browser to a blank page.
func (s *Service) enforceLanding(ctx context.Context, id string, res ActionResult) error {
	if res.URL == "" || !res.Navigated {
		return nil
	}
	if err := s.cfg.URLPolicy(ctx, res.URL); err != nil {
		_, _ = s.cfg.Browser.Navigate(id, "about:blank")
		return fmt.Errorf("the page redirected to a refused address and was closed: %w", err)
	}
	return nil
}

// Navigate opens url in the session (W).
func (s *Service) Navigate(ctx context.Context, id, url, goal string) (ActionResult, error) {
	ev := Event{SessionID: id, Action: "navigate", Class: "W", Goal: goal, URL: url}
	res, err := func() (ActionResult, error) {
		if err := s.cfg.URLPolicy(ctx, url); err != nil {
			if _, lerr := s.lookup(id); lerr != nil {
				return ActionResult{}, lerr
			}
			return ActionResult{}, err
		}
		if err := s.gateWrite(id, true); err != nil {
			return ActionResult{}, err
		}
		before := s.currentURL(id)
		if _, err := s.cfg.Browser.Navigate(id, url); err != nil {
			return ActionResult{}, err
		}
		res := s.settle(ctx, id, before)
		if url != before {
			res.Navigated = true
		}
		return res, s.enforceLanding(ctx, id, res)
	}()
	s.finish(ev, err)
	return res, err
}

// Click clicks an element or a viewport point (W).
func (s *Service) Click(ctx context.Context, id, selector string, x, y *float64, goal string) (ActionResult, error) {
	ev := Event{SessionID: id, Action: "click", Class: "W", Goal: goal, Selector: selector}
	res, err := s.writeAction(ctx, id, func() error {
		_, err := s.cfg.Browser.Click(id, selector, x, y)
		return err
	})
	s.finish(ev, err)
	return res, err
}

// Type inserts text into the focused element (W). Only its length is audited.
func (s *Service) Type(ctx context.Context, id, text, goal string) (ActionResult, error) {
	ev := Event{SessionID: id, Action: "type", Class: "W", Goal: goal, TextLen: len([]rune(text))}
	res, err := s.writeAction(ctx, id, func() error {
		_, err := s.cfg.Browser.Type(id, text)
		return err
	})
	s.finish(ev, err)
	return res, err
}

// Select chooses an option in a native select (W).
func (s *Service) Select(ctx context.Context, id, selector, value, label, goal string) (ActionResult, error) {
	ev := Event{SessionID: id, Action: "select", Class: "W", Goal: goal, Selector: selector}
	var picked platform.SelectResult
	res, err := s.writeAction(ctx, id, func() error {
		var err error
		picked, err = s.cfg.Browser.Select(id, selector, value, label)
		return err
	})
	res.Value, res.Label = picked.Value, picked.Label
	s.finish(ev, err)
	return res, err
}

// Key presses one key (W).
func (s *Service) Key(ctx context.Context, id, key, goal string) (ActionResult, error) {
	ev := Event{SessionID: id, Action: "key", Class: "W", Goal: goal}
	res, err := s.writeAction(ctx, id, func() error {
		_, err := s.cfg.Browser.Key(id, key)
		return err
	})
	s.finish(ev, err)
	return res, err
}

// Scroll scrolls at an element or point (W).
func (s *Service) Scroll(ctx context.Context, id, selector string, x, y *float64, dx, dy float64, goal string) (ActionResult, error) {
	ev := Event{SessionID: id, Action: "scroll", Class: "W", Goal: goal, Selector: selector}
	res, err := s.writeAction(ctx, id, func() error {
		_, err := s.cfg.Browser.Scroll(id, selector, x, y, dx, dy)
		return err
	})
	s.finish(ev, err)
	return res, err
}

func (s *Service) writeAction(ctx context.Context, id string, do func() error) (ActionResult, error) {
	if err := s.gateWrite(id, false); err != nil {
		return ActionResult{}, err
	}
	before := s.currentURL(id)
	if err := do(); err != nil {
		return ActionResult{}, err
	}
	res := s.settle(ctx, id, before)
	if res.Navigated {
		s.mu.Lock()
		s.navs = append(pruneBefore(s.navs, s.cfg.Now().Add(-time.Hour)), s.cfg.Now())
		s.mu.Unlock()
	}
	return res, s.enforceLanding(ctx, id, res)
}

// Extract reads an element's text and attributes, bounded (R).
func (s *Service) Extract(ctx context.Context, id, selector string, attrs []string, maxChars int, goal string) (platform.ExtractResult, error) {
	ev := Event{SessionID: id, Action: "extract", Class: "R", Goal: goal, Selector: selector}
	if maxChars <= 0 {
		maxChars = DefaultExtractChars
	}
	if maxChars > MaxExtractChars {
		maxChars = MaxExtractChars
	}
	var res platform.ExtractResult
	err := s.gateRead(ctx, id)
	if err == nil {
		res, err = s.cfg.Browser.ExtractBounded(id, selector, attrs, maxChars)
	}
	s.finish(ev, err)
	return res, err
}

// Screenshot captures the viewport as base64 PNG (R).
func (s *Service) Screenshot(ctx context.Context, id, goal string) (string, error) {
	ev := Event{SessionID: id, Action: "screenshot", Class: "R", Goal: goal}
	var png string
	err := s.gateRead(ctx, id)
	if err == nil {
		png, err = s.cfg.Browser.Screenshot(id)
	}
	s.finish(ev, err)
	return png, err
}

// SessionView is one session's status.
type SessionView struct {
	SessionID      string `json:"session_id"`
	State          string `json:"state"`
	Driver         string `json:"driver"`
	URL            string `json:"url,omitempty"`
	Browser        string `json:"browser,omitempty"`
	Goal           string `json:"goal,omitempty"`
	StartedAt      string `json:"started_at"`
	Approval       string `json:"approval"`
	GrantExpiresAt string `json:"grant_expires_at,omitempty"`
	HandoffSince   string `json:"handoff_since,omitempty"`
	HandoffReason  string `json:"handoff_reason,omitempty"`
	Lifetime       string `json:"lifetime"`
}

// redactWhileHuman reduces a URL to its origin. While the user drives, the
// path and query can carry one-time codes and tokens the agent must not see.
func redactWhileHuman(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/"
}

func (s *Service) agentViewLocked(ss *session, st platform.CobrowseSessionStatus, now time.Time) SessionView {
	v := s.viewLocked(ss, st, now)
	if st.Driver == platform.DriverHuman {
		v.URL = redactWhileHuman(v.URL)
	}
	return v
}

func (s *Service) viewLocked(ss *session, st platform.CobrowseSessionStatus, now time.Time) SessionView {
	v := SessionView{
		SessionID: ss.id,
		State:     string(st.State),
		Driver:    string(st.Driver),
		URL:       st.URL,
		Browser:   st.Browser,
		Goal:      ss.goal,
		StartedAt: ss.startedAt.UTC().Format(time.RFC3339),
		Lifetime:  LifetimePolicy,
	}
	switch g := ss.grant; {
	case g.denied:
		v.Approval = "denied"
	case g.revoked:
		v.Approval = "revoked"
	case g.expires.IsZero():
		v.Approval = "not_requested"
	case !now.Before(g.expires):
		v.Approval = "expired"
	default:
		v.Approval = "approved"
		v.GrantExpiresAt = g.expires.UTC().Format(time.RFC3339)
	}
	if st.Driver == platform.DriverHuman && !ss.handoffAt.IsZero() {
		v.HandoffSince = ss.handoffAt.UTC().Format(time.RFC3339)
		v.HandoffReason = ss.handoffReason
	}
	return v
}

// Status reports one session, or every session when id is empty (R).
func (s *Service) Status(ctx context.Context, id string) ([]SessionView, error) {
	ev := Event{SessionID: id, Action: "status", Class: "R"}
	out, err := s.status(id)
	s.finish(ev, err)
	return out, err
}

func (s *Service) status(id string) ([]SessionView, error) {
	if id != "" {
		if _, err := s.lookup(id); err != nil {
			return nil, err
		}
	} else if err := s.ensureOwner(); err != nil {
		return nil, err
	}
	views := s.Sessions(id)
	for i := range views {
		if views[i].Driver == string(platform.DriverHuman) {
			views[i].URL = redactWhileHuman(views[i].URL)
		}
	}
	return views, nil
}

// Sessions returns session views without auditing; the viewer uses it.
func (s *Service) Sessions(id string) []SessionView {
	s.mu.Lock()
	ids := make([]string, 0, len(s.sessions))
	for sid := range s.sessions {
		if id == "" || sid == id {
			ids = append(ids, sid)
		}
	}
	s.mu.Unlock()
	sort.Strings(ids)
	out := make([]SessionView, 0, len(ids))
	now := s.cfg.Now()
	for _, sid := range ids {
		st, ok := s.cfg.Browser.SessionStatus(sid)
		if !ok {
			continue
		}
		s.mu.Lock()
		if ss, ok := s.sessions[sid]; ok {
			out = append(out, s.viewLocked(ss, st, now))
		}
		s.mu.Unlock()
	}
	return out
}

// Handoff hands the browser to the user (L). The agent is then refused every
// browser action until the user hands back.
func (s *Service) Handoff(ctx context.Context, id, reason, goal string) (SessionView, error) {
	ev := Event{SessionID: id, Action: "handoff", Class: "L", Goal: goal}
	v, err := s.setDriver(id, true, reason)
	s.finish(ev, err)
	if err == nil {
		s.attention(id, "handoff")
	}
	return v, err
}

// ErrOnlyUserHandsBack refuses an agent resume while the user has control: a
// prompt-injected agent must not take the browser back mid-login.
var ErrOnlyUserHandsBack = errors.New("only_user_hands_back: the user has control and is the only one who can hand it back (Hand back to agent in the Citadel viewer); call local_cobrowse_wait with until=resumed")

// Resume reports whether control is back with the agent (L). The agent can no
// longer take control back itself; it is refused while the user drives.
func (s *Service) Resume(ctx context.Context, id string) (SessionView, error) {
	ev := Event{SessionID: id, Action: "resume", Class: "L"}
	v, err := func() (SessionView, error) {
		if _, err := s.lookup(id); err != nil {
			return SessionView{}, err
		}
		if s.humanDriving(id) {
			return SessionView{}, ErrOnlyUserHandsBack
		}
		return s.setDriver(id, false, "")
	}()
	s.finish(ev, err)
	return v, err
}

func (s *Service) setDriver(id string, human bool, reason string) (SessionView, error) {
	ss, err := s.lookup(id)
	if err != nil {
		return SessionView{}, err
	}
	var st platform.CobrowseSessionStatus
	if human {
		st, err = s.cfg.Browser.Handoff(id)
	} else {
		st, err = s.cfg.Browser.Resume(id)
	}
	if err != nil {
		return SessionView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if human {
		if ss.handoffAt.IsZero() {
			ss.handoffAt = s.cfg.Now()
		}
		ss.handoffReason = strings.TrimSpace(reason)
	} else {
		ss.handoffAt = time.Time{}
		ss.handoffReason = ""
	}
	s.notifyLocked()
	return s.agentViewLocked(ss, st, s.cfg.Now()), nil
}

// Stop ends a session and deletes its throwaway profile (L).
func (s *Service) Stop(ctx context.Context, id string) error {
	ev := Event{SessionID: id, Action: "stop", Class: "L"}
	err := s.stop(id, "stopped by the agent")
	s.finish(ev, err)
	return err
}

func (s *Service) stop(id, reason string) error {
	if _, err := s.lookup(id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.sessions, id)
	s.notifyLocked()
	s.mu.Unlock()
	return s.cfg.Browser.Stop(id)
}

// Close stops every session. The owner calls it on exit.
func (s *Service) Close() error {
	s.mu.Lock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	s.sessions = map[string]*session{}
	s.notifyLocked()
	s.mu.Unlock()
	for _, id := range ids {
		s.emit(Event{SessionID: id, Action: "stop", Class: "L", Outcome: "stopped_owner_exit"})
	}
	return s.cfg.Browser.StopAll()
}

// WaitResult is local_cobrowse_wait's result.
type WaitResult struct {
	SessionID string `json:"session_id"`
	Until     string `json:"until"`
	Satisfied bool   `json:"satisfied"`
	// StillWaiting is true when the timeout passed; call again to keep waiting.
	StillWaiting bool   `json:"still_waiting"`
	State        string `json:"state"`
	Detail       string `json:"detail,omitempty"`
}

// Wait long-polls until the condition holds, a terminal answer arrives (denied,
// revoked, session ended), or timeout (capped at MaxWait) passes.
func (s *Service) Wait(ctx context.Context, id, until string, timeout time.Duration) (WaitResult, error) {
	ev := Event{SessionID: id, Action: "wait", Class: "L"}
	res, err := s.wait(ctx, id, until, timeout)
	if err == nil {
		ev.Outcome = res.State
	}
	s.finish(ev, err)
	return res, err
}

func (s *Service) wait(ctx context.Context, id, until string, timeout time.Duration) (WaitResult, error) {
	until = strings.ToLower(strings.TrimSpace(until))
	switch until {
	case "approved", "unlocked", "resumed":
	default:
		return WaitResult{}, fmt.Errorf("until must be approved, unlocked or resumed")
	}
	if timeout <= 0 || timeout > MaxWait {
		timeout = MaxWait
	}
	if _, err := s.lookup(id); err != nil {
		return WaitResult{}, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	res := WaitResult{SessionID: id, Until: until}
	for {
		s.mu.Lock()
		ss, ok := s.sessions[id]
		changed := s.changed
		var g grantState
		if ok {
			g = ss.grant
		}
		s.mu.Unlock()
		if !ok {
			res.State, res.Detail = "ended", "the session ended"
			return res, nil
		}
		st, live := s.cfg.Browser.SessionStatus(id)
		if !live || st.State == platform.SessionExited {
			res.State, res.Detail = "ended", "the browser exited"
			return res, nil
		}
		now := s.cfg.Now()
		switch until {
		case "approved":
			switch {
			case g.denied:
				res.State = "denied"
				return res, nil
			case g.revoked:
				res.State = "revoked"
				return res, nil
			case !g.expires.IsZero() && now.Before(g.expires):
				res.State, res.Satisfied = "approved", true
				return res, nil
			}
		case "resumed":
			if st.Driver != platform.DriverHuman {
				res.State, res.Satisfied = "resumed", true
				return res, nil
			}
		case "unlocked":
			// Throwaway sessions have no lock (persistent profiles are a later slice).
			res.State, res.Satisfied = "unlocked", true
			return res, nil
		}
		select {
		case <-changed:
		case <-timer.C:
			res.State, res.StillWaiting = "waiting", true
			return res, nil
		case <-ctx.Done():
			res.State, res.StillWaiting = "waiting", true
			return res, nil
		}
	}
}

// --- Human side. Only the local viewer calls these; they are never reachable
// through the MCP tool path.

// Approve grants AI writes in a session for d (default and maximum 24h).
func (s *Service) Approve(id string, d time.Duration) error {
	if d <= 0 || d > s.cfg.GrantDuration {
		d = s.cfg.GrantDuration
	}
	return s.setGrant(id, "approve", func(g *grantState, now time.Time) {
		*g = grantState{expires: now.Add(d)}
	})
}

// Deny refuses AI writes in a session.
func (s *Service) Deny(id string) error {
	return s.setGrant(id, "deny", func(g *grantState, _ time.Time) { *g = grantState{denied: true} })
}

// Revoke withdraws an earlier approval.
func (s *Service) Revoke(id string) error {
	return s.setGrant(id, "revoke", func(g *grantState, _ time.Time) { *g = grantState{revoked: true} })
}

func (s *Service) setGrant(id, action string, f func(*grantState, time.Time)) error {
	s.mu.Lock()
	ss, ok := s.sessions[id]
	if ok {
		f(&ss.grant, s.cfg.Now())
		s.notifyLocked()
	}
	s.mu.Unlock()
	ev := Event{SessionID: id, Action: action, Class: "H"}
	if !ok {
		s.finish(ev, ErrNoSession)
		return ErrNoSession
	}
	s.finish(ev, nil)
	return nil
}

// HumanTakeControl gives the user the browser (viewer "Take control").
func (s *Service) HumanTakeControl(id string) error {
	_, err := s.setDriver(id, true, "taken by the user")
	s.finish(Event{SessionID: id, Action: "take_control", Class: "H"}, err)
	return err
}

// HumanHandBack returns the browser to the agent (viewer "Hand back").
func (s *Service) HumanHandBack(id string) error {
	_, err := s.setDriver(id, false, "")
	s.finish(Event{SessionID: id, Action: "hand_back", Class: "H"}, err)
	return err
}

// HumanStop ends a session from the viewer.
func (s *Service) HumanStop(id string) error {
	err := s.stop(id, "stopped by the user")
	s.finish(Event{SessionID: id, Action: "stop", Class: "H"}, err)
	return err
}

// HumanNavigate opens url while the user drives. The user's own typed address
// is in-the-moment consent, but the address filter still applies.
func (s *Service) HumanNavigate(ctx context.Context, id, url string) error {
	ev := Event{SessionID: id, Action: "navigate", Class: "H", URL: url}
	err := func() error {
		if _, err := s.lookup(id); err != nil {
			return err
		}
		if !s.humanDriving(id) {
			return fmt.Errorf("take control before typing an address")
		}
		if err := s.cfg.URLPolicy(ctx, url); err != nil {
			return err
		}
		_, err := s.cfg.Browser.NavigateAsHuman(id, url)
		return err
	}()
	s.finish(ev, err)
	return err
}
