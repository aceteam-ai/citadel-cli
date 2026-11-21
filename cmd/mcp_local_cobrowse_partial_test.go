package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// A partly done write is a tool ERROR (MCP isError) that carries the
// structured effects, never a plain success.
func TestPartialActionResultIsAnErrorCarryingEffects(t *testing.T) {
	done := false
	boom := errors.New("click hit its time limit")
	got, err := partialActionResult(localcobrowse.ActionResult{Dispatched: true, Done: &done,
		Effects: &platform.ActionEffects{Scrolled: true, ScrollY: 480}}, boom)
	if got != nil || err == nil {
		t.Fatalf("want error only, got %v %v", got, err)
	}
	for _, want := range []string{"click hit its time limit", `"done":false`, `"scroll_y":480`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if !errors.Is(err, boom) {
		t.Error("the original error is not wrapped")
	}
}

func TestPartialActionResultKeepsRefusalsAndPlainErrors(t *testing.T) {
	done := false
	for _, refusal := range []error{platform.ErrTargetChanged, platform.ErrPageRefused} {
		if got, err := partialActionResult(localcobrowse.ActionResult{Done: &done}, refusal); err != refusal || got != nil {
			t.Errorf("%v must stay itself, got %v %v", refusal, got, err)
		}
	}
	boom := errors.New("boom")
	if _, err := partialActionResult(localcobrowse.ActionResult{}, boom); err != boom {
		t.Errorf("plain error changed: %v", err)
	}
}
