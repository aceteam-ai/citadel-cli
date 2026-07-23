// internal/terminal/config_test.go
package terminal

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()

	if config.Port != 7860 {
		t.Errorf("expected default port 7860, got %d", config.Port)
	}

	if !config.Enabled {
		t.Error("expected default enabled to be true")
	}

	if config.IdleTimeout != 30*time.Minute {
		t.Errorf("expected default idle timeout 30m, got %v", config.IdleTimeout)
	}

	if config.MaxConnections != 10 {
		t.Errorf("expected default max connections 10, got %d", config.MaxConnections)
	}

	if config.Shell == "" {
		t.Error("expected default shell to be set")
	}

	if config.AuthServiceURL != "https://aceteam.ai" {
		t.Errorf("expected default auth service URL https://aceteam.ai, got %s", config.AuthServiceURL)
	}
}

func TestConfigFromEnv(t *testing.T) {
	// Set environment variables
	os.Setenv("CITADEL_TERMINAL_PORT", "8080")
	os.Setenv("CITADEL_TERMINAL_ENABLED", "false")
	os.Setenv("CITADEL_TERMINAL_IDLE_TIMEOUT", "60")
	os.Setenv("CITADEL_TERMINAL_MAX_CONNECTIONS", "20")
	defer func() {
		os.Unsetenv("CITADEL_TERMINAL_PORT")
		os.Unsetenv("CITADEL_TERMINAL_ENABLED")
		os.Unsetenv("CITADEL_TERMINAL_IDLE_TIMEOUT")
		os.Unsetenv("CITADEL_TERMINAL_MAX_CONNECTIONS")
	}()

	config := DefaultConfig()

	if config.Port != 8080 {
		t.Errorf("expected port 8080 from env, got %d", config.Port)
	}

	if config.Enabled {
		t.Error("expected enabled to be false from env")
	}

	if config.IdleTimeout != 60*time.Minute {
		t.Errorf("expected idle timeout 60m from env, got %v", config.IdleTimeout)
	}

	if config.MaxConnections != 20 {
		t.Errorf("expected max connections 20 from env, got %d", config.MaxConnections)
	}
}

// TestDefaultConfig_TmuxOnByDefault pins the on-by-default contract flipped by
// citadel #585: with no CITADEL_TERMINAL_SESSION set the default is "citadel",
// tmux backing is enabled, and (when a tmux binary is available) a persistent
// attach-or-create command is produced so `citadel connect` re-attach works out
// of the box.
func TestDefaultConfig_TmuxOnByDefault(t *testing.T) {
	// Empty simulates "unset": getEnvOrDefault treats "" as absent and falls
	// back to DefaultSessionName, which is now "citadel".
	t.Setenv("CITADEL_TERMINAL_SESSION", "")
	bin := makeFakeTmux(t)

	config := DefaultConfig()
	if config.SessionName != "citadel" {
		t.Errorf("expected default SessionName %q, got %q", "citadel", config.SessionName)
	}
	if sessionDisabled(config.SessionName) {
		t.Error("expected tmux backing to be ENABLED by default (citadel #585)")
	}
	got := sessionCommand(config.SessionName, "/bin/bash")
	want := []string{bin, "new-session", "-A", "-s", "citadel", "/bin/bash"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sessionCommand = %v, want %v", got, want)
	}
}

// TestDefaultConfig_TmuxOptOut verifies operators can force a bare, non-persistent
// shell by setting CITADEL_TERMINAL_SESSION to a disable sentinel.
func TestDefaultConfig_TmuxOptOut(t *testing.T) {
	t.Setenv("CITADEL_TERMINAL_SESSION", "none")
	makeFakeTmux(t) // even with tmux available, opt-out wins

	config := DefaultConfig()
	if config.SessionName != "none" {
		t.Errorf("expected SessionName %q from env, got %q", "none", config.SessionName)
	}
	if !sessionDisabled(config.SessionName) {
		t.Error("expected tmux backing to be disabled when opted out")
	}
	if got := sessionCommand(config.SessionName, config.Shell); got != nil {
		t.Errorf("expected nil session command when opted out, got %v", got)
	}
}

// TestDefaultConfig_TmuxMissingFallsBackToBareShell verifies the graceful
// fallback: with tmux enabled (default) but no resolvable tmux binary,
// sessionCommand returns nil so the server runs a bare shell (it logs a warning
// and never fails the connection). The missing binary is mocked via
// CITADEL_TMUX_BIN pointing at a nonexistent path.
func TestDefaultConfig_TmuxMissingFallsBackToBareShell(t *testing.T) {
	t.Setenv("CITADEL_TERMINAL_SESSION", "") // default -> "citadel" (enabled)
	t.Setenv("CITADEL_TMUX_BIN", filepath.Join(t.TempDir(), "no-such-tmux"))

	config := DefaultConfig()
	if sessionDisabled(config.SessionName) {
		t.Fatal("precondition: tmux should be enabled by default")
	}
	if got := sessionCommand(config.SessionName, config.Shell); got != nil {
		t.Errorf("expected nil session command (bare-shell fallback) when tmux is missing, got %v", got)
	}
}

// TestDefaultConfig_TrustMeshDefault pins the mesh-trust default (citadel #585):
// on by default, opt-outable via CITADEL_TERMINAL_TRUST_MESH.
func TestDefaultConfig_TrustMeshDefault(t *testing.T) {
	t.Setenv("CITADEL_TERMINAL_TRUST_MESH", "")
	if !DefaultConfig().TrustMeshPeers {
		t.Error("expected TrustMeshPeers to default to true")
	}
	t.Setenv("CITADEL_TERMINAL_TRUST_MESH", "false")
	if DefaultConfig().TrustMeshPeers {
		t.Error("expected TrustMeshPeers=false from env opt-out")
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  *Config
		wantErr error
	}{
		{
			name: "valid config",
			config: &Config{
				Port:           7860,
				MaxConnections: 10,
				IdleTimeout:    30 * time.Minute,
				OrgID:          "test-org",
			},
			wantErr: nil,
		},
		{
			name: "invalid port - too low",
			config: &Config{
				Port:           0,
				MaxConnections: 10,
				IdleTimeout:    30 * time.Minute,
				OrgID:          "test-org",
			},
			wantErr: ErrInvalidPort,
		},
		{
			name: "invalid port - too high",
			config: &Config{
				Port:           70000,
				MaxConnections: 10,
				IdleTimeout:    30 * time.Minute,
				OrgID:          "test-org",
			},
			wantErr: ErrInvalidPort,
		},
		{
			name: "invalid max connections",
			config: &Config{
				Port:           7860,
				MaxConnections: 0,
				IdleTimeout:    30 * time.Minute,
				OrgID:          "test-org",
			},
			wantErr: ErrInvalidMaxConnections,
		},
		{
			name: "invalid idle timeout",
			config: &Config{
				Port:           7860,
				MaxConnections: 10,
				IdleTimeout:    30 * time.Second,
				OrgID:          "test-org",
			},
			wantErr: ErrInvalidIdleTimeout,
		},
		{
			name: "missing org ID",
			config: &Config{
				Port:           7860,
				MaxConnections: 10,
				IdleTimeout:    30 * time.Minute,
				OrgID:          "",
			},
			wantErr: ErrMissingOrgID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if err != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
