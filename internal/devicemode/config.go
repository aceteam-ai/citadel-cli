// Package devicemode implements the lightweight "device" profile for
// non-Citadel mesh devices (laptops) — aceteam #5959.
//
// A device is NOT a compute node: no worker, no job queues, no Redis. It holds
// the same fabric mTLS identity a Citadel node does (EC P-256 key + CA-signed
// leaf, via internal/nodeidentity) and uses it for exactly one thing: keeping
// its mesh membership alive. First join is interactive (browser approve on the
// platform pairing page — the trust-root event); everything after is derived
// from the leaf:
//
//	session broken / key expiring  ->  mTLS POST to the nexus reenroll service
//	                               ->  fresh org authkey
//	                               ->  re-run `tailscale up`
//
// Unlike Citadel nodes (embedded tsnet), a device babysits the SYSTEM
// tailscale daemon — the Tailscale app people already run on laptops —
// including the macOS App Store variant whose CLI lives inside Tailscale.app.
package devicemode

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

const (
	configFileName    = "device.json"
	machineIDFileName = "machine-id"

	// DefaultReenrollURL is the sovereign mTLS re-enrollment endpoint (#4583
	// PR-3): nginx on nexus terminates client-cert TLS and forwards the
	// handshake-verified leaf to the keyless verifier, which mints an org
	// authkey. The platform API is deliberately NOT in this path.
	DefaultReenrollURL = "https://nexus.aceteam.ai/fabric/reenroll"

	// DefaultNexusURL is the mesh coordination server handed to
	// `tailscale up --login-server`.
	DefaultNexusURL = "https://nexus.aceteam.ai"

	// DefaultAPIBaseURL is the platform base URL (pairing, CA chain, leaf
	// renewal). Filled in for configs written before leaf renewal existed so
	// the daemon can renew without a re-enrollment.
	DefaultAPIBaseURL = "https://aceteam.ai"
)

// Config is the persisted device-mode state written by `citadel device enroll`
// and read by the daemon loop. It contains no secrets — the private key and
// leaf live in the nodeidentity store (ConfigDir()/identity/).
type Config struct {
	// NodeUID is the stable fabric identity assigned at enrollment (the leaf's
	// CN / aceteam:node: SAN value).
	NodeUID string `json:"node_uid"`
	// OrgName is display-only context from enrollment (may be empty).
	OrgName string `json:"org_name,omitempty"`
	// NexusURL is the login server for tailscale.
	NexusURL string `json:"nexus_url"`
	// ReenrollURL is the mTLS self-heal endpoint.
	ReenrollURL string `json:"reenroll_url"`
	// APIBaseURL is the platform base URL used at enrollment (pairing + CA chain).
	APIBaseURL string `json:"api_base_url"`
}

// ConfigPath returns the device-mode config file location.
func ConfigPath() string {
	return filepath.Join(platform.ConfigDir(), configFileName)
}

// LoadConfig reads the persisted device config. A missing file returns
// (nil, os.ErrNotExist) so callers can distinguish "not enrolled" from a
// corrupt file.
func LoadConfig() (*Config, error) {
	return loadConfigFrom(ConfigPath())
}

func loadConfigFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse device config %s: %w", path, err)
	}
	if cfg.NexusURL == "" {
		cfg.NexusURL = DefaultNexusURL
	}
	if cfg.ReenrollURL == "" {
		cfg.ReenrollURL = DefaultReenrollURL
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = DefaultAPIBaseURL
	}
	return &cfg, nil
}

// SaveConfig persists the device config (0600 — no secrets, but it is
// per-device state nobody else needs to read).
func SaveConfig(cfg *Config) error {
	return saveConfigTo(ConfigPath(), cfg)
}

func saveConfigTo(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal device config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write device config: %w", err)
	}
	return nil
}

// MachineID returns a stable per-machine identifier used so a re-enrolled
// device keeps its node_uid (the backend keys cert rows on (machine_id, org)).
// It is a random ID persisted on first use — OS-agnostic and requiring no
// privileged reads. Wiping the citadel config directory forfeits the mapping,
// which simply yields a fresh node_uid on the next enrollment.
func MachineID() (string, error) {
	return machineIDAt(filepath.Join(platform.ConfigDir(), machineIDFileName))
}

func machineIDAt(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		id := string(data)
		if len(id) > 0 {
			return trimNewline(id), nil
		}
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate machine id: %w", err)
	}
	id := fmt.Sprintf("%x", buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("persist machine id: %w", err)
	}
	return id, nil
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
