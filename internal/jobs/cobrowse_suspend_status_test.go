package jobs

import (
	"encoding/json"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestCobrowseDormantStatusHasNoStreamEndpoint(t *testing.T) {
	for _, state := range []platform.CobrowseSessionState{platform.SessionSuspending, platform.SessionSuspended,
		platform.SessionResuming, platform.SessionExpired, platform.SessionRunning, platform.SessionAttached} {
		t.Run(string(state), func(t *testing.T) {
			data, err := sessionStatusResult(platform.CobrowseSessionStatus{ID: "cb-aa", State: state, Generation: 8,
				ExpiresAt: "2026-11-21T00:00:00Z", ProfileKind: "throwaway"})
			var got map[string]any
			if err != nil || json.Unmarshal(data, &got) != nil {
				t.Fatal("status encoding failed", err)
			}
			_, stream := got["stream"]
			live := state == platform.SessionRunning || state == platform.SessionAttached
			if stream != live || got["generation"] != float64(8) || got["profile_kind"] != "throwaway" {
				t.Fatal("dormant status advertised a stream or lost metadata", string(data))
			}
		})
	}
}
