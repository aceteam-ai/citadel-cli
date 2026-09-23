// cmd/nexusurl.go
// Persist and honor the nexus/control URL node-side (citadel-cli#1110, the S0
// slice of the #1109 unified-entrypoint design).
//
// The bug: the control URL a node enrolled against was never persisted, so
// every service restart, control-center start, and `citadel work` rebuilt its
// ServerConfig with the compiled-in network.DefaultControlURL. On a node
// enrolled against a self-hosted nexus that mismatch invalidated the saved
// state and re-registered the node with a NEW identity on every restart.
//
// The fix has two halves, both in this package plus the read-back helpers in
// internal/network:
//   - persist the URL actually used to connect, at every enroll site
//     (persistNexusURLBestEffort / saveNexusURLToConfig here);
//   - read it back in every reconnect path (network.ResolveControlURL).
//
// Founder decision (D7 on #1109): the persisted value wins everywhere; the
// `--nexus` flag is consulted only at enroll time, and an explicit flag that
// differs from the persisted value on an already-enrolled node is refused with
// re-enroll guidance rather than silently moving (or churning) the node.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// persistNexusURLBestEffort writes the control URL this node just connected
// with to the machine-convergent node config, logging (never failing) on error.
// Called from every enroll site AFTER a successful connect, so the persisted
// value always equals the URL the node actually reached — persisting a URL the
// node never connected to would itself be an identity-churn vector.
func persistNexusURLBestEffort(url string) {
	if strings.TrimSpace(url) == "" {
		return
	}
	if err := saveNexusURLToConfig(url); err != nil {
		Debug("warning: could not persist nexus URL %q: %v", url, err)
	}
}

// saveNexusURLToConfig writes nexus_url into the same machine-convergent
// config.yaml (network.GetNodeConfigDir(), via nodeConfigDirFn) that holds
// device_api_token/org_id, so a root systemd `citadel work` and an interactive
// invocation agree on where it is (citadel-cli#845). Mirrors
// saveFabricNodeIDToConfig's read-modify-write discipline: it preserves every
// other key already in the file and carries aceteam_api_key forward from the
// legacy location on first write.
func saveNexusURLToConfig(url string) error {
	globalConfigDir := nodeConfigDirFn()
	globalConfigFile := filepath.Join(globalConfigDir, "config.yaml")

	if err := os.MkdirAll(globalConfigDir, 0755); err != nil {
		return fmt.Errorf("failed to create global config directory: %w", err)
	}

	var config map[string]interface{}
	data, err := os.ReadFile(globalConfigFile)
	if err == nil {
		if unmarshalErr := yaml.Unmarshal(data, &config); unmarshalErr != nil {
			config = nil
		}
	}
	if config == nil {
		config = make(map[string]interface{})
	}

	// Seed-on-first-write (citadel-cli#845): see seedAceteamAPIKeyFromLegacyFile.
	seedAceteamAPIKeyFromLegacyFile(config, filepath.Join(platform.ConfigDir(), "config.yaml"))

	config["nexus_url"] = url

	newData, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	if err := os.WriteFile(globalConfigFile, newData, 0600); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	// Fix ownership when this ran as root, same as the other config writers.
	fixStatePermissionsFn()

	return nil
}

// refuseNexusFlagMismatch refuses an explicit --nexus flag that differs from the
// control URL an already-enrolled node is bound to, returning an actionable
// re-enroll error. It is the production wrapper around the pure
// nexusFlagMismatchError, reading the two live signals: whether --nexus was set
// on this invocation and what URL is persisted.
func refuseNexusFlagMismatch(cmd *cobra.Command) error {
	return nexusFlagMismatchError(
		network.HasState(),
		cmd.Flags().Changed("nexus"),
		nexusURL,
		network.PersistedControlURL(),
	)
}

// nexusFlagMismatchError is the pure decision: return an actionable error only
// when the node is ALREADY enrolled (has network state AND a persisted control
// URL) and an EXPLICIT --nexus flag names a different control plane. Otherwise
// nil — the flag is free to drive a fresh enroll.
//
// "Enrolled" requires network state as well as a persisted URL: `citadel logout`
// clears the tsnet state (HasState()==false) but leaves config.yaml, so a
// post-logout re-enroll to a different nexus (the exact path the error message
// tells the user to take) must NOT be refused.
func nexusFlagMismatchError(hasState, explicitFlag bool, flagURL, persistedURL string) error {
	if !explicitFlag || !hasState || strings.TrimSpace(persistedURL) == "" {
		return nil
	}
	if normalizeControlURL(flagURL) == normalizeControlURL(persistedURL) {
		return nil
	}
	return fmt.Errorf(
		"this node is enrolled against %s, but --nexus %s names a different control plane.\n"+
			"  To move this node to a different control plane, run:\n"+
			"    citadel logout\n"+
			"    citadel enroll   (or: citadel init --nexus %s)",
		persistedURL, flagURL, flagURL)
}

// normalizeControlURL canonicalizes a control URL for equality comparison:
// trims surrounding whitespace and a single trailing slash. It does not
// lower-case or otherwise rewrite the host, so a genuine host/scheme difference
// is still treated as a mismatch.
func normalizeControlURL(url string) string {
	return strings.TrimSuffix(strings.TrimSpace(url), "/")
}
