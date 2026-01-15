// internal/nexus/sshkeys.go
// SSH key synchronization from AceTeam platform
package nexus

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Markers for the managed section of authorized_keys
	ManagedKeysStart = "# === AceTeam Managed Keys (DO NOT EDIT) ==="
	ManagedKeysEnd   = "# === End AceTeam Managed Keys ==="
)

// SSHKeysClient handles SSH key synchronization with the AceTeam platform
type SSHKeysClient struct {
	baseURL    string
	httpClient *http.Client
}

// AuthorizedKeysResponse represents the response from the authorized-keys endpoint
type AuthorizedKeysResponse struct {
	Keys []SSHKeyInfo `json:"keys"`
	// AuthorizedKeys is the pre-formatted string ready to write to authorized_keys file
	AuthorizedKeys string `json:"authorized_keys"`
	Count          int    `json:"count"`
}

// SSHKeyInfo represents a single SSH key with metadata
type SSHKeyInfo struct {
	PublicKey string `json:"public_key"`
	Name      string `json:"name"`
	KeyType   string `json:"key_type"`
	UserEmail string `json:"user_email"`
	UserName  string `json:"user_name"`
}

// SSHSyncConfig holds configuration for SSH key synchronization
type SSHSyncConfig struct {
	APIToken string // Bearer token for AceTeam API
	NodeID   string // Node ID in AceTeam platform
	BaseURL  string // API base URL (default: https://aceteam.ai)
}

// NewSSHKeysClient creates a new SSH keys synchronization client
func NewSSHKeysClient(baseURL string) *SSHKeysClient {
	return &SSHKeysClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// FetchAuthorizedKeys retrieves the authorized SSH keys for a node from the AceTeam API
func (c *SSHKeysClient) FetchAuthorizedKeys(nodeID, apiToken string) (*AuthorizedKeysResponse, error) {
	url := fmt.Sprintf("%s/api/fabric/nodes/%s/authorized-keys", c.baseURL, nodeID)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to AceTeam API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("authentication failed (status %d): re-authenticate with 'citadel login'", resp.StatusCode)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("node not found in AceTeam platform")
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var response AuthorizedKeysResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &response, nil
}

// SyncAuthorizedKeys fetches SSH keys from AceTeam and writes them to the authorized_keys file.
// It preserves any existing user keys that are not in the managed section.
func SyncAuthorizedKeys(config SSHSyncConfig) error {
	if config.APIToken == "" || config.NodeID == "" {
		return fmt.Errorf("SSH sync not configured: missing API token or node ID")
	}

	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = "https://aceteam.ai"
	}

	client := NewSSHKeysClient(baseURL)

	// Fetch keys from AceTeam
	response, err := client.FetchAuthorizedKeys(config.NodeID, config.APIToken)
	if err != nil {
		return fmt.Errorf("failed to fetch SSH keys: %w", err)
	}

	// Determine authorized_keys path
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	sshDir := filepath.Join(homeDir, ".ssh")
	authKeysPath := filepath.Join(sshDir, "authorized_keys")

	// Ensure .ssh directory exists with correct permissions
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return fmt.Errorf("failed to create .ssh directory: %w", err)
	}

	// Read existing authorized_keys (if exists)
	existingKeys, err := readExistingKeys(authKeysPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read existing authorized_keys: %w", err)
	}

	// Build new authorized_keys content
	var newContent strings.Builder

	// Write non-managed keys first
	for _, key := range existingKeys {
		newContent.WriteString(key)
		newContent.WriteString("\n")
	}

	// Add blank line before managed section if there are existing keys
	if len(existingKeys) > 0 {
		newContent.WriteString("\n")
	}

	// Write managed section
	newContent.WriteString(ManagedKeysStart)
	newContent.WriteString("\n")
	if response.AuthorizedKeys != "" {
		// Ensure each key is on its own line
		keys := strings.Split(strings.TrimSpace(response.AuthorizedKeys), "\n")
		for _, key := range keys {
			if key = strings.TrimSpace(key); key != "" {
				newContent.WriteString(key)
				newContent.WriteString("\n")
			}
		}
	}
	newContent.WriteString(ManagedKeysEnd)
	newContent.WriteString("\n")

	// Write to file atomically
	tmpPath := authKeysPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(newContent.String()), 0600); err != nil {
		return fmt.Errorf("failed to write temporary file: %w", err)
	}

	if err := os.Rename(tmpPath, authKeysPath); err != nil {
		os.Remove(tmpPath) // Clean up on failure
		return fmt.Errorf("failed to update authorized_keys: %w", err)
	}

	return nil
}

// readExistingKeys reads the authorized_keys file and returns keys that are NOT in the managed section
func readExistingKeys(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var keys []string
	inManagedSection := false
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()

		// Track managed section
		if strings.TrimSpace(line) == ManagedKeysStart {
			inManagedSection = true
			continue
		}
		if strings.TrimSpace(line) == ManagedKeysEnd {
			inManagedSection = false
			continue
		}

		// Skip lines in managed section
		if inManagedSection {
			continue
		}

		// Skip empty lines and comments for cleaner output
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		keys = append(keys, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return keys, nil
}

// LoadSSHSyncConfig loads SSH sync configuration from the config directory.
// Returns nil config if not configured (not an error - just means sync is disabled).
func LoadSSHSyncConfig(configDir string) (*SSHSyncConfig, error) {
	configPath := filepath.Join(configDir, "ssh_sync.yaml")

	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		// Not configured - return nil, no error
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read SSH sync config: %w", err)
	}

	// Simple YAML parsing (avoid adding dependency for just 3 fields)
	config := &SSHSyncConfig{}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		// Remove quotes if present
		value = strings.Trim(value, "\"'")

		switch key {
		case "api_token":
			config.APIToken = value
		case "node_id":
			config.NodeID = value
		case "base_url":
			config.BaseURL = value
		}
	}

	if config.APIToken == "" || config.NodeID == "" {
		// Partially configured - treat as not configured
		return nil, nil
	}

	return config, nil
}

// SaveSSHSyncConfig saves the SSH sync configuration to the config directory.
func SaveSSHSyncConfig(configDir string, config *SSHSyncConfig) error {
	configPath := filepath.Join(configDir, "ssh_sync.yaml")

	content := fmt.Sprintf(`# AceTeam SSH Key Sync Configuration
# This file is auto-generated. Manual edits may be overwritten.

api_token: "%s"
node_id: "%s"
base_url: "%s"
`, config.APIToken, config.NodeID, config.BaseURL)

	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed to save SSH sync config: %w", err)
	}

	return nil
}
