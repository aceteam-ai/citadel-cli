package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// fakeSink records every StreamAdd call so tests can assert emit/no-emit and
// inspect payloads for PII.
type fakeSink struct {
	calls []map[string]string
}

func (f *fakeSink) StreamAdd(_ context.Context, _ string, values map[string]string, _ int64) error {
	f.calls = append(f.calls, values)
	return nil
}

func newTestEmitter(t *testing.T, sink eventSink) (*emitter, string) {
	t.Helper()
	dir := t.TempDir()
	return &emitter{
		sink:            sink,
		configDir:       dir,
		nodeID:          "node-1",
		headscaleNodeID: "758",
		orgID:           "org-abc",
		version:         "v2.44.0",
	}, dir
}

func writeFlag(t *testing.T, dir string, enabled bool) {
	t.Helper()
	if err := config.SaveTelemetry(dir, &config.Telemetry{AnonTelemetryEnabled: enabled}); err != nil {
		t.Fatalf("SaveTelemetry: %v", err)
	}
}

// Gating: flag off => no emission.
func TestEmit_FlagOff_NoEmit(t *testing.T) {
	sink := &fakeSink{}
	e, dir := newTestEmitter(t, sink)
	writeFlag(t, dir, false)

	e.emit("info", "node connected")

	if len(sink.calls) != 0 {
		t.Fatalf("flag off should suppress emission, got %d calls", len(sink.calls))
	}
}

// Gating: flag on => exactly one emission.
func TestEmit_FlagOn_Emits(t *testing.T) {
	sink := &fakeSink{}
	e, dir := newTestEmitter(t, sink)
	writeFlag(t, dir, true)

	e.emit("info", "node connected")

	if len(sink.calls) != 1 {
		t.Fatalf("flag on should emit once, got %d calls", len(sink.calls))
	}
}

// Default (no file written) => enabled (opt-out), so emission happens.
func TestEmit_DefaultEnabled_Emits(t *testing.T) {
	sink := &fakeSink{}
	e, _ := newTestEmitter(t, sink)

	e.emit("info", "startup")

	if len(sink.calls) != 1 {
		t.Fatalf("default should be enabled and emit once, got %d calls", len(sink.calls))
	}
}

// Anonymization: payload carries node/debug context and NO user PII.
func TestBuildPayload_NoPII(t *testing.T) {
	e := &emitter{
		nodeID:          "node-1",
		headscaleNodeID: "758",
		orgID:           "org-abc",
		version:         "v2.44.0",
	}
	payload := e.buildPayload("warning", "VPN reconnect failed")

	// Required node/debug context is present.
	for _, key := range []string{"nodeId", "headscaleNodeId", "orgId", "version", "timestamp", "level", "message"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("payload missing expected field %q", key)
		}
	}

	// No user-identity field is present anywhere in the payload keys.
	for key := range payload {
		lower := strings.ToLower(key)
		for _, banned := range []string{"email", "username", "user_name", "password", "token", "secret"} {
			if strings.Contains(lower, banned) {
				t.Errorf("payload contains PII-like field %q", key)
			}
		}
	}

	if payload["nodeId"] != "node-1" || payload["orgId"] != "org-abc" {
		t.Errorf("unexpected node/debug context: %+v", payload)
	}
}

// Emit (the public goroutine wrapper) is a no-op when unconfigured and never
// panics.
func TestEmit_Unconfigured_NoPanic(t *testing.T) {
	Reset()
	Emit("info", "no emitter configured") // must not panic
}

// Configure(nil, ...) clears the emitter.
func TestConfigure_NilSink_Clears(t *testing.T) {
	sink := &fakeSink{}
	Configure(sink, t.TempDir(), "n", "h", "o", "v")
	Configure(nil, "", "", "", "", "")

	mu.RLock()
	defer mu.RUnlock()
	if current != nil {
		t.Error("Configure(nil) should clear the emitter")
	}
}

// Sanity: the on-disk flag file name matches what the settings pane (#295) will
// toggle.
func TestFlagFileName(t *testing.T) {
	dir := t.TempDir()
	writeFlag(t, dir, false)
	if _, err := os.Stat(filepath.Join(dir, "telemetry.yaml")); err != nil {
		t.Errorf("expected telemetry.yaml flag file: %v", err)
	}
}
