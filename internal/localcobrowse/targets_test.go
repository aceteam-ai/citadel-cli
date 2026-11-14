package localcobrowse

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestStatusReportsOpenTargetsAndRedactsWhileHumanDrives(t *testing.T) {
	svc, fb, _ := newTestService(t, nil)
	res, err := svc.Start(ctx, "https://app.test/", "sign in", "")
	if err != nil {
		t.Fatal(err)
	}
	id := res.SessionID
	fb.mu.Lock()
	fb.sessions[id].targets = []platform.CobrowseTarget{
		{ID: "a", Title: "App", URL: "https://app.test/home"},
		{ID: "p", Title: "Sign in", URL: "https://login.test/auth?code=SECRET", Active: true},
	}
	fb.mu.Unlock()

	views, err := svc.Status(ctx, id)
	if err != nil || len(views) != 1 {
		t.Fatalf("status: %v %v", views, err)
	}
	v := views[0]
	if v.ActiveTarget != "p" || len(v.Targets) != 2 || !v.Targets[1].Active || !strings.Contains(v.Targets[1].URL, "code=SECRET") {
		t.Fatalf("view = %+v", v)
	}

	fb.mu.Lock()
	fb.sessions[id].human = true
	fb.mu.Unlock()
	views, _ = svc.Status(ctx, id)
	for _, tg := range views[0].Targets {
		if strings.Contains(tg.URL, "SECRET") || strings.Contains(tg.URL, "?") || tg.Title != "" {
			t.Fatalf("target leaks while the user drives: %+v", tg)
		}
	}
}

func TestRecordTargetClosedIsAudited(t *testing.T) {
	svc, _, events := newTestService(t, nil)
	svc.RecordTargetEvent("cb-1", "target_closed", "http://private.test/", fmt.Errorf("%w: private address", ErrNavigationRefused))
	if len(*events) != 1 {
		t.Fatalf("events = %+v", *events)
	}
	e := (*events)[0]
	if e.Action != "target_closed" || e.Class != "B" || e.SessionID != "cb-1" || e.URL == "" {
		t.Fatalf("event = %+v", e)
	}
}

func TestTargetPolicyIsTheNavigationPolicy(t *testing.T) {
	want := fmt.Errorf("%w: only http and https", ErrNavigationRefused)
	svc, _, _ := newTestService(t, func(c *Config) {
		c.URLPolicy = func(_ context.Context, raw string) error {
			if strings.HasPrefix(raw, "file:") {
				return want
			}
			return nil
		}
	})
	if err := svc.TargetPolicy()(ctx, "file:///etc/hosts"); err != want {
		t.Fatalf("policy = %v", err)
	}
	if err := svc.TargetPolicy()(ctx, "https://login.test/"); err != nil {
		t.Fatal(err)
	}
}

// expectingBrowser records ExpectTarget calls and the order of browser calls.
type expectingBrowser struct {
	*fakeBrowser
	mu       sync.Mutex
	expected string
	log      []string
}

func (e *expectingBrowser) ExpectTarget(id, target string) {
	e.mu.Lock()
	e.expected = target
	e.log = append(e.log, "expect:"+target)
	e.mu.Unlock()
}

func (e *expectingBrowser) Click(id, selector string, x, y *float64) (platform.CobrowseSessionStatus, error) {
	e.mu.Lock()
	e.log = append(e.log, "click@"+e.expected)
	e.mu.Unlock()
	return e.fakeBrowser.Click(id, selector, x, y)
}

// The page that passed the gates is the page the browser is told to act on, and
// the expectation is lifted once the input is dispatched.
func TestWriteIsPinnedToTheCheckedPage(t *testing.T) {
	var eb *expectingBrowser
	svc, fb, _ := newTestService(t, func(c *Config) {
		eb = &expectingBrowser{fakeBrowser: c.Browser.(*fakeBrowser)}
		c.Browser = eb
	})
	res, err := svc.Start(ctx, "https://app.test/", "g", "")
	if err != nil {
		t.Fatal(err)
	}
	id := res.SessionID
	fb.mu.Lock()
	fb.sessions[id].targets = []platform.CobrowseTarget{{ID: "page-1", URL: "https://app.test/", Active: true}}
	fb.mu.Unlock()
	if err := svc.Approve(id, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Click(ctx, id, "#go", nil, nil, "g"); err != nil {
		t.Fatal(err)
	}
	eb.mu.Lock()
	defer eb.mu.Unlock()
	want := []string{"expect:page-1", "click@page-1", "expect:"}
	if len(eb.log) < 3 || eb.log[0] != want[0] || eb.log[1] != want[1] || eb.log[2] != want[2] {
		t.Fatalf("log = %v", eb.log)
	}
}
