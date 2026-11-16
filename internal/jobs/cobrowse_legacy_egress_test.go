package jobs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

type fakeLegacyCobrowse struct {
	state              platform.CobrowseStatus
	statusCalls, stops int
	stopErr            error
}

func (f *fakeLegacyCobrowse) Status() platform.CobrowseStatus { f.statusCalls++; return f.state }
func (f *fakeLegacyCobrowse) Stop() error {
	f.stops++
	if f.stopErr != nil {
		return f.stopErr
	}
	f.state.Running = false
	f.state.Driver = platform.DriverAI
	return nil
}

func TestLegacyCobrowseRefusesBeforeManagerDiscovery(t *testing.T) {
	for _, action := range []string{"start", "navigate", "handoff", "resume", "screenshot", "click", "type", "key", "scroll", "extract", "future_action"} {
		t.Run(action, func(t *testing.T) {
			h := &CobrowseHandler{manager: func() legacyCobrowseControl { t.Fatal("legacy refusal discovered a manager"); return nil }}
			out, err := h.Execute(JobContext{}, &nexus.Job{ID: "fixture", Type: "COBROWSE", Payload: map[string]string{"action": action, "profile": "must-not-be-created", "url": "https://public.test/"}})
			if !errors.Is(err, ErrLegacyCobrowseDisabled) || len(out) != 0 {
				t.Fatalf("legacy action result=%q err=%v", out, err)
			}
			for _, guidance := range []string{"COBROWSE_SESSION", "cobrowse_start", "session_id"} {
				if !strings.Contains(err.Error(), guidance) {
					t.Fatalf("missing restart guidance %s: %v", guidance, err)
				}
			}
		})
	}
}

func TestLegacyCobrowseStatusAndStopRemainAvailable(t *testing.T) {
	f := &fakeLegacyCobrowse{state: platform.CobrowseStatus{Running: true, Driver: platform.DriverHuman, URL: "https://fixture.test/"}}
	h := &CobrowseHandler{manager: func() legacyCobrowseControl { return f }}
	for _, action := range []string{CobrowseActionStatus, CobrowseActionStop, CobrowseActionStop} {
		out, err := h.Execute(JobContext{}, &nexus.Job{ID: "fixture", Payload: map[string]string{"action": action}})
		if err != nil {
			t.Fatal(err)
		}
		var st map[string]any
		if err = json.Unmarshal(out, &st); err != nil {
			t.Fatal(err)
		}
		if action == CobrowseActionStatus {
			if st["running"] != true || st["driver"] != string(platform.DriverHuman) || f.stops != 0 {
				t.Fatalf("status changed legacy session: %v", st)
			}
		} else if st["running"] != false {
			t.Fatalf("stop still reports running: %v", st)
		}
	}
	if f.stops != 2 || f.statusCalls != 3 {
		t.Fatalf("cleanup counts stops=%d status=%d", f.stops, f.statusCalls)
	}
	f.stopErr = errors.New("fixture stop uncertain")
	out, err := h.Execute(JobContext{}, &nexus.Job{ID: "fixture", Payload: map[string]string{"action": CobrowseActionStop}})
	if !errors.Is(err, f.stopErr) || len(out) != 0 || f.statusCalls != 3 {
		t.Fatalf("uncertain stop claimed success: %s %v", out, err)
	}
}
