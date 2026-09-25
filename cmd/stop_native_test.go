// cmd/stop_native_test.go
package cmd

import (
	"errors"
	"strings"
	"testing"

	internalServices "github.com/aceteam-ai/citadel-cli/internal/services"
)

// TestNativeStopOutcome pins the #1144 CLI messaging for `citadel stop <native>`:
// an externally-managed host engine yields guidance (not a failure, not the old
// false "✅ stopped"), a real stop yields success, any other error is a failure.
// Pure and seam-free -- no live process, manifest, or /proc read, so it never
// touches this dev box's real node.
func TestNativeStopOutcome(t *testing.T) {
	t.Run("externally managed -> guidance, not a stop, not a failure", func(t *testing.T) {
		line, stopped, failed := nativeStopOutcome("ollama", &internalServices.ErrNativeExternallyManaged{Unit: "ollama.service"})
		if stopped || failed {
			t.Errorf("stopped=%v failed=%v; want both false", stopped, failed)
		}
		if !strings.Contains(line, "sudo systemctl stop ollama") ||
			!strings.Contains(line, "host systemd unit ollama.service") {
			t.Errorf("line missing guidance: %q", line)
		}
		if strings.Contains(line, "✅") {
			t.Errorf("externally-managed engine must not report a false success: %q", line)
		}
	})

	t.Run("nil error -> real stop", func(t *testing.T) {
		line, stopped, failed := nativeStopOutcome("ollama", nil)
		if !stopped || failed {
			t.Errorf("stopped=%v failed=%v; want stopped=true failed=false", stopped, failed)
		}
		if !strings.Contains(line, "stopped") {
			t.Errorf("line = %q, want a success message", line)
		}
	})

	t.Run("other error -> failure", func(t *testing.T) {
		line, stopped, failed := nativeStopOutcome("ollama", errors.New("boom"))
		if stopped || !failed {
			t.Errorf("stopped=%v failed=%v; want failed=true", stopped, failed)
		}
		if !strings.Contains(line, "Failed to stop") {
			t.Errorf("line = %q, want a failure message", line)
		}
	})
}
