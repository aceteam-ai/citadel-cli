package jobs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func partialErr() *platform.ActionIncompleteError {
	done := false
	eff := platform.ActionEffects{Dispatched: true, Scrolled: true, ScrollY: 480, Focused: true, FocusTag: "textarea", TypedLength: 5, TypedUnconfirmed: true}
	return &platform.ActionIncompleteError{
		Action: "click", TimedOut: true, Effects: eff,
		Status: platform.CobrowseSessionStatus{ID: "cb-1", URL: "http://x.test/", Done: &done, Effects: &eff},
	}
}

// Platform tools treat any non-error result as success, so without the opt-in
// flag an incomplete action is an ERROR whose text says what happened.
func TestActionFailureWithoutFlagIsAnErrorWithSummary(t *testing.T) {
	ae := partialErr()
	out, err := actionFailureResult(ae, false)
	if out != nil || err == nil {
		t.Fatalf("want an error and no result, got %s %v", out, err)
	}
	for _, want := range []string{"scrolled to x=0 y=480", "focused a textarea", "typed 5 characters (unconfirmed)", "Not retried", "fresh screenshot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestActionFailureWithReportPartialReturnsStructuredResult(t *testing.T) {
	out, err := actionFailureResult(partialErr(), true)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["done"] != false || m["timed_out"] != true || m["id"] != "cb-1" || m["url"] != "http://x.test/" || m["stream"] == nil {
		t.Errorf("result %v", m)
	}
	e, _ := m["effects"].(map[string]any)
	if e["scrolled"] != true || e["scroll_y"] != 480.0 || e["focused"] != true || e["clicked"] != false {
		t.Errorf("effects %v", e)
	}
}

func TestActionFailureKeepsNonReportableErrorsUnchanged(t *testing.T) {
	plain := errors.New("no element matches")
	if out, err := actionFailureResult(plain, true); out != nil || err != plain {
		t.Errorf("plain error changed: %s %v", out, err)
	}
	for _, r := range []error{platform.ErrHandedOff, platform.ErrNotStarted, platform.ErrTargetChanged, platform.ErrPageRefused} {
		if out, err := actionFailureResult(r, true); out != nil || err != r {
			t.Errorf("refusal %v changed: %s %v", r, out, err)
		}
	}
	// Input may have been sent but nothing observed and no timeout: not reportable.
	ae := &platform.ActionIncompleteError{Action: "key", Effects: platform.ActionEffects{Dispatched: true}, Cause: plain}
	if out, err := actionFailureResult(ae, true); out != nil || err != ae {
		t.Errorf("unreportable wrapper changed: %s %v", out, err)
	}
}
