// internal/terminal/config_test.go
package terminal

import (
	"os"
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

// TestDefaultConfig_TmuxOffByDefault pins the off-by-default contract: with no
// CITADEL_TERMINAL_SESSION set, no tmux session is configured, so the server
// runs a bare shell (sessionCommand returns nil and sessionDisabled is true).
func TestDefaultConfig_TmuxOffByDefault(t *testing.T) {
	// Empty simulates "unset": getEnvOrDefault treats "" as absent and falls
	// back to DefaultSessionName, which is now empty.
	t.Setenv("CITADEL_TERMINAL_SESSION", "")

	config := DefaultConfig()
	if config.SessionName != "" {
		t.Errorf("expected empty SessionName by default, got %q", config.SessionName)
	}
	if !sessionDisabled(config.SessionName) {
		t.Error("expected tmux backing to be disabled by default")
	}
	// Even with a real tmux binary available, no command is produced.
	makeFakeTmux(t)
	if got := sessionCommand(config.SessionName, config.Shell); got != nil {
		t.Errorf("expected nil session command by default, got %v", got)
	}
}

// TestDefaultConfig_TmuxOptIn verifies operators restore persistent tmux by
// setting CITADEL_TERMINAL_SESSION to a session name.
func TestDefaultConfig_TmuxOptIn(t *testing.T) {
	t.Setenv("CITADEL_TERMINAL_SESSION", "citadel")
	bin := makeFakeTmux(t)

	config := DefaultConfig()
	if config.SessionName != "citadel" {
		t.Errorf("expected SessionName %q from env, got %q", "citadel", config.SessionName)
	}
	if sessionDisabled(config.SessionName) {
		t.Error("expected tmux backing to be enabled when opted in")
	}
	got := sessionCommand(config.SessionName, "/bin/bash")
	want := []string{bin, "new-session", "-A", "-s", "citadel", "/bin/bash"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sessionCommand = %v, want %v", got, want)
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
