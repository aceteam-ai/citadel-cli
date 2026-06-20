package proxmox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const configFileName = "proxmox.json"

// Config holds saved Proxmox connection settings.
type Config struct {
	BaseURL     string `json:"base_url"`
	TokenID     string `json:"token_id,omitempty"`
	TokenSecret string `json:"token_secret,omitempty"`
	NodeName    string `json:"node_name,omitempty"`
}

func configPath(configDir string) string {
	return filepath.Join(configDir, configFileName)
}

// LoadConfig reads the Proxmox config from the given config directory.
// Returns nil, nil if the file does not exist.
func LoadConfig(configDir string) (*Config, error) {
	path := configPath(configDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading proxmox config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing proxmox config: %w", err)
	}
	return &cfg, nil
}

// SaveConfig writes the Proxmox config to the given config directory.
func SaveConfig(configDir string, cfg *Config) error {
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling proxmox config: %w", err)
	}

	path := configPath(configDir)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing proxmox config: %w", err)
	}
	return nil
}

// IsConfigured returns true if a Proxmox config file exists and has a base URL.
func IsConfigured(configDir string) bool {
	cfg, err := LoadConfig(configDir)
	if err != nil || cfg == nil {
		return false
	}
	return cfg.BaseURL != ""
}

// ClientFromConfig creates a Client from saved configuration.
// Returns nil if not configured.
func ClientFromConfig(configDir string) (*Client, error) {
	cfg, err := LoadConfig(configDir)
	if err != nil {
		return nil, err
	}
	if cfg == nil || cfg.BaseURL == "" {
		return nil, nil
	}

	return NewClient(ClientConfig{
		BaseURL:     cfg.BaseURL,
		TokenID:     cfg.TokenID,
		TokenSecret: cfg.TokenSecret,
	}), nil
}
