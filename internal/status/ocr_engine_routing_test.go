package status

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestDiscoverModelsHandlesEveryProbeEngine guards the gap that kept
// baidu/Unlimited-OCR off the heartbeat: an engine in managedProbeEngines whose
// serviceType has no DiscoverModels case falls through to the "unsupported
// service type" default, which makes collectManagedEngineStatus drop it (it is
// only reported when a probe responds). A new managed engine MUST add a
// DiscoverModels case; this test fails loudly if it doesn't.
func TestDiscoverModelsHandlesEveryProbeEngine(t *testing.T) {
	d := NewModelDiscovery()
	for _, engine := range managedProbeEngines {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		// Probe a port with nothing listening: a HANDLED engine returns a
		// connection error (or empty list); an UNHANDLED one returns the
		// "unsupported service type" sentinel from the switch default.
		_, err := d.DiscoverModels(ctx, engine, 1)
		cancel()
		if err != nil && strings.Contains(err.Error(), "unsupported service type") {
			t.Errorf("DiscoverModels has no case for managedProbeEngine %q "+
				"(engine would be dropped from the heartbeat / not routable)", engine)
		}
	}
}
