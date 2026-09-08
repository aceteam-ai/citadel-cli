// cmd/egress_relay_resolve_test.go
package cmd

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// These tests exercise resolveEgressRelayFrom/resolveEgressAllowLANFrom (the
// pure, configDir-parameterized cores), NOT resolveEgressRelay/
// resolveEgressAllowLAN themselves. The wrappers resolve configDir via
// network.GetNodeConfigDir(), which follows a MACHINE-GLOBAL pointer
// (/etc/citadel/config.yaml when present) that a test's $HOME override
// cannot redirect -- calling the wrappers directly in a config-file-seeding
// test risks reading or writing a real node's actual config directory on a
// machine that happens to run one. See resolveEgressRelayFrom's doc comment
// in cmd/work.go.

func TestResolveEgressRelayFromDefaultsOff(t *testing.T) {
	dir := t.TempDir()
	if resolveEgressRelayFrom(dir) {
		t.Error("expected egress relay to default to off with no env var or config file")
	}
	if resolveEgressAllowLANFrom(dir) {
		t.Error("expected allow_lan to default to off with no env var or config file")
	}
}

func TestResolveEgressRelayFromConfigFileWins(t *testing.T) {
	dir := t.TempDir()
	relay := config.DefaultEgressRelay()
	relay.Enabled = true
	relay.AllowLAN = true
	if err := config.SaveEgressRelay(dir, relay); err != nil {
		t.Fatalf("SaveEgressRelay: %v", err)
	}

	if !resolveEgressRelayFrom(dir) {
		t.Error("expected persisted config to enable the relay")
	}
	if !resolveEgressAllowLANFrom(dir) {
		t.Error("expected persisted config to enable allow_lan")
	}
}

func TestResolveEgressRelayFromEnvVarWinsOverConfig(t *testing.T) {
	dir := t.TempDir()
	relay := config.DefaultEgressRelay()
	relay.Enabled = true // persisted ON
	if err := config.SaveEgressRelay(dir, relay); err != nil {
		t.Fatalf("SaveEgressRelay: %v", err)
	}

	t.Setenv("CITADEL_EGRESS_RELAY", "0") // env explicitly OFF must win
	if resolveEgressRelayFrom(dir) {
		t.Error("expected CITADEL_EGRESS_RELAY=0 to override a persisted enabled config")
	}
}

func TestResolveEgressRelayFromEnvVarTruthyValues(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"1", "true", "yes", "on"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("CITADEL_EGRESS_RELAY", v)
			if !resolveEgressRelayFrom(dir) {
				t.Errorf("expected CITADEL_EGRESS_RELAY=%q to enable the relay", v)
			}
		})
	}
}

func TestResolveEgressAllowLANFromEnvVarWinsOverConfig(t *testing.T) {
	dir := t.TempDir()
	relay := config.DefaultEgressRelay()
	relay.AllowLAN = true // persisted ON
	if err := config.SaveEgressRelay(dir, relay); err != nil {
		t.Fatalf("SaveEgressRelay: %v", err)
	}

	t.Setenv("CITADEL_EGRESS_ALLOW_LAN", "false")
	if resolveEgressAllowLANFrom(dir) {
		t.Error("expected CITADEL_EGRESS_ALLOW_LAN=false to override a persisted enabled config")
	}
}

// TestResolveEgressRelayEnvVarShortCircuitsBeforeAnyDiskRead pins that the
// unparameterized wrappers ARE safe to call directly as long as an env var
// is set (the env-var branch returns before ever calling
// network.GetNodeConfigDir() / reading a config file), which is what makes
// the CLI/worker wiring itself trivially exercisable without a real mesh or
// a real node config dir.
func TestResolveEgressRelayEnvVarShortCircuitsBeforeAnyDiskRead(t *testing.T) {
	t.Setenv("CITADEL_EGRESS_RELAY", "1")
	if !resolveEgressRelay() {
		t.Error("expected CITADEL_EGRESS_RELAY=1 to enable the relay via the real wrapper")
	}
	t.Setenv("CITADEL_EGRESS_RELAY", "0")
	if resolveEgressRelay() {
		t.Error("expected CITADEL_EGRESS_RELAY=0 to disable the relay via the real wrapper")
	}

	t.Setenv("CITADEL_EGRESS_ALLOW_LAN", "1")
	if !resolveEgressAllowLAN() {
		t.Error("expected CITADEL_EGRESS_ALLOW_LAN=1 to enable allow_lan via the real wrapper")
	}
	t.Setenv("CITADEL_EGRESS_ALLOW_LAN", "0")
	if resolveEgressAllowLAN() {
		t.Error("expected CITADEL_EGRESS_ALLOW_LAN=0 to disable allow_lan via the real wrapper")
	}
}
