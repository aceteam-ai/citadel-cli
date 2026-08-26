// cmd/manifest.go
package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

// Service defines a single managed service.
type Service struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type,omitempty"`         // "native" or "docker" (default: auto-detect)
	ComposeFile string `yaml:"compose_file,omitempty"` // For docker services
	Port        int    `yaml:"port,omitempty"`         // For native services
	// DesiredStatus is the operator-assigned run-state for boot. Empty means the
	// service is started on boot (the historical behavior). "stopped" makes a
	// remote MODULE_SET "stopped" DURABLE: the service stays installed but the
	// boot paths (citadel run / citadel work) SKIP composing it up, so a stop
	// survives a reboot instead of silently coming back. This is a per-service
	// marker; it is NOT the same as uninstalling (which removes the service).
	DesiredStatus string `yaml:"desired_status,omitempty"`
	// EvictedByJob is a durable marker set when a job-scoped GPU reservation
	// (citadel-cli#832, internal/jobs.ServiceHandler.Reserve) stopped this
	// service to free VRAM. Non-empty means DesiredStatus=="stopped" exists
	// BECAUSE that reservation evicted it, so Release(jobID) — or a worker-
	// startup reconcile, if the reserving process crashed — should restart it.
	// Distinct from a plain #577 preemptForVRAM eviction, which stops peers
	// WITHOUT tagging them (sticky forever until an explicit SERVICE_START).
	// This struct never WRITES the field (internal/jobs owns that, via its own
	// yaml.Node-surgery setter to avoid a jobs->cmd import), but it must be
	// modeled here so any cmd-package manifest rewrite (writeManifest does a
	// full struct round-trip) does not silently drop it — see #832's PR
	// description for why that failure mode is the one that matters most.
	EvictedByJob string `yaml:"evicted_by_job,omitempty"`
	// EvictedPriorStatus records this service's DesiredStatus value immediately
	// before a reservation (above) evicted it, so Release restores that exact
	// prior durable intent instead of unconditionally clearing it — e.g. a
	// service an operator had already marked "stopped" (whose compose-down
	// then failed, leaving it still running and a preemption candidate) must
	// not be silently flipped to start-on-boot by an unrelated reservation.
	EvictedPriorStatus string `yaml:"evicted_prior_status,omitempty"`
}

// serviceStartDisabled reports whether a service is marked "stopped" and must be
// SKIPPED by the boot-time service-start paths (runAllServices in cmd/run.go and
// startManagedServices in cmd/work.go). This is the single predicate both boot
// paths consult so a remote-assigned "stopped" state is honored consistently and
// does not restart on reboot.
func serviceStartDisabled(s Service) bool {
	return strings.EqualFold(strings.TrimSpace(s.DesiredStatus), "stopped")
}

// ManifestCapabilities defines the optional capabilities section in citadel.yaml.
// If not declared, capabilities are auto-detected at startup.
type ManifestCapabilities struct {
	GPUs    []ManifestGPU `yaml:"gpus,omitempty"`
	Engines []string      `yaml:"engines,omitempty"` // inference engines: vllm, sglang, ollama, llamacpp
}

// ManifestGPU describes a GPU declared in the manifest.
type ManifestGPU struct {
	Name   string `yaml:"name"`              // e.g. "NVIDIA GeForce RTX 3090"
	VRAMMb int    `yaml:"vram_mb,omitempty"` // e.g. 24576
	Count  int    `yaml:"count,omitempty"`   // defaults to 1
}

// CitadelManifest defines the structure of the citadel.yaml file.
type CitadelManifest struct {
	Node struct {
		Name  string   `yaml:"name"`
		Tags  []string `yaml:"tags"`
		OrgID string   `yaml:"org_id,omitempty"`
	} `yaml:"node"`
	Services     []Service             `yaml:"services"`
	Capabilities *ManifestCapabilities `yaml:"capabilities,omitempty"`
	// PinnedServices is a node-wide allowlist of service names that must NEVER be
	// preempted (auto-stopped) to make room for another deploy (citadel-cli#577).
	// Everything not listed is preemptible: a SERVICE_START that declares a VRAM
	// budget may durably stop non-pinned services to fit. Empty/absent =>
	// preemption allowed for all services (the default).
	PinnedServices []string `yaml:"pinned_services,omitempty"`
}

// manifestPinnedServices returns the pinned_services allowlist for a manifest,
// nil-safe so heartbeat/collector wiring can pass a possibly-nil manifest
// (citadel-cli#577).
func manifestPinnedServices(m *CitadelManifest) []string {
	if m == nil {
		return nil
	}
	return m.PinnedServices
}

// findAndReadManifest locates and parses the node's manifest file.
// It exclusively uses the global config file as the single source of truth for
// locating the node's configuration directory. This ensures consistent behavior
// regardless of the current working directory.
//
// EXCEPTION (citadel#853): when --node-dir/CITADEL_NODE_DIR is set
// (resolveNodeDirOverride), the global config indirection is bypassed
// entirely and citadel.yaml is read directly from the override directory. See
// cmd/nodedir.go for why this exists and its exact scope.
func findAndReadManifest() (*CitadelManifest, string, error) {
	if override := resolveNodeDirOverride(); override != "" {
		return readManifestFromDir(override)
	}

	globalConfigFile := filepath.Join(platform.ConfigDir(), "config.yaml")

	// Step 1: Read the global config file to find the node's directory.
	globalConfigData, err := os.ReadFile(globalConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("global config not found at %s. Please run 'citadel init'", globalConfigFile)
		}
		return nil, "", fmt.Errorf("could not read global config %s: %w", globalConfigFile, err)
	}

	var globalConf struct {
		NodeConfigDir string `yaml:"node_config_dir"`
	}
	if err := yaml.Unmarshal(globalConfigData, &globalConf); err != nil {
		return nil, "", fmt.Errorf("could not parse global config %s: %w", globalConfigFile, err)
	}

	if globalConf.NodeConfigDir == "" {
		// Try to auto-fix by checking default location
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, "", fmt.Errorf("global config %s is invalid: missing 'node_config_dir'", globalConfigFile)
		}

		defaultNodeDir := filepath.Join(homeDir, "citadel-node")
		defaultManifest := filepath.Join(defaultNodeDir, "citadel.yaml")

		if _, err := os.Stat(defaultManifest); err == nil {
			// Found manifest in default location - auto-fix the config
			globalConf.NodeConfigDir = defaultNodeDir

			// Read existing config to preserve other fields. A successful
			// unmarshal of an empty/whitespace/null file yields a nil map (e.g.
			// when the config was truncated by a disk-full event), so guard
			// against nil before writing or the assignment below panics.
			var config map[string]interface{}
			if err := yaml.Unmarshal(globalConfigData, &config); err != nil || config == nil {
				config = make(map[string]interface{})
			}
			config["node_config_dir"] = defaultNodeDir

			// Write back
			if newData, err := yaml.Marshal(config); err == nil {
				_ = os.WriteFile(globalConfigFile, newData, 0600)
			}
		} else {
			return nil, "", fmt.Errorf("global config %s is invalid: missing 'node_config_dir'", globalConfigFile)
		}
	}

	// Step 2: Load the manifest from the path specified in the global config.
	nodeConfigDir := globalConf.NodeConfigDir
	manifestPath := filepath.Join(nodeConfigDir, "citadel.yaml")

	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("manifest not found at %s. Run 'citadel init' to regenerate the configuration", manifestPath)
		}
		return nil, "", fmt.Errorf("could not read manifest from global path %s: %w", manifestPath, err)
	}

	var manifest CitadelManifest
	if err := yaml.Unmarshal(manifestData, &manifest); err != nil {
		return nil, "", fmt.Errorf("could not parse manifest from global path %s: %w", manifestPath, err)
	}

	// Return the manifest and the absolute path to its directory.
	return &manifest, nodeConfigDir, nil
}

// readManifestFromDir loads citadel.yaml directly from configDir, bypassing the
// global config.yaml indirection entirely. This is the --node-dir/
// CITADEL_NODE_DIR override path (citadel#853): a caller that wants to target
// an explicit node directory without depending on $HOME or platform.ConfigDir()
// gets EXACTLY that directory, with no auto-fix/fallback behavior layered on
// top (unlike the default path above, which self-heals a missing
// node_config_dir key).
func readManifestFromDir(configDir string) (*CitadelManifest, string, error) {
	manifestPath := filepath.Join(configDir, "citadel.yaml")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("manifest not found at %s (--node-dir/CITADEL_NODE_DIR override). Run 'citadel init' there, or drop the override to use the default location", manifestPath)
		}
		return nil, "", fmt.Errorf("could not read manifest from %s: %w", manifestPath, err)
	}

	var manifest CitadelManifest
	if err := yaml.Unmarshal(manifestData, &manifest); err != nil {
		return nil, "", fmt.Errorf("could not parse manifest from %s: %w", manifestPath, err)
	}
	return &manifest, configDir, nil
}

// findOrCreateManifest returns the manifest if it exists, or creates a bootstrap
// configuration if it doesn't. This enables `citadel run` to work without `citadel init`.
//
// EXCEPTION (citadel#853): when --node-dir/CITADEL_NODE_DIR is set, a missing
// manifest is bootstrapped AT the override directory instead of
// $HOME/citadel-node, and the machine-wide global config.yaml is left
// untouched -- an override is a one-off target (a test, an agent's isolated
// probe), not a new permanent default for every future un-overridden
// invocation on this machine.
func findOrCreateManifest() (*CitadelManifest, string, error) {
	// Try to find existing manifest
	manifest, configDir, err := findAndReadManifest()
	if err == nil {
		return manifest, configDir, nil
	}

	override := resolveNodeDirOverride()
	if override != "" {
		configDir = override
	} else {
		homeDir, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return nil, "", fmt.Errorf("failed to get home directory: %w", homeErr)
		}
		configDir = filepath.Join(homeDir, "citadel-node")
	}
	servicesDir := filepath.Join(configDir, "services")
	manifestPath := filepath.Join(configDir, "citadel.yaml")

	// Create directories
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		return nil, "", fmt.Errorf("failed to create config directory: %w", err)
	}

	// Get hostname for node name
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "citadel-node"
	}

	// Create minimal manifest
	manifest = &CitadelManifest{
		Node: struct {
			Name  string   `yaml:"name"`
			Tags  []string `yaml:"tags"`
			OrgID string   `yaml:"org_id,omitempty"`
		}{
			Name: hostname,
			Tags: []string{},
		},
		Services: []Service{},
	}

	// Write manifest
	if err := writeManifest(manifestPath, manifest); err != nil {
		return nil, "", err
	}

	// Only point the machine-wide global config at this dir when there is no
	// override active -- see the function doc comment above.
	if override == "" {
		if err := writeGlobalConfig(configDir); err != nil {
			return nil, "", err
		}
	}

	fmt.Printf("✅ Created new configuration at %s\n", configDir)
	return manifest, configDir, nil
}

// writeManifest writes the manifest to disk.
func writeManifest(path string, manifest *CitadelManifest) error {
	yamlData, err := yaml.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}
	if err := os.WriteFile(path, yamlData, 0600); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}
	return nil
}

// writeGlobalConfig creates the global config file pointing to the node's config directory.
func writeGlobalConfig(nodeConfigDir string) error {
	globalConfigDir := platform.ConfigDir()
	globalConfigFile := filepath.Join(globalConfigDir, "config.yaml")

	if err := os.MkdirAll(globalConfigDir, 0755); err != nil {
		return fmt.Errorf("failed to create global config directory %s: %w", globalConfigDir, err)
	}

	configContent := fmt.Sprintf("node_config_dir: %s\n", nodeConfigDir)
	if err := os.WriteFile(globalConfigFile, []byte(configContent), 0600); err != nil {
		return fmt.Errorf("failed to write global config file %s: %w", globalConfigFile, err)
	}
	return nil
}

// hasService checks if a service is already in the manifest.
func hasService(manifest *CitadelManifest, serviceName string) bool {
	for _, s := range manifest.Services {
		if s.Name == serviceName {
			return true
		}
	}
	return false
}

// addServiceToManifest adds a service to the manifest and writes it to disk,
// honoring the hardcoded capability-tag map for embedded/catalog services.
func addServiceToManifest(configDir, serviceName string) error {
	return addServiceToManifestWithTags(configDir, serviceName, nil)
}

// addServiceToManifestWithTags adds a service to the manifest and writes it to
// disk. In addition to the hardcoded capability-tag map (back-compat for
// embedded/catalog services), it merges any module-declared routing tags
// (service.yaml's node_tags) into Node.Tags, so third-party engines become
// routable without a CLI change. Tags are deduped via containsTag.
func addServiceToManifestWithTags(configDir, serviceName string, nodeTags []string) error {
	manifestPath := filepath.Join(configDir, "citadel.yaml")

	// Read existing manifest
	manifest, _, err := findAndReadManifest()
	if err != nil {
		return fmt.Errorf("failed to read manifest: %w", err)
	}

	// Check if already exists
	if hasService(manifest, serviceName) {
		return nil // Already present
	}

	// Add new service
	manifest.Services = append(manifest.Services, Service{
		Name:        serviceName,
		ComposeFile: filepath.Join("./services", serviceName+".yml"),
	})

	// Auto-add capability tags for specific embedded/catalog services (back-compat).
	serviceTags := map[string][]string{
		"extraction": {"extraction:gliner2", "model:gliner2-base-v1"},
	}
	if tags, ok := serviceTags[serviceName]; ok {
		for _, tag := range tags {
			if !containsTag(manifest.Node.Tags, tag) {
				manifest.Node.Tags = append(manifest.Node.Tags, tag)
			}
		}
	}

	// Merge module-declared routing tags (service.yaml node_tags).
	for _, tag := range nodeTags {
		if tag != "" && !containsTag(manifest.Node.Tags, tag) {
			manifest.Node.Tags = append(manifest.Node.Tags, tag)
		}
	}

	// Write back
	return writeManifest(manifestPath, manifest)
}

// removeServiceFromManifest removes a service from the node manifest by name and
// writes it back. It is the de-registration half of an uninstall (the compose
// teardown + lockfile/lock-file cleanup are the caller's responsibility). It is
// idempotent: removing a service that is not present rewrites the manifest
// unchanged and returns nil, so a re-run of an uninstall converges cleanly.
func removeServiceFromManifest(configDir, serviceName string) error {
	manifestPath := filepath.Join(configDir, "citadel.yaml")
	manifest, _, err := findAndReadManifest()
	if err != nil {
		return fmt.Errorf("failed to read manifest: %w", err)
	}
	kept := make([]Service, 0, len(manifest.Services))
	for _, s := range manifest.Services {
		if s.Name == serviceName {
			continue
		}
		kept = append(kept, s)
	}
	manifest.Services = kept

	// Symmetric tag cleanup (#514). Install adds a module's node_tags to
	// Node.Tags (addServiceToManifestWithTags), but uninstall historically left
	// them behind, so a node kept advertising a capability tag (e.g. `meeting`)
	// for a module it no longer runs. Best-effort: strip the removed module's
	// declared node_tags. The capability DETECTOR is the worker's source of truth
	// (it re-derives tags at startup), so this mainly keeps `citadel status` and
	// generic routing honest; a missing catalog manifest just skips the cleanup.
	if mod, mErr := catalog.LoadServiceManifest(serviceName); mErr == nil && len(mod.NodeTags) > 0 {
		manifest.Node.Tags = stripTags(manifest.Node.Tags, mod.NodeTags)
	}
	return writeManifest(manifestPath, manifest)
}

// stripTags returns tags with every entry in remove filtered out, preserving
// order. Pure so the uninstall tag cleanup is unit-testable.
func stripTags(tags, remove []string) []string {
	if len(remove) == 0 {
		return tags
	}
	drop := make(map[string]struct{}, len(remove))
	for _, t := range remove {
		drop[t] = struct{}{}
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if _, ok := drop[t]; ok {
			continue
		}
		out = append(out, t)
	}
	return out
}

// setServiceDesiredStatus sets (or clears, when status is "") the per-service
// boot marker used to make a remote "stopped" durable. Setting "stopped" makes
// the boot-time service-start paths skip the service (serviceStartDisabled);
// clearing it (status == "") restores start-on-boot. Returns an error if the
// service is not present so a caller does not silently no-op on a typo'd name.
func setServiceDesiredStatus(configDir, serviceName, status string) error {
	manifestPath := filepath.Join(configDir, "citadel.yaml")
	manifest, _, err := findAndReadManifest()
	if err != nil {
		return fmt.Errorf("failed to read manifest: %w", err)
	}
	found := false
	for i := range manifest.Services {
		if manifest.Services[i].Name == serviceName {
			manifest.Services[i].DesiredStatus = status
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("service %q not found in manifest", serviceName)
	}
	return writeManifest(manifestPath, manifest)
}

// containsTag checks if a tag is already in the tags slice.
func containsTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// ensureComposeFile ensures the compose file exists in the services directory.
// If it doesn't exist, it extracts the embedded compose file from the binary.
//
// citadel#860: under an active --node-dir/CITADEL_NODE_DIR override, the
// materialized file's `container_name: citadel-<serviceName>` line is
// rewritten to the override-namespaced name embeddedContainerName derives
// (citadel-<hash>-<serviceName>), so two override dirs materializing the same
// service never collide on `up`. No override -> content is written/left
// verbatim, byte-identical to pre-#860. This reconciliation runs on EVERY
// call, not just first-write -- see ensureNamespacedContainerNameOnDisk for
// why the "file already exists, leave it alone" fast path below cannot skip
// it.
func ensureComposeFile(configDir, serviceName string) error {
	servicesDir := filepath.Join(configDir, "services")
	destPath := filepath.Join(servicesDir, serviceName+".yml")

	// Check if file already exists
	if _, err := os.Stat(destPath); err == nil {
		if err := ensureNamespacedContainerNameOnDisk(destPath, serviceName); err != nil {
			return err
		}
		// Still ensure any build-context aux files exist (idempotent), so a
		// build-based service like bonsai is startable even if its .yml was
		// materialized by an older binary that predated WriteAuxFiles.
		return services.WriteAuxFiles(servicesDir, serviceName)
	}

	// Get content from embedded services
	content, ok := services.ServiceMap[serviceName]
	if !ok {
		return fmt.Errorf("unknown service: %s", serviceName)
	}

	// citadel#860: namespace container_name under an active override. Only
	// EMBEDDED services (services.ServiceMap, confirmed above) are templated
	// this way -- catalog/third-party modules author their own container_name
	// and are out of scope (see cmd/nodedir.go's package doc). Shares
	// compose.EnsureNamespacedContainerName with the existing-file branch
	// above (not a second ad hoc rewrite) so both paths apply the identical
	// rule.
	if override := resolveNodeDirOverride(); override != "" {
		rewritten, _, err := compose.EnsureNamespacedContainerName(content, serviceName, override)
		if err != nil {
			return fmt.Errorf("namespace container_name for %q under --node-dir override: %w", serviceName, err)
		}
		content = rewritten
	}

	// Ensure services directory exists
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		return fmt.Errorf("failed to create services directory: %w", err)
	}

	// Write compose file (0600 to protect any sensitive env vars)
	if err := os.WriteFile(destPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed to write compose file: %w", err)
	}

	// Materialize any build-context files the service needs (e.g. bonsai's
	// Dockerfile), so `docker compose build` resolves on the node.
	return services.WriteAuxFiles(servicesDir, serviceName)
}

// ensureNamespacedContainerNameOnDisk re-checks (and if needed rewrites) an
// ALREADY-MATERIALIZED embedded compose file's container_name against the
// active --node-dir/CITADEL_NODE_DIR override, if any.
//
// Without this, ensureComposeFile's "the .yml already exists, leave it alone"
// fast path (the common case on any node that has run before) would leave a
// file materialized BEFORE #860 shipped -- or materialized by this same
// binary before an override was ever pointed at this dir -- carrying the
// UNnamespaced `container_name: citadel-<svc>` forever. That's not just a
// cosmetic drift from what embeddedContainerName now returns: a later
// `citadel run`/`module start --node-dir <this dir>` would `up` that stale
// file, and #856's compose "-p" project scoping only makes that "safe and
// loud" (a cross-project container_name conflict) WHEN the real node's
// citadel-<svc> container currently exists. If it's stopped/absent, the `up`
// SUCCEEDS -- silently annexing the real node's global container name under
// the override's compose project. Reconciling on every call closes that gap
// (see compose.EnsureNamespacedContainerName's doc for the exact rule,
// including its loud refusal on a hand-edited or differently-namespaced
// file). A no-op, including the read, when no override is active.
func ensureNamespacedContainerNameOnDisk(destPath, serviceName string) error {
	override := resolveNodeDirOverride()
	if override == "" {
		return nil
	}
	// SCOPE (citadel#860's non-goal): ensureComposeFile is also the
	// materialization choke point for manifest-declared, NON-embedded
	// services (e.g. a catalog-installed module whose .yml already exists on
	// disk) -- serviceIsKnown/knownServiceNames (cmd/run.go) accepts both. A
	// catalog module authors its own container_name; it is not
	// "citadel-<name>" by convention, so probing for that pattern would
	// either false-negative-refuse (the loud error in
	// compose.EnsureNamespacedContainerName) or, worse, coincidentally match
	// and rewrite something this issue does not own. Only reconcile when
	// serviceName is an actual services.ServiceMap entry.
	if !isEmbeddedService(serviceName) {
		return nil
	}
	existing, err := os.ReadFile(destPath)
	if err != nil {
		return fmt.Errorf("read materialized compose file for %q: %w", serviceName, err)
	}
	rewritten, changed, err := compose.EnsureNamespacedContainerName(string(existing), serviceName, override)
	if err != nil {
		return fmt.Errorf("namespace container_name for %q under --node-dir override: %w", serviceName, err)
	}
	if !changed {
		return nil
	}
	return os.WriteFile(destPath, []byte(rewritten), 0600)
}
