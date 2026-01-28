// internal/terminal/config.go
package terminal

import (
	"os"
	"runtime"
	"strconv"
	"time"
)

// Config holds the terminal server configuration
type Config struct {
	// Port is the port the WebSocket server listens on
	Port int

	// Enabled determines whether the terminal service is active
	Enabled bool

	// IdleTimeout is the duration after which idle connections are closed
	IdleTimeout time.Duration

	// MaxConnections is the maximum number of concurrent terminal sessions
	MaxConnections int

	// Shell is the shell to spawn for terminal sessions
	Shell string

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
		Port:                 getEnvInt("CITADEL_TERMINAL_PORT", 7860),
		Enabled:              getEnvBool("CITADEL_TERMINAL_ENABLED", true),
		IdleTimeout:          time.Duration(getEnvInt("CITADEL_TERMINAL_IDLE_TIMEOUT", 30)) * time.Minute,
		MaxConnections:       getEnvInt("CITADEL_TERMINAL_MAX_CONNECTIONS", 10),
		Shell:                getEnvOrDefault("CITADEL_TERMINAL_SHELL", defaultShell()),
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
