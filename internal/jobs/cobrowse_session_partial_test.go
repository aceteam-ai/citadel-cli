package jobs

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestActionFailureResultReportsPartialEffects(t *testing.T) {
	done := false
	eff := platform.ActionEffects{Dispatched: true, Scrolled: true, ScrollY: 480, Focused: true, FocusTag: "textarea"}
	ae := &platform.ActionIncompleteError{
		Action: "click", TimedOut: true, Effects: eff,
		Status: platform.CobrowseSessionStatus{ID: "cb-1", URL: "http://x.test/", Done: &done, Effects: &eff},
	}
	out, err := actionFailureResult(ae)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["done"] != false || m["timed_out"] != true || m["id"] != "cb-1" || m["url"] != "http://x.test/" {
		t.Errorf("result %v", m)
	}
	e, _ := m["effects"].(map[string]any)
	if e["scrolled"] != true || e["scroll_y"] != 480.0 || e["focused"] != true || e["clicked"] != false {
		t.Errorf("effects %v", e)
	}
	if m["stream"] == nil {
		t.Error("existing stream field missing")
	}
}

func TestActionFailureResultKeepsPlainErrors(t *testing.T) {
	plain := errors.New("no element matches")
	if out, err := actionFailureResult(plain); out != nil || err != plain {
		t.Errorf("plain error changed: %s %v", out, err)
	}
	noEffect := &platform.ActionIncompleteError{Action: "click", Cause: plain}
	if out, err := actionFailureResult(noEffect); out != nil || err == nil {
		t.Errorf("effect-free failure must stay an error: %s %v", out, err)
	}
}
