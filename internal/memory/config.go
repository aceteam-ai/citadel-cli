// Package memory implements the Citadel-side onboarding for AceTeam agent
// memory (epic aceteam #7160). It stores a scoped act_ API key minted via the
// device-authorization "memory" flow and talks to the AceTeam memory MCP so
// that an external client (Claude Code) can recall memory before each prompt
// and capture durable facts after each turn.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigFileName is the name of the memory config file within the Citadel
// config directory.
const ConfigFileName = "memory.yaml"

// DefaultAPIBaseURL is the default AceTeam API/app host.
const DefaultAPIBaseURL = "https://aceteam.ai"

// Config holds the credential and endpoints for the AceTeam memory substrate.
// It is stored user-only (0600) because APIKey is a bearer secret.
type Config struct {
	// APIKey is the scoped act_ API key minted by the device-auth memory flow.
	APIKey string `yaml:"api_key"`
	// APIBaseURL is the AceTeam host (default https://aceteam.ai).
	APIBaseURL string `yaml:"api_base_url,omitempty"`
	// MCPURL overrides the derived MCP endpoint (optional).
	MCPURL string `yaml:"mcp_url,omitempty"`
	// OrgID / OrgName identify the organization the key is scoped to.
	OrgID   string `yaml:"org_id,omitempty"`
	OrgName string `yaml:"org_name,omitempty"`
	// Scopes echoes the grant the minted key carries (informational).
	Scopes []string `yaml:"scopes,omitempty"`
}

// DefaultMCPURL derives the AceTeam MCP endpoint for external clients from an
// API base URL, e.g. https://aceteam.ai/api/mcp/aceteam/mcp.
func DefaultMCPURL(apiBaseURL string) string {
	base := strings.TrimRight(apiBaseURL, "/")
	if base == "" {
		base = DefaultAPIBaseURL
	}
	return base + "/api/mcp/aceteam/mcp"
}

// EffectiveMCPURL returns the configured MCP URL or one derived from the base.
func (c *Config) EffectiveMCPURL() string {
	if c.MCPURL != "" {
		return c.MCPURL
	}
	base := c.APIBaseURL
	if base == "" {
		base = DefaultAPIBaseURL
	}
	return DefaultMCPURL(base)
}

// ConfigPath returns the memory config path within the given config dir.
func ConfigPath(configDir string) string {
	return filepath.Join(configDir, ConfigFileName)
}

// Load reads the memory config from configDir. It returns (nil, nil) when the
// file does not exist so callers (hooks) can fail open silently.
func Load(configDir string) (*Config, error) {
	data, err := os.ReadFile(ConfigPath(configDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ConfigFileName, err)
	}
	return &c, nil
}

// Save writes the memory config to configDir with user-only (0600) perms,
// creating the directory if needed.
func Save(configDir string, c *Config) error {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal memory config: %w", err)
	}
	path := ConfigPath(configDir)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// Tighten perms even if the file pre-existed with looser mode.
	_ = os.Chmod(path, 0o600)
	return nil
}
