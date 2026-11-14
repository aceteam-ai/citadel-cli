// Package localcobrowse is Citadel local mode's co-browse owner (aceteam#13503
// design sections 2.1 to 2.5 and 4): one process per OS user that holds the
// browser sessions, the per-domain write consent, the abuse limits and the
// navigation address filter, and serves the local_cobrowse_* MCP tools.
//
// Tool classes follow the platform's split. W (write: navigate, click, type,
// select, key, scroll) needs a live, human-given grant for the registrable
// domain it acts on (aceteam#13687 D3), and a commit action (send, post, submit,
// pay, delete, or an unidentifiable target) additionally needs a human answer
// to that one action every time (D1, D2, D5). R (read:
// status, extract, screenshot) and L (lifecycle: start, handoff, resume, stop,
// wait) do not. While a human has taken control, every agent browser action,
// including screenshot and extract, is refused by the session manager.
//
// Consent can only be given by the human in the local viewer; nothing reachable
// through the MCP tool path can grant it. The viewer side lives in a later
// slice and calls ApproveDomain, Deny, Revoke, ApproveCommit, DenyCommit,
// HumanTakeControl and HumanHandBack.
package localcobrowse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	// DefaultGrantDuration is a domain grant's default length (aceteam#13687
	// D4); MaxGrantDuration is the longest any approval may last.
	DefaultGrantDuration = 8 * time.Hour
	MaxGrantDuration     = 24 * time.Hour
	// A domain where the user has signed in or otherwise driven the browser is
	// identity-bearing: an agent there acts as that person, so its grant gets
	// a shorter absolute cap and an idle timeout.
	IdentityGrantDuration = time.Hour
	IdentityIdleTimeout   = 15 * time.Minute
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
	// InspectTarget describes what an action would act on, without acting.
	InspectTarget(id string, p platform.TargetProbe) (platform.TargetInfo, error)
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
	ErrApprovalRequired = errors.New("approval_required: ask the user to approve AI actions on this site in the Citadel viewer, then call local_cobrowse_wait with until=approved")
	ErrApprovalDenied   = errors.New("approval_denied: the user denied AI actions in this session; do not retry writes unless the user asks")
	ErrApprovalExpired  = errors.New("approval_expired: the user's approval for this site expired; ask them to approve again in the Citadel viewer, then call local_cobrowse_wait with until=approved")
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
	// Agent is the connected MCP client's name at the time of the call.
	Agent string
	// PendingSeq is the audit seq of this write's pending record, set on
	// the outcome event.
	PendingSeq int64
	// Domain is the registrable domain a grant or commit prompt concerns.
	Domain string
	// Fingerprint is the first 16 hex characters of the action fingerprint of a
	// commit prompt, approval, denial or execution.
	Fingerprint string
	// Label is the target element's own label (a button caption), bounded and
	// stripped of control characters. Never typed text.
	Label string
}

// Config configures a Service. Zero values take the local-mode defaults.
type Config struct {
	Browser            Browser
	MaxSessions        int
	WritesPerMinute    int
	NavigationsPerHour int
	ReadsPerMinute     int
	IdleLifetime       time.Duration
	// GrantDuration is the default domain grant length; MaxGrantDuration caps
	// any approval; IdentityGrantDuration and IdentityIdleTimeout cap grants on
	// identity-bearing domains (D4).
	GrantDuration         time.Duration
	MaxGrantDuration      time.Duration
	IdentityGrantDuration time.Duration
	IdentityIdleTimeout   time.Duration
	// URLPolicy decides which URLs may be opened. Nil uses NewURLPolicy with the
	// system resolver and ViewerPorts.
	URLPolicy URLPolicy
	// ViewerPorts reports ports the browser must never be pointed at.
	ViewerPorts func() []int
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
	// BeforeAction is called after a write passes its gates and before it
	// reaches the browser; an error refuses the write. The owner records a
	// pending audit record here, so a write never runs unrecorded.
	BeforeAction func(Event) (pendingSeq int64, err error)
	// Precheck runs before every co-browse call; an error refuses the call.
	// The owner uses it to fail closed when the audit log cannot be written.
	Precheck func() error
	// AcquireOwner is called before the first session starts. A non-nil error
	// (another process already owns co-browse for this user) refuses every
	// local_cobrowse_* call. Nil means no single-instance check.
	AcquireOwner func() error
	// OnAttention is told when the human is needed (reason "approval" or
	// "handoff") or a session starts ("start"), so the viewer can be shown.
	OnAttention func(sessionID, reason string)
	// OnEvent receives every call outcome, including refusals.
	OnEvent func(Event)
	// ViewerNote is returned in local_cobrowse_start's "viewer" field: where
	// the user watches and approves. It never contains a secret.
	ViewerNote string
}

// grantState is one domain's consent record within a session. Grants belong
// to a session and end with it (D3, D4): nothing here is persisted.
type grantState struct {
	approvedAt time.Time
	expires    time.Time
	lastUsed   time.Time
	denied     bool
	revoked    bool
}

type session struct {
	id         string
	goal       string
	startedAt  time.Time
	lastActive time.Time
	// grants is keyed by registrable domain (D3).
	grants map[string]*grantState
	// humanDrove is set once a human has driven this session's browser. The
	// session is then identity-bearing (D4): a sign-in may have happened on any
	// domain, including one reached through an SSO redirect while they drove, so
	// every grant in the session takes the identity caps.
	humanDrove bool
	// askedDomain is the domain the last refused write needed a grant for.
	askedDomain string
	// approvalAsked is set when a write was refused for lack of a grant, so
	// the viewer shows the approval card; any human answer clears it.
	approvalAsked bool
	// commit is the one pending send/post/submit/pay request; deniedFP are
	// actions the user declined (sticky for the session); lastCommit is the
	// last outcome (executed, changed, denied, failed, expired, revoked).
	commit     *pendingCommit
	deniedFP   map[string]bool
	lastCommit string
	// result is the last commit's outcome, shown on the card and in status.
	result *CommitResult
	// wmu serializes every write in the session, from the checks through the
	// probe, budgets, audit, dispatch and settle, and the human's approve
	// (which runs the approved action). Lock order: wmu, then Service.mu.
	wmu           sync.Mutex
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
	if cfg.MaxGrantDuration <= 0 {
		cfg.MaxGrantDuration = MaxGrantDuration
	}
	if cfg.GrantDuration > cfg.MaxGrantDuration {
		cfg.GrantDuration = cfg.MaxGrantDuration
	}
	if cfg.IdentityGrantDuration <= 0 {
		cfg.IdentityGrantDuration = IdentityGrantDuration
	}
	if cfg.IdentityIdleTimeout <= 0 {
		cfg.IdentityIdleTimeout = IdentityIdleTimeout
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
	if ev.Agent == "" {
		ev.Agent = s.Agent()
	}
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
func (s *Service) ensureOwner() error { return s.ensureOwnerFor(false) }

// ensureOwnerFor skips Precheck for the human's safety controls (stop, take
// control): an audit failure must never leave the user unable to stop or take
// over a live session.
func (s *Service) ensureOwnerFor(humanSafety bool) error {
	// ownerMu, not mu: AcquireOwner may call back into the service (Agent).
	if s.cfg.Precheck != nil && !humanSafety {
		if err := s.cfg.Precheck(); err != nil {
			return err
		}
	}
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
	Title     string `json:"title,omitempty"`
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
	ss := &session{id: st.ID, goal: goal, startedAt: now, lastActive: now,
		grants: map[string]*grantState{}, deniedFP: map[string]bool{}}
	if startURL != "about:blank" {
		s.navs = append(s.navs, now)
	}
	s.sessions[st.ID] = ss
	s.notifyLocked()
	s.mu.Unlock()
	title := platform.SanitizeCobrowseTitle(st.Title)
	// A start URL can redirect. Check where the browser landed before any
	// read can see it.
	if startURL != "about:blank" {
		res := s.settle(ctx, st.ID, "")
		if res.Title != "" {
			title = res.Title
		}
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
		Title:     title,
		Browser:   st.Browser,
		Lifetime:  LifetimePolicy,
		Writes:    "navigate, click, type, select, key and scroll need the user's approval for each website in the Citadel viewer; anything that sends, posts, submits, pays or deletes asks every time",
		Viewer:    s.cfg.ViewerNote,
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
		ErrCommitApprovalRequired, ErrCommitPending, ErrCommitDenied, ErrNoPendingCommit, ErrUnknownPage, ErrPageChanged, ErrTypeNewline,
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
func (s *Service) lookup(id string) (*session, error) { return s.lookupFor(id, false) }

func (s *Service) lookupFor(id string, humanSafety bool) (*session, error) {
	if err := s.ensureOwnerFor(humanSafety); err != nil {
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

// grantEvalLocked returns a domain grant's state and effective expiry. On an
// identity-bearing domain the absolute cap and the idle timeout apply (D4); the
// check is made at use time, so a domain that becomes identity-bearing after an
// approval shortens that approval too. Caller holds s.mu.
func (s *Service) grantEvalLocked(ss *session, domain string, now time.Time) (string, time.Time) {
	g := ss.grants[domain]
	switch {
	case g == nil || (g.expires.IsZero() && !g.denied && !g.revoked):
		return "not_requested", time.Time{}
	case g.denied:
		return "denied", time.Time{}
	case g.revoked:
		return "revoked", time.Time{}
	}
	exp := g.expires
	if ss.humanDrove {
		if abs := g.approvedAt.Add(s.cfg.IdentityGrantDuration); abs.Before(exp) {
			exp = abs
		}
		if idle := g.lastUsed.Add(s.cfg.IdentityIdleTimeout); idle.Before(exp) {
			exp = idle
		}
	}
	if !now.Before(exp) {
		return "expired", exp
	}
	return "approved", exp
}

// requireGrant checks the domain grant for one write and, if it holds, records
// the use. A refusal remembers the domain so the viewer asks about it.
func (s *Service) requireGrant(id string, ss *session, domain string) error {
	if domain == UnknownDomain {
		return ErrUnknownPage
	}
	now := s.cfg.Now()
	s.mu.Lock()
	state, _ := s.grantEvalLocked(ss, domain, now)
	var err error
	switch state {
	case "approved":
		ss.grants[domain].lastUsed = now
	case "denied":
		err = fmt.Errorf("%w (domain %s)", ErrApprovalDenied, domain)
	case "revoked":
		err = fmt.Errorf("%w (domain %s)", ErrApprovalRevoked, domain)
	case "expired":
		err = fmt.Errorf("%w (domain %s)", ErrApprovalExpired, domain)
	default:
		err = fmt.Errorf("%w (domain %s)", ErrApprovalRequired, domain)
	}
	ask := errors.Is(err, ErrApprovalRequired) || errors.Is(err, ErrApprovalExpired)
	if ask {
		ss.askedDomain = domain
		if !ss.approvalAsked {
			ss.approvalAsked = true
			s.notifyLocked()
		}
	}
	s.mu.Unlock()
	if ask {
		s.attention(id, "approval")
	}
	return err
}

// beginWrite takes the session's write lock and runs the checks every write
// shares: the session exists, the user is not driving, and no send, post,
// submit or payment decision is outstanding (so the agent cannot change what
// the user is looking at). The caller must call unlock.
func (s *Service) beginWrite(id string) (ss *session, unlock func(), err error) {
	ss, err = s.lookup(id)
	if err != nil {
		return nil, nil, err
	}
	ss.wmu.Lock()
	if s.humanDriving(id) {
		ss.wmu.Unlock()
		return nil, nil, ErrHumanDriving
	}
	s.mu.Lock()
	s.expireCommitLocked(ss, s.cfg.Now())
	pending := ss.commit != nil
	s.mu.Unlock()
	if pending {
		ss.wmu.Unlock()
		return nil, nil, ErrCommitPending
	}
	return ss, ss.wmu.Unlock, nil
}

// expireCommitLocked drops a request nobody answered. Caller holds s.mu.
func (s *Service) expireCommitLocked(ss *session, now time.Time) {
	if ss.commit != nil && now.Sub(ss.commit.requestedAt) >= PendingCommitTTL {
		s.resolveCommitLocked(ss, ss.commit, "expired", "no answer in time, not executed", now)
	}
}

// resolveCommitLocked ends the pending request with an outcome the card and
// the agent's status show. Caller holds s.mu.
func (s *Service) resolveCommitLocked(ss *session, pc *pendingCommit, state, detail string, now time.Time) {
	ss.commit = nil
	ss.lastCommit = state
	ss.result = &CommitResult{ID: pc.id, Domain: pc.domain, Action: pc.action, State: state, Detail: detail, At: now.UTC().Format(time.RFC3339)}
	s.notifyLocked()
}

// spendBudget applies the owner-wide write (and navigation) budgets. The
// returned func gives the spend back, for a write that then fails before it is
// sent (an audit failure must not use up the user's or the agent's budget).
func (s *Service) spendBudget(navigation bool) (refund func(), err error) {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = pruneBefore(s.writes, now.Add(-time.Minute))
	if len(s.writes) >= s.cfg.WritesPerMinute {
		return nil, fmt.Errorf("%w: at most %d actions per minute; slow down", ErrRateLimited, s.cfg.WritesPerMinute)
	}
	if navigation {
		s.navs = pruneBefore(s.navs, now.Add(-time.Hour))
		if len(s.navs) >= s.cfg.NavigationsPerHour {
			return nil, fmt.Errorf("%w: at most %d page loads per hour", ErrRateLimited, s.cfg.NavigationsPerHour)
		}
	}
	s.writes = append(s.writes, now)
	if navigation {
		s.navs = append(s.navs, now)
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i := len(s.writes) - 1; i >= 0; i-- {
			if s.writes[i].Equal(now) {
				s.writes = append(s.writes[:i], s.writes[i+1:]...)
				break
			}
		}
		if navigation {
			for i := len(s.navs) - 1; i >= 0; i-- {
				if s.navs[i].Equal(now) {
					s.navs = append(s.navs[:i], s.navs[i+1:]...)
					break
				}
			}
		}
	}, nil
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
	Title      string `json:"title,omitempty"`
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
			res.Title = platform.SanitizeCobrowseTitle(ps.Title)
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
// or on a domain with no live grant. Either way the browser is returned to a
// blank page; a missing grant also raises the approval card for that domain, so
// a redirect cannot carry the agent onto a site the user never approved.
func (s *Service) enforceLanding(ctx context.Context, id string, ss *session, res ActionResult) error {
	if res.URL == "" || !res.Navigated {
		return nil
	}
	if err := s.cfg.URLPolicy(ctx, res.URL); err != nil {
		_, _ = s.cfg.Browser.Navigate(id, "about:blank")
		return fmt.Errorf("the page redirected to a refused address and was closed: %w", err)
	}
	if ss != nil {
		if err := s.requireGrant(id, ss, RegistrableDomain(res.URL)); err != nil {
			_, _ = s.cfg.Browser.Navigate(id, "about:blank")
			return fmt.Errorf("the browser landed on a site that is not approved and was returned to a blank page: %w", err)
		}
	}
	return nil
}

// finishAfter settles a dispatched write, counts a page load toward the budget
// and checks where the browser landed.
func (s *Service) finishAfter(ctx context.Context, id string, ss *session, before string, navigation bool) (ActionResult, error) {
	res := s.settle(ctx, id, before)
	if res.Navigated || navigation {
		s.mu.Lock()
		s.navs = append(pruneBefore(s.navs, s.cfg.Now().Add(-time.Hour)), s.cfg.Now())
		s.mu.Unlock()
	}
	return res, s.enforceLanding(ctx, id, ss, res)
}

// Navigate opens url in the session (W). An address that itself changes or ends
// something (a logout or delete path, an action=delete query) is a commit.
func (s *Service) Navigate(ctx context.Context, id, rawURL, goal string) (ActionResult, error) {
	ev := Event{SessionID: id, Action: "navigate", Class: "W", Goal: goal, URL: rawURL, Domain: RegistrableDomain(rawURL)}
	res, err := func() (ActionResult, error) {
		if err := s.cfg.URLPolicy(ctx, rawURL); err != nil {
			if _, lerr := s.lookup(id); lerr != nil {
				return ActionResult{}, lerr
			}
			return ActionResult{}, err
		}
		ss, unlock, err := s.beginWrite(id)
		if err != nil {
			return ActionResult{}, err
		}
		defer unlock()
		if err := s.requireGrant(id, ss, ev.Domain); err != nil {
			return ActionResult{}, err
		}
		a := actionRef{Kind: "navigate", URL: rawURL}
		if dec := ClassifyNavigation(rawURL); dec.Class == ClassCommit {
			return ActionResult{}, s.authorizeCommit(&ev, ss, a, platform.TargetInfo{Resolved: true}, dec, ev.Domain)
		}
		refund, err := s.spendBudget(true)
		if err != nil {
			return ActionResult{}, err
		}
		if err := s.beforeAction(&ev); err != nil {
			refund()
			return ActionResult{}, err
		}
		before := s.currentURL(id)
		if _, err := s.cfg.Browser.Navigate(id, rawURL); err != nil {
			return ActionResult{}, err
		}
		res := s.settle(ctx, id, before)
		if rawURL != before {
			res.Navigated = true
		}
		return res, s.enforceLanding(ctx, id, ss, res)
	}()
	s.finish(ev, err)
	return res, err
}

// Click clicks an element or a viewport point (W).
func (s *Service) Click(ctx context.Context, id, selector string, x, y *float64, goal string) (ActionResult, error) {
	ev := Event{SessionID: id, Action: "click", Class: "W", Goal: goal, Selector: selector}
	res, err := s.writeAction(ctx, &ev, actionRef{Kind: "click", Selector: selector, X: x, Y: y}, func() error {
		_, err := s.cfg.Browser.Click(id, selector, x, y)
		return err
	})
	s.finish(ev, err)
	return res, err
}

// Type inserts text into the focused element (W). Only its length is audited.
func (s *Service) Type(ctx context.Context, id, text, goal string) (ActionResult, error) {
	ev := Event{SessionID: id, Action: "type", Class: "W", Goal: goal, TextLen: len([]rune(text))}
	if strings.ContainsAny(text, "\r\n") {
		// A newline can send a message or submit a form, which is Enter, a
		// commit. Typing never carries it; use local_cobrowse_key Enter.
		s.finish(ev, ErrTypeNewline)
		return ActionResult{}, ErrTypeNewline
	}
	res, err := s.writeAction(ctx, &ev, actionRef{Kind: "type"}, func() error {
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
	res, err := s.writeAction(ctx, &ev, actionRef{Kind: "select", Selector: selector, Value: value, Label: label}, func() error {
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
	res, err := s.writeAction(ctx, &ev, actionRef{Kind: "key", Key: key}, func() error {
		_, err := s.cfg.Browser.Key(id, key)
		return err
	})
	s.finish(ev, err)
	return res, err
}

// Scroll scrolls at an element or point (W).
func (s *Service) Scroll(ctx context.Context, id, selector string, x, y *float64, dx, dy float64, goal string) (ActionResult, error) {
	ev := Event{SessionID: id, Action: "scroll", Class: "W", Goal: goal, Selector: selector}
	res, err := s.writeAction(ctx, &ev, actionRef{Kind: "scroll"}, func() error {
		_, err := s.cfg.Browser.Scroll(id, selector, x, y, dx, dy)
		return err
	})
	s.finish(ev, err)
	return res, err
}

// beforeAction records the pending event and stores its seq on ev, so the
// outcome record links back to it.
func (s *Service) beforeAction(ev *Event) error {
	if s.cfg.BeforeAction == nil {
		return nil
	}
	pending := *ev
	pending.Outcome = "pending"
	if pending.Agent == "" {
		pending.Agent = s.Agent()
	}
	seq, err := s.cfg.BeforeAction(pending)
	if err != nil {
		return err
	}
	ev.PendingSeq = seq
	return nil
}

// probe reads the target of an action.
func (s *Service) probe(id string, a actionRef) (platform.TargetInfo, error) {
	t, err := s.cfg.Browser.InspectTarget(id, platform.TargetProbe{Kind: a.Kind, Selector: a.Selector, X: a.X, Y: a.Y})
	if errors.Is(err, platform.ErrHandedOff) {
		return platform.TargetInfo{}, ErrHumanDriving
	}
	return t, err
}

// writeAction runs one scripted browser write under the session's write lock.
// Order: the user is not driving and no commit decision is pending; the page's
// domain has a live grant; the target is resolved and classified; a commit is
// recorded for the user and refused (it runs only when the user approves it,
// see ApproveCommit); otherwise the budgets are spent, the pending audit record
// is written, the target is probed again and, only if it is unchanged, the
// action is sent.
func (s *Service) writeAction(ctx context.Context, ev *Event, a actionRef, do func() error) (ActionResult, error) {
	id := ev.SessionID
	ss, unlock, err := s.beginWrite(id)
	if err != nil {
		return ActionResult{}, err
	}
	defer unlock()
	pageDomain := RegistrableDomain(s.currentURL(id))
	ev.Domain = pageDomain
	if err := s.requireGrant(id, ss, pageDomain); err != nil {
		return ActionResult{}, err
	}
	navigation := false
	classify := a.Kind != "scroll"
	var fp0 string
	var class0 Class
	if classify {
		t, err := s.probe(id, a)
		if errors.Is(err, ErrHumanDriving) {
			return ActionResult{}, err
		}
		if err != nil {
			// Fail closed: an action whose target cannot be read is a commit.
			t = platform.TargetInfo{Reason: "the page could not be inspected"}
		}
		// The page may have moved since the grant check: the probe's own
		// address is the authority for which domain the action lands on.
		if t.PageURL != "" {
			if d := RegistrableDomain(t.PageURL); d != pageDomain {
				pageDomain, ev.Domain = d, d
				if err := s.requireGrant(id, ss, d); err != nil {
					return ActionResult{}, err
				}
			}
		}
		dec := Classify(a.Kind, a.Key, t)
		switch dec.Class {
		case ClassNavigate:
			navigation = true
			if dec.Domain != pageDomain {
				if err := s.requireGrant(id, ss, dec.Domain); err != nil {
					return ActionResult{}, err
				}
			}
		case ClassCommit:
			return ActionResult{}, s.authorizeCommit(ev, ss, a, t, dec, pageDomain)
		}
		fp0, class0 = fingerprint(a, t), dec.Class
	}
	refund, err := s.spendBudget(false)
	if err != nil {
		return ActionResult{}, err
	}
	if err := s.beforeAction(ev); err != nil {
		refund()
		return ActionResult{}, err
	}
	if classify {
		// Re-probe immediately before dispatch: the action goes out only if
		// the target is exactly what was classified.
		t2, err := s.probe(id, a)
		if errors.Is(err, ErrHumanDriving) {
			return ActionResult{}, err
		}
		if err != nil || fingerprint(a, t2) != fp0 || Classify(a.Kind, a.Key, t2).Class != class0 {
			return ActionResult{}, ErrPageChanged
		}
	}
	before := s.currentURL(id)
	if err := do(); err != nil {
		return ActionResult{}, err
	}
	return s.finishAfter(ctx, id, ss, before, navigation)
}

// authorizeCommit records a commit for the user and returns the refusal the
// agent sees: a sticky ErrCommitDenied for an action the user already
// declined, otherwise ErrCommitApprovalRequired. Only the action reference and
// its fingerprint are kept, never typed text; the action runs from
// ApproveCommit if and only if the user approves it.
func (s *Service) authorizeCommit(ev *Event, ss *session, a actionRef, t platform.TargetInfo, dec Decision, domain string) error {
	fp := fingerprint(a, t)
	label := sanitizeLabel(t.Name)
	now := s.cfg.Now()
	ev.Fingerprint, ev.Label = fp[:16], label
	s.mu.Lock()
	s.expireCommitLocked(ss, now)
	if ss.deniedFP[fp] {
		s.mu.Unlock()
		return ErrCommitDenied
	}
	pc := &pendingCommit{
		id: newCommitID(), fp: fp, ref: a, domain: domain, action: describeAction(a, t), label: label,
		reason: dec.Reason, requestedAt: now, rect: t.Rect,
	}
	ss.commit = pc
	ss.lastCommit, ss.result = "", nil
	s.notifyLocked()
	s.mu.Unlock()

	// The screenshot is the human's evidence of what is about to happen. It
	// stays in memory and is dropped with the request.
	if png, err := s.cfg.Browser.Screenshot(ss.id); err == nil {
		s.mu.Lock()
		if ss.commit == pc {
			pc.shot = png
		}
		s.mu.Unlock()
	}
	s.emit(Event{SessionID: ss.id, Action: "commit_prompt", Class: "W", Goal: ev.Goal, Domain: domain,
		Fingerprint: fp[:16], Label: label, Outcome: "asked"})
	s.attention(ss.id, "approval")
	return ErrCommitApprovalRequired
}

func newCommitID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// sanitizeLabel bounds a control's label and drops control characters before
// it reaches a card or the audit log.
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	return truncRunes(strings.TrimSpace(b.String()), 80)
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
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	Driver    string `json:"driver"`
	URL       string `json:"url,omitempty"`
	Browser   string `json:"browser,omitempty"`
	Goal      string `json:"goal,omitempty"`
	StartedAt string `json:"started_at"`
	// Domain is the registrable domain of the current page. Approval and
	// GrantExpiresAt describe that domain's grant; Grants lists them all.
	Domain   string `json:"domain,omitempty"`
	Approval string `json:"approval"`
	// ApprovalAsked is true while an agent write is waiting for an answer, and
	// AskedDomain is the domain it needs a grant for.
	ApprovalAsked  bool        `json:"approval_asked,omitempty"`
	AskedDomain    string      `json:"asked_domain,omitempty"`
	GrantExpiresAt string      `json:"grant_expires_at,omitempty"`
	Grants         []GrantView `json:"grants,omitempty"`
	// Commit is the send, post, submit or payment waiting for the user.
	Commit        *CommitView   `json:"commit,omitempty"`
	CommitResult  *CommitResult `json:"commit_result,omitempty"`
	HandoffSince  string        `json:"handoff_since,omitempty"`
	HandoffReason string        `json:"handoff_reason,omitempty"`
	Lifetime      string        `json:"lifetime"`
}

// GrantView is one live domain grant.
type GrantView struct {
	Domain          string `json:"domain"`
	ExpiresAt       string `json:"expires_at"`
	IdentityBearing bool   `json:"identity_bearing,omitempty"`
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
		SessionID:     ss.id,
		State:         string(st.State),
		Driver:        string(st.Driver),
		URL:           st.URL,
		Browser:       st.Browser,
		Goal:          ss.goal,
		StartedAt:     ss.startedAt.UTC().Format(time.RFC3339),
		Lifetime:      LifetimePolicy,
		ApprovalAsked: ss.approvalAsked,
	}
	v.Domain = RegistrableDomain(st.URL)
	if ss.approvalAsked {
		v.AskedDomain = ss.askedDomain
	}
	state, exp := s.grantEvalLocked(ss, v.Domain, now)
	v.Approval = state
	if state == "approved" {
		v.GrantExpiresAt = exp.UTC().Format(time.RFC3339)
	}
	doms := make([]string, 0, len(ss.grants))
	for d := range ss.grants {
		doms = append(doms, d)
	}
	sort.Strings(doms)
	for _, d := range doms {
		if st2, e := s.grantEvalLocked(ss, d, now); st2 == "approved" {
			v.Grants = append(v.Grants, GrantView{Domain: d, ExpiresAt: e.UTC().Format(time.RFC3339), IdentityBearing: ss.humanDrove})
		}
	}
	if c := ss.commit; c != nil && now.Sub(c.requestedAt) < PendingCommitTTL {
		cv := &CommitView{ID: c.id, Domain: c.domain, Action: c.action, Label: c.label, Reason: c.reason,
			RequestedAt: c.requestedAt.UTC().Format(time.RFC3339), HasShot: c.shot != ""}
		if c.rect.W > 0 && c.rect.H > 0 {
			r := c.rect
			cv.Rect = &r
		}
		v.Commit = cv
	}
	if r := ss.result; r != nil {
		if at, err := time.Parse(time.RFC3339, r.At); err == nil && now.Sub(at) < 5*time.Minute {
			rc := *r
			v.CommitResult = &rc
		}
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
	return s.setDriverFor(id, human, reason, false)
}

func (s *Service) setDriverFor(id string, human bool, reason string, humanSafety bool) (SessionView, error) {
	ss, err := s.lookupFor(id, humanSafety)
	if err != nil {
		return SessionView{}, err
	}
	var st platform.CobrowseSessionStatus
	// A human driving a page is the identity boundary (D4): whatever they do
	// there, the page may now act as them.
	if human {
		s.markIdentity(ss)
		st, err = s.cfg.Browser.Handoff(id)
	} else {
		st, err = s.cfg.Browser.Resume(id)
	}
	if err != nil {
		return SessionView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The page is about to change under the user's hands: an unanswered or
	// unused commit approval no longer describes it.
	if ss.commit != nil {
		s.resolveCommitLocked(ss, ss.commit, "expired", "you took control, so it was not executed", s.cfg.Now())
	}
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

// markIdentity records that a human has driven the session.
func (s *Service) markIdentity(ss *session) {
	s.mu.Lock()
	ss.humanDrove = true
	s.mu.Unlock()
}

// Stop ends a session and deletes its throwaway profile (L).
func (s *Service) Stop(ctx context.Context, id string) error {
	ev := Event{SessionID: id, Action: "stop", Class: "L"}
	err := s.stop(id, "stopped by the agent")
	s.finish(ev, err)
	return err
}

func (s *Service) stop(id, reason string) error { return s.stopFor(id, reason, false) }

func (s *Service) stopFor(id, reason string, humanSafety bool) error {
	if _, err := s.lookupFor(id, humanSafety); err != nil {
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
	case "approved", "unlocked", "resumed", "commit":
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
		var gstate, lastCommit string
		var commitPending bool
		if ok {
			s.expireCommitLocked(ss, s.cfg.Now())
			dom := ss.askedDomain
			if dom == "" {
				if cur, live := s.cfg.Browser.SessionStatus(id); live {
					dom = RegistrableDomain(cur.URL)
				}
			}
			gstate, _ = s.grantEvalLocked(ss, dom, s.cfg.Now())
			lastCommit, commitPending = ss.lastCommit, ss.commit != nil
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
		switch until {
		case "approved":
			switch gstate {
			case "denied", "revoked":
				res.State = gstate
				return res, nil
			case "approved":
				res.State, res.Satisfied = "approved", true
				return res, nil
			}
		case "commit":
			if !commitPending {
				if lastCommit == "" {
					lastCommit = "none"
				}
				res.State, res.Satisfied = lastCommit, true
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

// RecordRefusal records a call refused before it reached the service, such as
// malformed tool arguments, so every refusal is audited.
func (s *Service) RecordRefusal(sessionID, action, class string, err error) {
	if err == nil {
		return
	}
	s.emit(Event{SessionID: sessionID, Action: action, Class: class, Outcome: "refused", Error: err.Error()})
}

// --- Human side. Only the local viewer calls these; they are never reachable
// through the MCP tool path.

// Approve grants AI writes on the domain the agent last asked about (or, if
// none, the current page's domain) for d. The viewer names the domain with
// ApproveDomain; this form exists for callers that only know the session.
func (s *Service) Approve(id string, d time.Duration) error { return s.ApproveDomain(id, "", d) }

// ApproveDomain grants AI fill and navigate actions on one registrable domain
// for d: the default when d <= 0, never more than the maximum, and on an
// identity-bearing domain never more than the identity cap (D3, D4). It never
// covers a commit action (D1).
func (s *Service) ApproveDomain(id, domain string, d time.Duration) error {
	if domain == UnknownDomain {
		return ErrUnknownPage
	}
	return s.setGrant(id, "approve", domain, func(ss *session, dom string, now time.Time) {
		if d <= 0 {
			d = s.cfg.GrantDuration
		}
		if d > s.cfg.MaxGrantDuration {
			d = s.cfg.MaxGrantDuration
		}
		if ss.humanDrove && d > s.cfg.IdentityGrantDuration {
			d = s.cfg.IdentityGrantDuration
		}
		ss.grants[dom] = &grantState{approvedAt: now, expires: now.Add(d), lastUsed: now}
	}, true)
}

// Deny refuses AI writes on the domain the agent last asked about.
func (s *Service) Deny(id string) error {
	return s.setGrant(id, "deny", "", func(ss *session, dom string, _ time.Time) {
		ss.grants[dom] = &grantState{denied: true}
	}, true)
}

// Revoke withdraws every grant in the session and any approved commit.
func (s *Service) Revoke(id string) error {
	return s.setGrant(id, "revoke", "", func(ss *session, _ string, _ time.Time) {
		for _, g := range ss.grants {
			if !g.denied {
				*g = grantState{revoked: true}
			}
		}
		if ss.commit != nil {
			s.resolveCommitLocked(ss, ss.commit, "revoked", "approvals were revoked, so it was not executed", s.cfg.Now())
		}
	}, false)
}

// setGrant applies a human consent change. domain "" means the domain asked
// about, else the current page's.
func (s *Service) setGrant(id, action, domain string, f func(*session, string, time.Time), perDomain bool) error {
	s.mu.Lock()
	ss, ok := s.sessions[id]
	dom := domain
	if ok && perDomain && dom == "" {
		dom = ss.askedDomain
	}
	s.mu.Unlock()
	if ok && perDomain && dom == "" {
		if st, live := s.cfg.Browser.SessionStatus(id); live {
			dom = RegistrableDomain(st.URL)
		}
	}
	s.mu.Lock()
	ss, ok = s.sessions[id]
	if ok {
		f(ss, dom, s.cfg.Now())
		ss.approvalAsked = false
		ss.askedDomain = ""
		s.notifyLocked()
	}
	s.mu.Unlock()
	ev := Event{SessionID: id, Action: action, Class: "H", Domain: dom}
	if !ok {
		s.finish(ev, ErrNoSession)
		return ErrNoSession
	}
	s.finish(ev, nil)
	return nil
}

// ApproveCommit approves the one pending send, post, submit, payment or
// destructive address, named by the id the viewer showed (so an answer to a
// stale card cannot approve a newer action), and runs it (aceteam#13687 D5).
// It holds the session's write lock throughout: it probes the live target
// again, requires the fingerprint to equal the approved one, and only then
// spends the budgets, writes the pending audit record and dispatches. If the
// page changed it does not execute and says so on the card. A budget or audit
// failure leaves the request pending, so the approval is not lost.
func (s *Service) ApproveCommit(ctx context.Context, id, commitID string) error {
	s.mu.Lock()
	ss, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		s.finish(Event{SessionID: id, Action: "approve_commit", Class: "H"}, ErrNoSession)
		return ErrNoSession
	}
	ss.wmu.Lock()
	defer ss.wmu.Unlock()

	s.mu.Lock()
	s.expireCommitLocked(ss, s.cfg.Now())
	pc := ss.commit
	s.mu.Unlock()
	if pc == nil || pc.id != commitID {
		s.finish(Event{SessionID: id, Action: "approve_commit", Class: "H"}, ErrNoPendingCommit)
		return ErrNoPendingCommit
	}
	hev := Event{SessionID: id, Action: "approve_commit", Class: "H", Domain: pc.domain, Fingerprint: pc.fp[:16], Label: pc.label}
	resolve := func(state, detail string) {
		s.mu.Lock()
		if ss.commit == pc {
			s.resolveCommitLocked(ss, pc, state, detail, s.cfg.Now())
		}
		s.mu.Unlock()
	}
	if s.humanDriving(id) {
		s.finish(hev, ErrHumanDriving)
		return ErrHumanDriving
	}
	a := pc.ref
	t := platform.TargetInfo{Resolved: true}
	if a.Kind != "navigate" {
		var err error
		if t, err = s.probe(id, a); err != nil {
			t = platform.TargetInfo{Reason: "the page could not be inspected"}
		}
	}
	if fingerprint(a, t) != pc.fp {
		resolve("changed", "the page changed, so it was not executed; the agent will ask again")
		hev.Outcome = "page_changed"
		s.emit(hev)
		return nil
	}
	if err := s.requireGrant(id, ss, pc.domain); err != nil {
		resolve("failed", "the approval for this site is no longer valid, so it was not executed")
		s.finish(hev, err)
		return err
	}
	ev := Event{SessionID: id, Action: a.Kind, Class: "W", Selector: a.Selector, URL: a.URL,
		Domain: pc.domain, Fingerprint: pc.fp[:16], Label: pc.label}
	refund, err := s.spendBudget(a.Kind == "navigate")
	if err != nil {
		s.finish(hev, err)
		return err
	}
	if err := s.beforeAction(&ev); err != nil {
		refund()
		s.finish(hev, err)
		return err
	}
	before := s.currentURL(id)
	derr := s.dispatchCommit(id, a)
	if derr == nil {
		_, derr = s.finishAfter(ctx, id, ss, before, a.Kind == "navigate")
	}
	s.finish(ev, derr)
	if derr != nil {
		resolve("failed", "the action did not complete: "+truncRunes(derr.Error(), 120))
		hev.Outcome = "error"
		s.emit(hev)
		return nil
	}
	resolve("executed", "approved and done")
	hev.Outcome = "ok"
	s.emit(hev)
	return nil
}

// dispatchCommit sends an approved action to the browser.
func (s *Service) dispatchCommit(id string, a actionRef) error {
	var err error
	switch a.Kind {
	case "click":
		_, err = s.cfg.Browser.Click(id, a.Selector, a.X, a.Y)
	case "key":
		_, err = s.cfg.Browser.Key(id, a.Key)
	case "select":
		_, err = s.cfg.Browser.Select(id, a.Selector, a.Value, a.Label)
	case "navigate":
		_, err = s.cfg.Browser.Navigate(id, a.URL)
	default:
		err = fmt.Errorf("unknown action %q", a.Kind)
	}
	return err
}

// DenyCommit declines the pending commit; the same action is then refused for
// the rest of the session.
func (s *Service) DenyCommit(id, commitID string) error {
	s.mu.Lock()
	ss, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		s.finish(Event{SessionID: id, Action: "deny_commit", Class: "H"}, ErrNoSession)
		return ErrNoSession
	}
	ss.wmu.Lock()
	defer ss.wmu.Unlock()
	s.mu.Lock()
	s.expireCommitLocked(ss, s.cfg.Now())
	pc := ss.commit
	var err error
	if pc == nil || pc.id != commitID {
		err = ErrNoPendingCommit
	} else {
		ss.deniedFP[pc.fp] = true
		s.resolveCommitLocked(ss, pc, "denied", "you declined it", s.cfg.Now())
	}
	s.mu.Unlock()
	ev := Event{SessionID: id, Action: "deny_commit", Class: "H"}
	if pc != nil {
		ev.Domain, ev.Fingerprint, ev.Label = pc.domain, pc.fp[:16], pc.label
	}
	s.finish(ev, err)
	return err
}

// CommitShot returns the pending commit's screenshot (base64 PNG) and target
// box for the viewer's approval card. ok is false when it is not (or no
// longer) the pending request.
func (s *Service) CommitShot(id, commitID string) (png string, rect Rect, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, found := s.sessions[id]
	if !found || ss.commit == nil || ss.commit.id != commitID || ss.commit.shot == "" {
		return "", Rect{}, false
	}
	return ss.commit.shot, ss.commit.rect, true
}

// HumanTakeControl gives the user the browser (viewer "Take control").
func (s *Service) HumanTakeControl(id string) error {
	_, err := s.setDriverFor(id, true, "taken by the user", true)
	s.finish(Event{SessionID: id, Action: "take_control", Class: "H"}, err)
	return err
}

// HumanHandBack returns the browser to the agent (viewer "Hand back").
func (s *Service) HumanHandBack(id string) error {
	if ss, err := s.lookup(id); err == nil {
		s.markIdentity(ss) // the human drove until now
	}
	_, err := s.setDriver(id, false, "")
	s.finish(Event{SessionID: id, Action: "hand_back", Class: "H"}, err)
	return err
}

// HumanStop ends a session from the viewer.
func (s *Service) HumanStop(id string) error {
	err := s.stopFor(id, "stopped by the user", true)
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
		if err == nil {
			if ss, lerr := s.lookup(id); lerr == nil {
				s.markIdentity(ss)
			}
		}
		return err
	}()
	s.finish(ev, err)
	return err
}
