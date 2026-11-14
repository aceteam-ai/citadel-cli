package localcobrowse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// fakeBrowser is an in-memory Browser. Every browser action honors the human
// driver exactly like the real session manager (ErrHandedOff).
type fakeBrowser struct {
	mu       sync.Mutex
	n        int
	sessions map[string]*fakeSession
	calls    []string
	// startLandsOn simulates a start URL that redirects.
	startLandsOn string
	// clickNavigatesTo simulates a link click that loads another page.
	clickNavigatesTo string
	// navigateLandsOn simulates a navigate that redirects; pageTitle is the
	// title every page reports; notReady keeps readyState at "loading".
	navigateLandsOn string
	pageTitle       string
	notReady        bool
	// startErr fails StartSessionWithID; stopErr fails Stop.
	startErr error
	stopErr  error
	// target, when set, is what InspectTarget reports (default: a plain
	// element on the current page).
	target *platform.TargetInfo
	// onClick runs (outside the lock) when a click arrives; onInspect on each
	// InspectTarget call.
	onClick   func()
	onInspect func()
}

type fakeSession struct {
	url     string
	human   bool
	exited  bool
	profile platform.SessionProfile
}

func newFakeBrowser() *fakeBrowser { return &fakeBrowser{sessions: map[string]*fakeSession{}} }

func (f *fakeBrowser) record(c string) { f.calls = append(f.calls, c) }

func (f *fakeBrowser) statusLocked(id string) platform.CobrowseSessionStatus {
	s := f.sessions[id]
	st := platform.CobrowseSessionStatus{ID: id, State: platform.SessionRunning, URL: s.url, Driver: platform.DriverAI, Browser: "edge"}
	if s.human {
		st.Driver = platform.DriverHuman
	}
	if s.exited {
		st.State = platform.SessionExited
	}
	return st
}

func (f *fakeBrowser) drivable(id string) (*fakeSession, error) {
	s, ok := f.sessions[id]
	if !ok {
		return nil, fmt.Errorf("no such browser session: %q", id)
	}
	if s.human {
		return nil, platform.ErrHandedOff
	}
	return s, nil
}

func (f *fakeBrowser) StartSession(u string) (platform.CobrowseSessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := fmt.Sprintf("cb-%d", f.n)
	land := u
	if f.startLandsOn != "" {
		land = f.startLandsOn
	}
	f.sessions[id] = &fakeSession{url: land}
	f.record("start " + u)
	return f.statusLocked(id), nil
}
func (f *fakeBrowser) StartSessionWithID(id, u string, p platform.SessionProfile) (platform.CobrowseSessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		if p != nil {
			_ = p.Close()
		}
		return platform.CobrowseSessionStatus{}, f.startErr
	}
	land := u
	if f.startLandsOn != "" {
		land = f.startLandsOn
	}
	f.sessions[id] = &fakeSession{url: land, profile: p}
	if p != nil {
		f.record("start-profile " + id)
	} else {
		f.record("start-id " + id)
	}
	return f.statusLocked(id), nil
}
func (f *fakeBrowser) SessionStatus(id string) (platform.CobrowseSessionStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[id]; !ok {
		return platform.CobrowseSessionStatus{}, false
	}
	return f.statusLocked(id), true
}
func (f *fakeBrowser) Stop(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, id)
	f.record("stop " + id)
	return f.stopErr
}
func (f *fakeBrowser) StopAll() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = map[string]*fakeSession{}
	return nil
}
func (f *fakeBrowser) act(id, call string, mut func(*fakeSession)) (platform.CobrowseSessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.drivable(id)
	if err != nil {
		return platform.CobrowseSessionStatus{}, err
	}
	f.record(call)
	if mut != nil {
		mut(s)
	}
	return f.statusLocked(id), nil
}
func (f *fakeBrowser) Navigate(id, u string) (platform.CobrowseSessionStatus, error) {
	return f.act(id, "navigate "+u, func(s *fakeSession) {
		s.url = u
		if f.navigateLandsOn != "" && u != "about:blank" {
			s.url = f.navigateLandsOn
		}
	})
}
func (f *fakeBrowser) NavigateAsHuman(id, u string) (platform.CobrowseSessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[id]
	if s == nil || !s.human {
		return platform.CobrowseSessionStatus{}, errors.New("not human")
	}
	s.url = u
	f.record("human-navigate " + u)
	return f.statusLocked(id), nil
}
func (f *fakeBrowser) Click(id, sel string, x, y *float64) (platform.CobrowseSessionStatus, error) {
	if f.onClick != nil {
		f.onClick()
	}
	return f.act(id, "click "+sel, func(s *fakeSession) {
		if f.clickNavigatesTo != "" {
			s.url = f.clickNavigatesTo
		}
	})
}
func (f *fakeBrowser) Type(id, text string) (platform.CobrowseSessionStatus, error) {
	return f.act(id, "type", nil)
}
func (f *fakeBrowser) Select(id, sel, value, label string) (platform.SelectResult, error) {
	_, err := f.act(id, "select "+sel, nil)
	return platform.SelectResult{Value: value, Label: label}, err
}
func (f *fakeBrowser) Key(id, key string) (platform.CobrowseSessionStatus, error) {
	return f.act(id, "key "+key, nil)
}
func (f *fakeBrowser) Scroll(id, sel string, x, y *float64, dx, dy float64) (platform.CobrowseSessionStatus, error) {
	return f.act(id, "scroll", nil)
}
func (f *fakeBrowser) ExtractBounded(id, sel string, attrs []string, max int) (platform.ExtractResult, error) {
	_, err := f.act(id, "extract "+sel, nil)
	return platform.ExtractResult{Text: "hello"}, err
}
func (f *fakeBrowser) Screenshot(id string) (string, error) {
	_, err := f.act(id, "screenshot", nil)
	return "iVBORw0KGgo=", err
}
func (f *fakeBrowser) PageState(id string) (platform.PageState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.drivable(id)
	if err != nil {
		return platform.PageState{}, err
	}
	rs := "complete"
	if f.notReady {
		rs = "loading"
	}
	return platform.PageState{URL: s.url, ReadyState: rs, Title: f.pageTitle}, nil
}
func (f *fakeBrowser) InspectTarget(id string, _ platform.TargetProbe) (platform.TargetInfo, error) {
	if f.onInspect != nil {
		f.onInspect()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.drivable(id)
	if err != nil {
		return platform.TargetInfo{}, err
	}
	if f.target != nil {
		t := *f.target
		if t.PageURL == "" {
			t.PageURL = s.url
		}
		return t, nil
	}
	return platform.TargetInfo{Resolved: true, Tag: "div", PageURL: s.url}, nil
}
func (f *fakeBrowser) Handoff(id string) (platform.CobrowseSessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[id].human = true
	return f.statusLocked(id), nil
}
func (f *fakeBrowser) Resume(id string) (platform.CobrowseSessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[id].human = false
	return f.statusLocked(id), nil
}

func allowAll(context.Context, string) error { return nil }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestService(t *testing.T, mod func(*Config)) (*Service, *fakeBrowser, *[]Event) {
	t.Helper()
	settleWindow, settleMinimum, settlePoll = 50*time.Millisecond, 0, 5*time.Millisecond
	fb := newFakeBrowser()
	var mu sync.Mutex
	events := &[]Event{}
	cfg := Config{
		Browser:   fb,
		URLPolicy: allowAll,
		OnEvent: func(e Event) {
			mu.Lock()
			*events = append(*events, e)
			mu.Unlock()
		},
	}
	if mod != nil {
		mod(&cfg)
	}
	return New(cfg), fb, events
}

var ctx = context.Background()

func TestWritesRefusedWithoutGrant(t *testing.T) {
	var attention []string
	svc, fb, _ := newTestService(t, func(c *Config) {
		c.OnAttention = func(id, reason string) { attention = append(attention, reason) }
	})
	st, err := svc.Start(ctx, "http://a.test/", "find orders", "")
	if err != nil {
		t.Fatal(err)
	}
	id := st.SessionID
	writes := map[string]func() error{
		"navigate": func() error { _, err := svc.Navigate(ctx, id, "http://b.test/", "g"); return err },
		"click":    func() error { _, err := svc.Click(ctx, id, "#x", nil, nil, "g"); return err },
		"type":     func() error { _, err := svc.Type(ctx, id, "secret", "g"); return err },
		"select":   func() error { _, err := svc.Select(ctx, id, "#s", "v", "", "g"); return err },
		"key":      func() error { _, err := svc.Key(ctx, id, "Tab", "g"); return err },
		"scroll":   func() error { _, err := svc.Scroll(ctx, id, "#x", nil, nil, 0, 100, "g"); return err },
	}
	for name, w := range writes {
		if err := w(); !errors.Is(err, ErrApprovalRequired) {
			t.Errorf("%s without a grant: want approval_required, got %v", name, err)
		}
	}
	for _, c := range fb.calls {
		if !strings.HasPrefix(c, "start") {
			t.Errorf("a refused write reached the browser: %s", c)
		}
	}
	if len(attention) == 0 || attention[len(attention)-1] != "approval" {
		t.Errorf("approval_required should ask for the viewer, got %v", attention)
	}

	// Reads work without a grant.
	if _, err := svc.Extract(ctx, id, "h1", nil, 0, "g"); err != nil {
		t.Errorf("extract: %v", err)
	}

	// A grant belongs to one registrable domain; the navigate target is another.
	for _, d := range []string{"a.test", "b.test"} {
		if err := svc.ApproveDomain(id, d, 0); err != nil {
			t.Fatal(err)
		}
	}
	for name, w := range writes {
		if err := w(); err != nil {
			t.Errorf("%s with a grant: %v", name, err)
		}
	}
}

func TestEveryActionRefusedWhileHandedOff(t *testing.T) {
	svc, fb, _ := newTestService(t, nil)
	st, _ := svc.Start(ctx, "", "g", "")
	id := st.SessionID
	if err := svc.Approve(id, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Handoff(ctx, id, "log in", "g"); err != nil {
		t.Fatal(err)
	}
	before := len(fb.calls)
	actions := map[string]func() error{
		"navigate":   func() error { _, err := svc.Navigate(ctx, id, "http://b.test/", "g"); return err },
		"click":      func() error { _, err := svc.Click(ctx, id, "#x", nil, nil, "g"); return err },
		"type":       func() error { _, err := svc.Type(ctx, id, "pw", "g"); return err },
		"select":     func() error { _, err := svc.Select(ctx, id, "#s", "v", "", "g"); return err },
		"key":        func() error { _, err := svc.Key(ctx, id, "Tab", "g"); return err },
		"scroll":     func() error { _, err := svc.Scroll(ctx, id, "", nil, nil, 0, 1, "g"); return err },
		"extract":    func() error { _, err := svc.Extract(ctx, id, "input", []string{"value"}, 0, "g"); return err },
		"screenshot": func() error { _, err := svc.Screenshot(ctx, id, "g"); return err },
	}
	for name, a := range actions {
		if err := a(); !errors.Is(err, ErrHumanDriving) {
			t.Errorf("%s while handed off: want human_driving, got %v", name, err)
		}
	}
	if len(fb.calls) != before {
		t.Errorf("an action reached the browser while the user drove: %v", fb.calls[before:])
	}
	views, err := svc.Status(ctx, id)
	if err != nil || len(views) != 1 || views[0].HandoffSince == "" || views[0].HandoffReason != "log in" {
		t.Fatalf("status should show the pending handoff: %+v %v", views, err)
	}
}

func TestApprovalDeniedExpiredRevokedAreDistinct(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	svc, _, _ := newTestService(t, func(cfg *Config) { cfg.Now = c.now; cfg.IdleLifetime = 72 * time.Hour })
	st, _ := svc.Start(ctx, "", "g", "")
	id := st.SessionID
	write := func() error { _, err := svc.Click(ctx, id, "#x", nil, nil, "g"); return err }

	_ = svc.Deny(id)
	if err := write(); !errors.Is(err, ErrApprovalDenied) {
		t.Errorf("denied: %v", err)
	}
	_ = svc.Approve(id, 48*time.Hour) // capped at 24h
	c.add(23 * time.Hour)
	if err := write(); err != nil {
		t.Errorf("inside 24h: %v", err)
	}
	c.add(2 * time.Hour)
	if err := write(); !errors.Is(err, ErrApprovalExpired) {
		t.Errorf("expired: %v", err)
	}
	_ = svc.Approve(id, time.Hour)
	_ = svc.Revoke(id)
	if err := write(); !errors.Is(err, ErrApprovalRevoked) {
		t.Errorf("revoked: %v", err)
	}
}

func TestWaitApprovedAndResumed(t *testing.T) {
	svc, _, _ := newTestService(t, nil)
	st, _ := svc.Start(ctx, "", "g", "")
	id := st.SessionID

	done := make(chan WaitResult, 1)
	go func() {
		r, _ := svc.Wait(ctx, id, "approved", 5*time.Second)
		done <- r
	}()
	time.Sleep(30 * time.Millisecond)
	_ = svc.Approve(id, 0)
	select {
	case r := <-done:
		if !r.Satisfied || r.State != "approved" {
			t.Fatalf("wait approved: %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approve did not wake the waiter")
	}

	_, _ = svc.Handoff(ctx, id, "log in", "g")
	go func() {
		r, _ := svc.Wait(ctx, id, "resumed", 5*time.Second)
		done <- r
	}()
	time.Sleep(30 * time.Millisecond)
	_ = svc.HumanHandBack(id)
	select {
	case r := <-done:
		if !r.Satisfied || r.State != "resumed" {
			t.Fatalf("wait resumed: %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hand back did not wake the waiter")
	}

	r, err := svc.Wait(ctx, id, "resumed", 10*time.Millisecond)
	if err != nil || !r.Satisfied {
		t.Fatalf("already resumed: %+v %v", r, err)
	}
	_ = svc.Revoke(id)
	r, _ = svc.Wait(ctx, id, "approved", 20*time.Millisecond)
	if r.Satisfied || r.State != "revoked" || r.StillWaiting {
		t.Fatalf("revoked is a terminal answer: %+v", r)
	}
	_ = svc.Deny(id)
	r, _ = svc.Wait(ctx, id, "approved", 20*time.Millisecond)
	if r.State != "denied" {
		t.Fatalf("denied: %+v", r)
	}
	svc2, _, _ := newTestService(t, nil)
	st2, _ := svc2.Start(ctx, "", "g", "")
	r, _ = svc2.Wait(ctx, st2.SessionID, "approved", 20*time.Millisecond)
	if !r.StillWaiting || r.Satisfied {
		t.Fatalf("timeout should report still_waiting: %+v", r)
	}
	if _, err := svc2.Wait(ctx, st2.SessionID, "forever", time.Millisecond); err == nil {
		t.Fatal("unknown until must error")
	}
}

func TestLimits(t *testing.T) {
	svc, _, _ := newTestService(t, func(c *Config) {
		c.WritesPerMinute = 3
		c.NavigationsPerHour = 3
		c.ReadsPerMinute = 4
	})
	a, _ := svc.Start(ctx, "", "g", "")
	if _, err := svc.Start(ctx, "", "g", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(ctx, "", "g", ""); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("third session: %v", err)
	}
	_ = svc.Approve(a.SessionID, 0)
	for i := 0; i < 3; i++ {
		if _, err := svc.Key(ctx, a.SessionID, "Tab", "g"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Key(ctx, a.SessionID, "Tab", "g"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("write rate: %v", err)
	}
	// Stopping and restarting does not reset the owner-wide budget.
	_ = svc.Stop(ctx, a.SessionID)
	b, err := svc.Start(ctx, "", "g", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.Approve(b.SessionID, 0)
	if _, err := svc.Key(ctx, b.SessionID, "Tab", "g"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("budget must survive a new session: %v", err)
	}
	// Reads are budgeted too.
	for i := 0; i < 4; i++ {
		if _, err := svc.Extract(ctx, b.SessionID, "h1", nil, 0, "g"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Screenshot(ctx, b.SessionID, "g"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("read rate: %v", err)
	}
}

func TestNavigationBudgetCountsStartsAndLinkClicks(t *testing.T) {
	svc, fb, _ := newTestService(t, func(c *Config) { c.NavigationsPerHour = 2 })
	st, _ := svc.Start(ctx, "http://one.test/", "g", "") // 1
	_ = svc.ApproveDomain(st.SessionID, "one.test", 0)
	_ = svc.ApproveDomain(st.SessionID, "three.test", 0)
	_ = svc.ApproveDomain(st.SessionID, "two.test", 0)
	fb.clickNavigatesTo = "http://two.test/"
	if res, err := svc.Click(ctx, st.SessionID, "a", nil, nil, "g"); err != nil || !res.Navigated {
		t.Fatalf("link click: %+v %v", res, err) // 2
	}
	if _, err := svc.Navigate(ctx, st.SessionID, "http://three.test/", "g"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("a link-click navigation must count: %v", err)
	}
}

func TestStartRedirectToRefusedAddressClosesSession(t *testing.T) {
	svc, fb, _ := newTestService(t, func(c *Config) {
		c.URLPolicy = NewURLPolicy(fixedResolver(map[string][]string{"public.test": {"203.0.113.10"}}), nil)
	})
	fb.startLandsOn = "http://169.254.169.254/latest/"
	if _, err := svc.Start(ctx, "http://public.test/redirect", "g", ""); !errors.Is(err, ErrNavigationRefused) {
		t.Fatalf("start redirect: %v", err)
	}
	if len(fb.sessions) != 0 || len(svc.Sessions("")) != 0 {
		t.Fatal("the session must be closed")
	}
}

func TestReadRefusedWhenPageMovedToRefusedAddress(t *testing.T) {
	svc, fb, _ := newTestService(t, func(c *Config) {
		c.URLPolicy = NewURLPolicy(fixedResolver(map[string][]string{"public.test": {"203.0.113.10"}}), nil)
	})
	st, err := svc.Start(ctx, "http://public.test/", "g", "")
	if err != nil {
		t.Fatal(err)
	}
	fb.mu.Lock()
	fb.sessions[st.SessionID].url = "http://192.168.1.1/" // a script moved it
	fb.mu.Unlock()
	if _, err := svc.Extract(ctx, st.SessionID, "body", nil, 0, "g"); !errors.Is(err, ErrNavigationRefused) {
		t.Fatalf("read on a refused page: %v", err)
	}
	if fb.sessions[st.SessionID].url != "about:blank" {
		t.Fatal("the refused page should be closed")
	}
}

func TestAgentCannotResumeWhileUserDrives(t *testing.T) {
	svc, _, _ := newTestService(t, nil)
	st, _ := svc.Start(ctx, "", "g", "")
	_, _ = svc.Handoff(ctx, st.SessionID, "log in", "g")
	if _, err := svc.Resume(ctx, st.SessionID); !errors.Is(err, ErrOnlyUserHandsBack) {
		t.Fatalf("agent resume while handed off: %v", err)
	}
	if v := svc.Sessions(st.SessionID); v[0].Driver != "human" {
		t.Fatal("the user must keep control")
	}
	_ = svc.HumanHandBack(st.SessionID)
	if _, err := svc.Resume(ctx, st.SessionID); err != nil {
		t.Fatalf("resume once back with the agent: %v", err)
	}
}

func TestStatusRedactsURLWhileUserDrives(t *testing.T) {
	svc, fb, _ := newTestService(t, nil)
	st, _ := svc.Start(ctx, "", "g", "")
	_, _ = svc.Handoff(ctx, st.SessionID, "log in", "g")
	fb.mu.Lock()
	fb.sessions[st.SessionID].url = "https://login.test/callback?code=SECRET123"
	fb.mu.Unlock()
	views, _ := svc.Status(ctx, st.SessionID)
	if views[0].URL != "https://login.test/" {
		t.Fatalf("agent status must show only the origin: %q", views[0].URL)
	}
	if v := svc.Sessions(st.SessionID); v[0].URL != "https://login.test/callback?code=SECRET123" {
		t.Fatalf("the viewer keeps the full URL: %q", v[0].URL)
	}
}

func TestIdleSessionsAreStopped(t *testing.T) {
	c := &clock{t: time.Now()}
	svc, fb, _ := newTestService(t, func(cfg *Config) { cfg.Now = c.now })
	st, _ := svc.Start(ctx, "", "g", "")
	c.add(DefaultIdleLifetime + time.Minute)
	svc.ReapIdle()
	if _, ok := fb.SessionStatus(st.SessionID); ok {
		t.Fatal("idle session still running")
	}
	if _, err := svc.Status(ctx, st.SessionID); !errors.Is(err, ErrNoSession) {
		t.Fatalf("status after idle stop: %v", err)
	}
}

func TestNavigationFilterAppliesToStartAndNavigate(t *testing.T) {
	svc, fb, _ := newTestService(t, func(c *Config) {
		c.URLPolicy = NewURLPolicy(fixedResolver(nil), nil)
	})
	if _, err := svc.Start(ctx, "http://127.0.0.1:9222/json", "g", ""); !errors.Is(err, ErrNavigationRefused) {
		t.Fatalf("start to loopback: %v", err)
	}
	st, err := svc.Start(ctx, "", "g", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.Approve(st.SessionID, 0)
	if _, err := svc.Navigate(ctx, st.SessionID, "http://192.168.1.1/", "g"); !errors.Is(err, ErrNavigationRefused) {
		t.Fatalf("navigate private: %v", err)
	}
	for _, c := range fb.calls {
		if strings.Contains(c, "127.0.0.1") || strings.Contains(c, "192.168") {
			t.Errorf("refused URL reached the browser: %s", c)
		}
	}
}

func TestOwnerRefusalBlocksCobrowse(t *testing.T) {
	oe := &OwnerError{Known: true, Info: OwnerInfo{PID: 4242, Client: "Other Agent", Since: "x"}}
	svc, fb, events := newTestService(t, func(c *Config) { c.AcquireOwner = func() error { return oe } })
	_, err := svc.Start(ctx, "", "g", "")
	var got *OwnerError
	if !errors.As(err, &got) || !strings.Contains(err.Error(), "Other Agent") {
		t.Fatalf("want the owner error naming the other client, got %v", err)
	}
	if len(fb.calls) != 0 {
		t.Fatal("browser started without the owner lock")
	}
	if len(*events) != 1 || (*events)[0].Outcome != "refused" {
		t.Fatalf("refusal should be recorded: %+v", *events)
	}
}

func TestEventsRecordTextLengthOnly(t *testing.T) {
	svc, _, events := newTestService(t, nil)
	st, _ := svc.Start(ctx, "", "g", "")
	_ = svc.Approve(st.SessionID, 0)
	const secret = "correct horse battery staple"
	if _, err := svc.Type(ctx, st.SessionID, secret, "fill"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range *events {
		if strings.Contains(fmt.Sprintf("%+v", e), secret) {
			t.Fatalf("typed text leaked into an event: %+v", e)
		}
		if e.Action == "type" {
			found = e.TextLen == len(secret)
		}
	}
	if !found {
		t.Fatal("type event with text length missing")
	}
}

func TestProfileRefusedWithoutStore(t *testing.T) {
	svc, fb, _ := newTestService(t, nil)
	if _, err := svc.Start(ctx, "", "g", "work"); !errors.Is(err, ErrProfilesUnavailable) {
		t.Fatalf("profile: %v", err)
	}
	if len(fb.calls) != 0 {
		t.Fatalf("no browser may start: %v", fb.calls)
	}
}

func TestHumanNavigateOnlyWhileDriving(t *testing.T) {
	svc, fb, _ := newTestService(t, nil)
	st, _ := svc.Start(ctx, "", "g", "")
	if err := svc.HumanNavigate(ctx, st.SessionID, "http://a.test/"); err == nil {
		t.Fatal("address bar must need take control")
	}
	_ = svc.HumanTakeControl(st.SessionID)
	if err := svc.HumanNavigate(ctx, st.SessionID, "http://a.test/"); err != nil {
		t.Fatal(err)
	}
	if v := svc.Sessions(st.SessionID); len(v) != 1 || v[0].Driver != "human" {
		t.Fatalf("user keeps control after navigating: %+v", v)
	}
	if fb.calls[len(fb.calls)-1] != "human-navigate http://a.test/" {
		t.Fatalf("calls: %v", fb.calls)
	}
}

func TestNavigateReturnsFinalURLAndTitleAfterRedirect(t *testing.T) {
	svc, fb, _ := newTestService(t, nil)
	st, _ := svc.Start(ctx, "", "g", "")
	_ = svc.ApproveDomain(st.SessionID, "one.test", 0)
	_ = svc.ApproveDomain(st.SessionID, "two.test", 0)
	fb.navigateLandsOn = "http://two.test/final"
	fb.pageTitle = "Final\x00 Page\n"
	res, err := svc.Navigate(ctx, st.SessionID, "http://one.test/start", "g")
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "http://two.test/final" || res.Title != "Final Page" || !res.Settled {
		t.Fatalf("want the post-redirect url and sanitized title, got %+v", res)
	}
}

func TestNavigateSlowPageReturnsCurrentURLWithinWindow(t *testing.T) {
	old := settleWindow
	settleWindow = 250 * time.Millisecond
	t.Cleanup(func() { settleWindow = old })
	svc, fb, _ := newTestService(t, nil)
	st, _ := svc.Start(ctx, "", "g", "")
	_ = svc.ApproveDomain(st.SessionID, "slow.test", 0)
	fb.notReady = true
	start := time.Now()
	res, err := svc.Navigate(ctx, st.SessionID, "http://slow.test/", "g")
	if err != nil {
		t.Fatalf("a slow page is not a failure: %v", err)
	}
	if time.Since(start) > 3*time.Second || res.Settled || res.URL != "http://slow.test/" {
		t.Fatalf("want the current url, unsettled, within the window: %+v after %v", res, time.Since(start))
	}
}

func TestNavigateTitleBounded(t *testing.T) {
	svc, fb, _ := newTestService(t, nil)
	st, _ := svc.Start(ctx, "", "g", "")
	_ = svc.ApproveDomain(st.SessionID, "one.test", 0)
	fb.pageTitle = strings.Repeat("t", 5000)
	res, err := svc.Navigate(ctx, st.SessionID, "http://one.test/", "g")
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(res.Title)); n != platform.MaxCobrowseTitleRunes {
		t.Fatalf("title length %d, want %d", n, platform.MaxCobrowseTitleRunes)
	}
}

func TestStartWithURLReportsSettledTitle(t *testing.T) {
	svc, fb, _ := newTestService(t, nil)
	fb.pageTitle = "Example\x00 Domain\n"
	res, err := svc.Start(ctx, "http://one.test/", "g", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "Example Domain" {
		t.Fatalf("title = %q", res.Title)
	}
	blank, err := svc.Start(ctx, "", "g", "")
	if err != nil {
		t.Fatal(err)
	}
	if blank.Title != "" {
		t.Fatalf("blank start title = %q, want empty", blank.Title)
	}
}
