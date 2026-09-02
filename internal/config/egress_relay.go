package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// EgressRelay controls the on-node SOCKS5 egress relay (citadel #787): a
// mesh-only listener that lets another citadel node tunnel outbound traffic
// through THIS node's own network egress (net.Dial), the server-side
// counterpart to #786's client-side `citadel socks`. Like energy sampling and
// the sensitive-surface permissions, it is default-OFF and default-DENY: a
// node never relays another peer's traffic until an operator or the platform
// explicitly turns it on.
type EgressRelay struct {
	// Enabled turns the relay listener on. When false (the default) `citadel
	// work` never binds the relay port at all -- not "binds but refuses every
	// connection", genuinely absent, mirroring EnergySampling's posture.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// AllowLAN disables the relay's destination deny-list (RFC1918/loopback/
	// link-local/CGNAT -- see internal/egressrelay.IsDestinationAllowed) when
	// true. Default false: an authorized peer can egress to the public
	// internet through this node but cannot pivot into this node's own LAN or
	// mesh unless an operator explicitly opts in.
	AllowLAN bool `yaml:"allow_lan" json:"allow_lan"`
}

const egressRelayFile = "egress-relay.yaml"

// DefaultEgressRelay returns EgressRelay with the relay off and LAN-pivot
// denied: the fail-closed default.
func DefaultEgressRelay() *EgressRelay {
	return &EgressRelay{Enabled: false, AllowLAN: false}
}

// LoadEgressRelay reads egress-relay settings from the config directory. A
// missing file returns defaults (disabled, LAN denied). Partial files
// preserve defaults for absent keys (unmarshal into a pre-initialized
// struct), mirroring LoadEnergy/LoadTelemetry.
func LoadEgressRelay(configDir string) *EgressRelay {
	e := DefaultEgressRelay()

	data, err := os.ReadFile(filepath.Join(configDir, egressRelayFile))
	if err != nil {
		return e
	}

	// yaml.Unmarshal only overwrites keys present in the file, so an absent
	// key keeps its default (false) value.
	_ = yaml.Unmarshal(data, e)
	return e
}

// SaveEgressRelay writes egress-relay settings to the config directory. Both
// the `citadel egress-relay` CLI and the APPLY_DEVICE_CONFIG handler call
// this so every configuration surface converges on one persisted value.
func SaveEgressRelay(configDir string, e *EgressRelay) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal egress relay config: %w", err)
	}

	return os.WriteFile(filepath.Join(configDir, egressRelayFile), data, 0644)
}
