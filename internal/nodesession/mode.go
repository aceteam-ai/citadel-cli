// Package nodesession stores the durable mode of this machine's Citadel node.
// It does not create network connections or subscribe to jobs.
package nodesession

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

type Mode string

const (
	Presence Mode = "presence"
	Worker   Mode = "worker"
	fileName      = "session.yaml"
)

type Config struct {
	Mode      Mode      `yaml:"mode"`
	UpdatedAt time.Time `yaml:"updated_at"`
}

func DefaultMode(hasDeviceCredentials bool) Mode {
	if hasDeviceCredentials {
		return Worker
	}
	return Presence
}

func validate(mode Mode) error {
	if mode != Presence && mode != Worker {
		return fmt.Errorf("invalid session mode %q (want presence or worker)", mode)
	}
	return nil
}

func Path(nodeConfigDir string) string { return filepath.Join(nodeConfigDir, fileName) }

// Load never silently defaults a corrupt or unknown mode to worker. In
// particular, a malformed file must not subscribe an authkey-only node.
func Load(nodeConfigDir string) (Config, error) {
	data, err := os.ReadFile(Path(nodeConfigDir))
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("read session config: %w", err)
	}
	if err := validate(cfg.Mode); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LoadOrInitialize is idempotent across re-enrolls: an explicit persisted
// choice always wins over the current credential tier. Only a fresh node gets
// the enrollment-tier default.
func LoadOrInitialize(nodeConfigDir string, hasDeviceCredentials bool) (Config, error) {
	cfg, err := Load(nodeConfigDir)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Config{}, err
	}
	cfg = Config{Mode: DefaultMode(hasDeviceCredentials), UpdatedAt: time.Now().UTC()}
	if err := Save(nodeConfigDir, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Save writes a restrictive, atomic file so an interrupted update cannot
// turn a worker into an implicit default or expose node configuration.
func Save(nodeConfigDir string, cfg Config) error {
	if err := validate(cfg.Mode); err != nil {
		return err
	}
	if err := os.MkdirAll(nodeConfigDir, 0o700); err != nil {
		return err
	}
	if cfg.UpdatedAt.IsZero() {
		cfg.UpdatedAt = time.Now().UTC()
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(nodeConfigDir, ".session-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), Path(nodeConfigDir))
}
