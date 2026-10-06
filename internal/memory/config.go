// Package memory implements the Citadel-side onboarding for AceTeam agent
// memory (epic aceteam #7160). It stores a scoped act_ API key minted via the
// device-authorization "memory" flow and talks to the AceTeam memory MCP so
// that an external client (Claude Code) can recall memory before each prompt
// and capture a bounded semantic summary when a session ends.
package memory

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
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

const (
	ScopeRead  = "memory:read"
	ScopeWrite = "memory:write"
)

// ValidateScopes enforces the least-privilege contract of a memory device
// credential. The backend treats an absent scope list as legacy full access,
// so missing, duplicate, wildcard, admin, or otherwise additional scopes must
// fail closed before the bearer is persisted or exposed to an MCP client.
// Ordering is intentionally irrelevant.
func ValidateScopes(scopes []string) error {
	if len(scopes) != 2 {
		return fmt.Errorf("memory credential must carry exactly %s and %s", ScopeRead, ScopeWrite)
	}
	seen := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		if scope != ScopeRead && scope != ScopeWrite {
			return fmt.Errorf("memory credential contains an unexpected scope")
		}
		if seen[scope] {
			return fmt.Errorf("memory credential contains a duplicate scope")
		}
		seen[scope] = true
	}
	if !seen[ScopeRead] || !seen[ScopeWrite] {
		return fmt.Errorf("memory credential must carry exactly %s and %s", ScopeRead, ScopeWrite)
	}
	return nil
}

// ValidateCredential rejects a missing or over-privileged memory credential.
func (c *Config) ValidateCredential() error {
	if c == nil || strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("no memory API key configured")
	}
	if !validAceTeamAPIKey(c.APIKey) {
		return fmt.Errorf("memory API key has an invalid format")
	}
	if err := ValidateScopes(c.Scopes); err != nil {
		return err
	}
	return ValidateMCPURL(c.EffectiveMCPURL())
}

func validAceTeamAPIKey(key string) bool {
	if len(key) != 68 || !strings.HasPrefix(key, "act_") {
		return false
	}
	for _, ch := range key[4:] {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

// ValidateMCPURL prevents a scoped bearer from being sent to an ambiguous or
// plaintext remote endpoint. Plain HTTP remains available for loopback-only
// development servers.
func ValidateMCPURL(raw string) error {
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("memory MCP URL is invalid")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("memory MCP URL must use HTTPS (HTTP is allowed only on loopback)")
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
	path := ConfigPath(configDir)
	data, err := readPrivateRegularFile(path)
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

const maxConfigBytes = 1 << 20

// readPrivateRegularFile refuses links, special files, and (on Unix) files
// readable by another user. The second pathname check closes the ordinary
// lstat/open replacement window: the descriptor we read must still name the
// same regular file after open. The credential is tiny, so also cap reads to
// avoid turning a corrupt file into an unbounded allocation.
func readPrivateRegularFile(path string) ([]byte, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("memory config is not a regular file")
	}
	if runtime.GOOS != "windows" && pathInfo.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("memory config has insecure permissions %04o (group/other access is forbidden)", pathInfo.Mode().Perm())
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	openedInfo, err := f.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("memory config is not a regular file")
	}
	currentInfo, err := os.Lstat(path)
	if err != nil || currentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, currentInfo) {
		return nil, fmt.Errorf("memory config changed while opening")
	}

	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("memory config exceeds %d bytes", maxConfigBytes)
	}
	return data, nil
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
	if err := withFileLock(path, func() error { return atomicWriteFile(path, data, 0o600) }); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Remove deletes the local memory credential while holding the same sidecar
// lock used by Save. This prevents install/save and uninstall from mutating the
// credential concurrently.
func Remove(configDir string) error {
	path := ConfigPath(configDir)
	if err := withFileLock(path, func() error {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
