package nodevault

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// policyFileName holds the node-level master-PIN policy. It is a CONFIG VALUE
// from day one (issue #796 amendment): the minimum length and whether a
// passphrase is allowed are node settings, never hardcoded at the setter.
const policyFileName = "policy.yaml"

// LoadPolicy reads the node master-PIN policy from baseDir, falling back to
// DefaultPolicy() (6-char minimum, passphrases allowed, 60-bit badge threshold)
// when no policy file exists. Absent fields keep their default, so a policy
// file that predates a field still yields sane enforcement.
func LoadPolicy(baseDir string) Policy {
	p := DefaultPolicy()
	data, err := os.ReadFile(filepath.Join(baseDir, vaultDirName, policyFileName))
	if err != nil {
		return p
	}
	_ = yaml.Unmarshal(data, &p)
	return p.normalized()
}

// SavePolicy persists the node master-PIN policy under baseDir.
func SavePolicy(baseDir string, p Policy) error {
	if p.MinLength < 1 {
		return errors.New("nodevault: policy MinLength must be at least 1")
	}
	dir := filepath.Join(baseDir, vaultDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("nodevault: create dir: %w", err)
	}
	data, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("nodevault: marshal policy: %w", err)
	}
	return writeFileAtomic(filepath.Join(dir, policyFileName), data, 0o600)
}
