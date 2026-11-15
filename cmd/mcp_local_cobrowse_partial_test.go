package cmd

import (
	"errors"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestPartialActionResultSurfacesDoneFalseAsResult(t *testing.T) {
	done := false
	boom := errors.New("did not finish")
	got, err := partialActionResult(localcobrowse.ActionResult{Dispatched: true, Done: &done}, boom)
	if err != nil || got.(localcobrowse.ActionResult).Done == nil {
		t.Errorf("partial result not surfaced: %v %v", got, err)
	}
	if _, err := partialActionResult(localcobrowse.ActionResult{}, boom); err != boom {
		t.Errorf("plain error must pass through, got %v", err)
	}
}

func TestPartialActionResultKeepsRefusalsAsErrors(t *testing.T) {
	done := false
	for _, refusal := range []error{platform.ErrTargetChanged, platform.ErrPageRefused} {
		if got, err := partialActionResult(localcobrowse.ActionResult{Done: &done}, refusal); err == nil || got != nil {
			t.Errorf("%v must stay an error, got %v %v", refusal, got, err)
		}
	}
}
