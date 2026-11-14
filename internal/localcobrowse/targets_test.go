package localcobrowse

import (
	"context"
	"fmt"
	"strings"
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
	svc.RecordTargetClosed("cb-1", "http://private.test/", fmt.Errorf("%w: private address", ErrNavigationRefused))
	if len(*events) != 1 {
		t.Fatalf("events = %+v", *events)
	}
	e := (*events)[0]
	if e.Action != "target_closed" || e.Outcome != "refused" || e.SessionID != "cb-1" || e.URL == "" {
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
