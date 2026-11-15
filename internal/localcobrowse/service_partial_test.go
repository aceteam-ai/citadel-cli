package localcobrowse

import (
	"errors"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func incomplete(timedOut bool, eff platform.ActionEffects) *platform.ActionIncompleteError {
	return &platform.ActionIncompleteError{Action: "click", TimedOut: timedOut, Effects: eff,
		Status: platform.CobrowseSessionStatus{URL: "http://x.test/"}, Cause: errors.New("CDP read: context deadline exceeded")}
}

func TestPartialActionResult(t *testing.T) {
	res, ok := partialActionResult(incomplete(true, platform.ActionEffects{Dispatched: true, TypedLength: 4, TypedUnconfirmed: true}))
	if !ok || res.Done == nil || *res.Done || !res.Dispatched || res.Effects == nil || res.Effects.TypedLength != 4 || res.URL != "http://x.test/" || res.Error == "" {
		t.Errorf("%+v %v", res, ok)
	}
	if _, ok := partialActionResult(errors.New("boom")); ok {
		t.Error("plain error treated as partial")
	}
	// Once input may have been dispatched, any error is reportable.
	if res, ok := partialActionResult(incomplete(false, platform.ActionEffects{Dispatched: true, Unknown: true})); !ok || !res.Dispatched {
		t.Error("a dispatched action with a non-timeout error was not reported")
	}
	// A refusal before dispatch stays plain.
	if _, ok := partialActionResult(incomplete(false, platform.ActionEffects{})); ok {
		t.Error("an undispatched failure treated as partial")
	}
}

func TestPartialNeverWrapsRefusals(t *testing.T) {
	for _, r := range []error{platform.ErrHandedOff, platform.ErrNotStarted, platform.ErrTargetChanged, platform.ErrPageRefused, ErrHumanDriving} {
		ae := incomplete(true, platform.ActionEffects{Dispatched: true, Scrolled: true})
		ae.Cause = r
		if _, ok := partialActionResult(ae); ok {
			t.Errorf("%v wrapped as a partial result", r)
		}
	}
}

// A click that navigated to an address the policy refuses and then timed out
// must still be sent to about:blank, exactly as on the normal path.
func TestPartialClickLandingOnRefusedPageIsEnforced(t *testing.T) {
	svc, fb, _ := newTestService(t, func(c *Config) {
		c.URLPolicy = NewURLPolicy(fixedResolver(map[string][]string{"public.test": {"203.0.113.10"}}), nil)
	})
	st, err := svc.Start(ctx, "http://public.test/", "g", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.ApproveDomain(st.SessionID, "public.test", 0)
	fb.clickNavigatesTo = "http://192.168.1.1/"
	fb.clickErr = incomplete(true, platform.ActionEffects{Dispatched: true, Clicked: true})
	res, err := svc.Click(ctx, st.SessionID, "a", nil, nil, "g")
	if err == nil || res.Done == nil || *res.Done {
		t.Fatalf("want partial result and error, got %+v %v", res, err)
	}
	if !errors.Is(err, ErrNavigationRefused) {
		t.Errorf("landing refusal not reported: %v", err)
	}
	if fb.sessions[st.SessionID].url != "about:blank" {
		t.Errorf("refused landing was not sent to about:blank: %q", fb.sessions[st.SessionID].url)
	}
}
