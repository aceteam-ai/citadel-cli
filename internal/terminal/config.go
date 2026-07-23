// internal/terminal/config.go
package terminal

import (
	"os"
	"runtime"
	"strconv"
	"time"
)

// DefaultSessionName is the base tmux session name used when
// CITADEL_TERMINAL_SESSION is not set.
//
// It defaults to "citadel", which turns persistent tmux backing ON by default
// (citadel #585, Gap 2): each connection attaches to (or creates) a per-user
// `tmux new-session -A -s <name>` session, so a repeated `citadel connect
// <node>` — and a reconnect after a drop — re-attaches to the SAME live shell
// (running command, scrollback, cwd preserved) instead of spawning a duplicate.
//
// This is a deliberate behavior change from the previous empty (tmux-off)
// default: platform/token terminals that used to get a fresh bare shell now get
// a resumable tmux session too. It stays safe because (a) when no tmux binary is
// resolvable the server logs a warning and falls back to a bare shell, and
// (b) operators can force a bare shell by setting CITADEL_TERMINAL_SESSION to a
// disable sentinel ("none"/"off"/"disabled"/"false"/"0").
const DefaultSessionName = "citadel"

// Config holds the terminal server configuration
type Config struct {
	// Host is the address the WebSocket server binds to (default: 127.0.0.1)
	// External access comes through the tsnet VPN listener, not the TCP bind.
	Host string

	// Port is the port the WebSocket server listens on
	Port int

	// Version is the server version string (passed from cmd package)
	Version string

	// Enabled determines whether the terminal service is active
	Enabled bool

	// IdleTimeout is the duration after which idle connections are closed
	IdleTimeout time.Duration

	// MaxConnections is the maximum number of concurrent terminal sessions
	MaxConnections int

	// Shell is the shell to spawn for terminal sessions
	Shell string

	// SessionName, when non-empty, makes the server back every connection with
	// a persistent named tmux session (`tmux new-session -A -s <name>`) instead
	// of a fresh bare shell. The tmux server keeps the session alive after a
	// client disconnects, so reconnecting re-attaches to the same session and
	// the terminal state survives. Requires a resolvable tmux binary; when tmux
	// is unavailable the server falls back to a bare shell. Configured via
	// CITADEL_TERMINAL_SESSION.
	//
	// SessionName is treated as a base name. The server derives a stable,
	// per-user session name from it (base + sanitized user ID) so each user
	// re-attaches to their own persistent terminal across reconnects while
	// staying isolated from other users.
	//
	// It defaults to DefaultSessionName ("citadel"), so tmux backing is ON by
	// default (citadel #585): terminals get a persistent, reconnect-resilient,
	// per-user session so `citadel connect` re-attaches seamlessly. To force a
	// fresh, non-persistent bare shell (e.g. for scripted/automated terminal use
	// that wants no state bleed across connections), set CITADEL_TERMINAL_SESSION
	// to a disable sentinel ("none"/"off"/"disabled"/"false"/"0").
	SessionName string

	// TrustMeshPeers enables mesh-peer identity trust for connections that
	// arrive over the VPN listener (citadel #585). When true AND a
	// MeshIdentityResolver is wired (SetMeshResolver), a tokenless VPN
	// connection whose source resolves to a verified same-owner tailnet peer is
	// authorized without a platform-minted token. It has NO effect on the
	// localhost/LAN bind, which always requires a token. Defaults to true
	// (CITADEL_TERMINAL_TRUST_MESH); set false to force tokens even on the mesh.
	TrustMeshPeers bool

	// AuthServiceURL is the URL of the AceTeam auth service for token validation
	AuthServiceURL string

	// OrgID is the organization ID for token validation
	OrgID string

	// RateLimitRPS is the rate limit in requests per second per IP
	RateLimitRPS float64

	// RateLimitBurst is the burst limit for rate limiting
	RateLimitBurst int

	// TokenRefreshInterval is how often to refresh the token cache from the API
	TokenRefreshInterval time.Duration

	// Debug enables verbose debug logging
	Debug bool
}

// DefaultConfig returns a Config with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		Host:                 getEnvOrDefault("CITADEL_TERMINAL_HOST", "127.0.0.1"),
		Port:                 getEnvInt("CITADEL_TERMINAL_PORT", 7860),
		Version:              "dev",
		Enabled:              getEnvBool("CITADEL_TERMINAL_ENABLED", true),
		IdleTimeout:          time.Duration(getEnvInt("CITADEL_TERMINAL_IDLE_TIMEOUT", 30)) * time.Minute,
		MaxConnections:       getEnvInt("CITADEL_TERMINAL_MAX_CONNECTIONS", 10),
		Shell:                getEnvOrDefault("CITADEL_TERMINAL_SHELL", defaultShell()),
		SessionName:          getEnvOrDefault("CITADEL_TERMINAL_SESSION", DefaultSessionName),
		TrustMeshPeers:       getEnvBool("CITADEL_TERMINAL_TRUST_MESH", true),
		AuthServiceURL:       getEnvOrDefault("CITADEL_AUTH_HOST", "https://aceteam.ai"),
		RateLimitRPS:         1.0, // 1 connection attempt per second per IP
		RateLimitBurst:       5,   // Allow bursts of 5
		TokenRefreshInterval: time.Duration(getEnvInt("CITADEL_TOKEN_REFRESH_INTERVAL", 60)) * time.Minute,
		Debug:                getEnvBool("CITADEL_TERMINAL_DEBUG", false),
	}
}

// defaultShell returns the appropriate default shell for the current platform
func defaultShell() string {
	switch runtime.GOOS {
	case "darwin":
		return "/bin/zsh"
	case "windows":
		// Try PowerShell Core first, then Windows PowerShell
		if _, err := os.Stat(`C:\Program Files\PowerShell\7\pwsh.exe`); err == nil {
			return `C:\Program Files\PowerShell\7\pwsh.exe`
		}
		return "powershell.exe"
	default: // linux and others
		if shell := os.Getenv("SHELL"); shell != "" {
			return shell
		}
		return "/bin/bash"
	}
}

// getEnvOrDefault returns the environment variable value or a default
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvInt returns the environment variable as an int or a default
func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return defaultValue
}

// getEnvBool returns the environment variable as a bool or a default
func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolVal, err := strconv.ParseBool(value); err == nil {
			return boolVal
		}
	}
	return defaultValue
}

// Validate checks that the configuration is valid
func (c *Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return ErrInvalidPort
	}
	if c.MaxConnections < 1 {
		return ErrInvalidMaxConnections
	}
	if c.IdleTimeout < time.Minute {
		return ErrInvalidIdleTimeout
	}
	if c.OrgID == "" {
		return ErrMissingOrgID
	}
	return nil
}
