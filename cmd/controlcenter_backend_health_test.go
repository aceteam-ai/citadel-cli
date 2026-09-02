package cmd

import "testing"

// TestControlCenterAPIPublisherConfigSetsMarkerDir pins the citadel#429 Part 1
// fix directly: before this fix, the control center's APIPublisherConfig
// construction omitted MarkerDir entirely, so the control center never wrote
// the cross-process heartbeat marker (internal/heartbeat/marker.go) when it
// was itself a node's heartbeat publisher (i.e. no dedicated `citadel work`
// held the worker lock) -- both `citadel status` and the TUI's own Backend:
// row would then read "unknown" forever. This test does not spin up a real
// control center (no TTY, no network, no Redis); it only exercises the pure
// config-construction seam controlCenterAPIPublisherConfig factors out.
func TestControlCenterAPIPublisherConfigSetsMarkerDir(t *testing.T) {
	var loggedLevel, loggedMsg string
	logFn := func(level, msg string) {
		loggedLevel, loggedMsg = level, msg
	}

	cfg := controlCenterAPIPublisherConfig(nil, "test-node", "758", "org-123", logFn)

	if cfg.MarkerDir == "" {
		t.Fatal("expected non-empty MarkerDir so the control center writes the cross-process heartbeat marker; got empty string")
	}

	// Pass-through fields should be wired unchanged.
	if cfg.NodeID != "test-node" {
		t.Errorf("NodeID = %q, want %q", cfg.NodeID, "test-node")
	}
	if cfg.HeadscaleNodeID != "758" {
		t.Errorf("HeadscaleNodeID = %q, want %q", cfg.HeadscaleNodeID, "758")
	}
	if cfg.OrgID != "org-123" {
		t.Errorf("OrgID = %q, want %q", cfg.OrgID, "org-123")
	}
	if cfg.LogFn == nil {
		t.Fatal("expected LogFn to be wired so control center logs route through the TUI")
	}
	cfg.LogFn("info", "hello")
	if loggedLevel != "info" || loggedMsg != "hello" {
		t.Errorf("LogFn did not route to the injected logger: got (%q, %q)", loggedLevel, loggedMsg)
	}
}
