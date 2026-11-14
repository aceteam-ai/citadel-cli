package localcobrowse

import (
	"errors"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestPartialActionResult(t *testing.T) {
	ae := &platform.ActionIncompleteError{Action: "type", TimedOut: true,
		Effects: platform.ActionEffects{Dispatched: true, TypedLength: 4, TypedUnconfirmed: true},
		Status:  platform.CobrowseSessionStatus{URL: "http://x.test/"}}
	res, ok := partialActionResult(ae)
	if !ok || res.Done == nil || *res.Done || !res.Dispatched || res.Effects == nil || res.Effects.TypedLength != 4 || res.URL != "http://x.test/" {
		t.Errorf("%+v %v", res, ok)
	}
	if _, ok := partialActionResult(errors.New("boom")); ok {
		t.Error("plain error treated as partial")
	}
	if _, ok := partialActionResult(&platform.ActionIncompleteError{Action: "click"}); ok {
		t.Error("effect-free failure treated as partial")
	}
}
