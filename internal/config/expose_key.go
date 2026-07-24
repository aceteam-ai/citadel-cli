// internal/config/expose_key.go
//
// Per-node signing key for gateway `link` access tokens (issue #598).
//
// The gateway signs shareable `link` exposure tokens with an HMAC key that must
// (a) be stable across restarts so a token minted yesterday still verifies today
// and (b) never leave the node. There is no existing per-node secret suited to
// this: the passcode hash is a bcrypt digest (not a raw HMAC key) and may be
// unset. So we mint a dedicated 32-byte random key on first use and persist it
// 0600 alongside the other node config, exactly like the passcode hash.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// exposeKeyFile is the filename (under the config dir) holding the hex-encoded
// link-token signing key.
const exposeKeyFile = "expose_signing_key"

// exposeKeyLen is the length in bytes of a freshly-minted signing key.
const exposeKeyLen = 32

// LoadOrCreateExposeSigningKey returns the node's persistent link-token signing
// key, generating and persisting one (0600) on first use. The key is stored
// hex-encoded. A short/corrupt existing file is treated as absent and rotated
// (which safely invalidates any outstanding link tokens — they fail closed).
func LoadOrCreateExposeSigningKey(configDir string) ([]byte, error) {
	path := filepath.Join(configDir, exposeKeyFile)
	if data, err := os.ReadFile(path); err == nil {
		if key, derr := hex.DecodeString(strings.TrimSpace(string(data))); derr == nil && len(key) >= exposeKeyLen {
			return key, nil
		}
		// Fall through to regenerate a malformed/short key.
	}

	key := make([]byte, exposeKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate expose signing key: %w", err)
	}
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0600); err != nil {
		return nil, fmt.Errorf("persist expose signing key: %w", err)
	}
	return key, nil
}
