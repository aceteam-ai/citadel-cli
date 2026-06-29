// Package catalog manages the local cache of the citadel-services catalog repository
// and provides functions for reading service manifests and registry entries.
package catalog

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"gopkg.in/yaml.v3"
)

const (
	// DefaultCatalogURL is the git URL for the official service catalog.
	DefaultCatalogURL = "https://github.com/aceteam-ai/citadel-services.git"
	// catalogSubdir is the subdirectory within the config dir for the catalog cache.
	catalogSubdir = "catalog"
	// servicesSubdir is the subdirectory within the catalog repo that holds the
	// per-service directories (e.g. services/vllm/service.yaml). The catalog repo
	// (aceteam-ai/citadel-services) stores services here, alongside a top-level
	// registry.yaml index.
	servicesSubdir = "services"
)

// Registry is the top-level index of available services (registry.yaml).
type Registry struct {
	Version  int             `yaml:"version"`
	Services []RegistryEntry `yaml:"services"`
}

// RegistryEntry is a summary of a single service in the registry index.
type RegistryEntry struct {
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Category    string   `yaml:"category"`
	GPU         string   `yaml:"gpu"` // "required", "optional", "no"
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags,omitempty"`
	// Source is the name of the catalog source this entry came from. It is not
	// read from registry.yaml; the aggregation layer stamps it during a
	// cross-source load so list/search/info can disambiguate collisions.
	Source string `yaml:"-"`
}

// ServiceManifest is the full definition of a service (service.yaml inside a service dir).
type ServiceManifest struct {
	// SchemaVersion is the manifest schema major version. A zero/absent value
	// means schema v1. A value newer than CurrentSchemaVersion triggers a
	// forward-compat warning (but never a hard failure).
	SchemaVersion int           `yaml:"schema_version"`
	Name          string        `yaml:"name"`
	Version       string        `yaml:"version"`
	Description   string        `yaml:"description"`
	Category      string        `yaml:"category"`
	Author        string        `yaml:"author"`
	License       string        `yaml:"license"`
	Homepage      string        `yaml:"homepage"`
	Requires      Requirements  `yaml:"requires"`
	Ports         []PortMapping `yaml:"ports"`
	Config        []ConfigVar   `yaml:"config"`
	HealthCheck   HealthCheck   `yaml:"health_check"`
	Volumes       []VolumeMount `yaml:"volumes"`
	// Tags are display/search tags (free-form), used by `catalog list/search`.
	Tags []string `yaml:"tags"`
	// NodeTags are namespaced key:value routing tags (e.g. "engine:tei",
	// "task:embedding", "model:gte-multilingual-base") that are merged into the
	// node manifest's Node.Tags on install so third-party engines become
	// routable without a CLI change. Distinct from the display Tags above.
	NodeTags []string `yaml:"node_tags"`
	// Sandbox declares the least-privilege needs of an untrusted (Tier 2) module.
	// It is OPTIONAL: a module with no sandbox: block gets the strict defaults
	// (all caps dropped, no-new-privileges, read-only rootfs, conservative
	// resource limits). The installer grants exactly what is declared here and
	// drops everything else. Trusted (Tier 0/1) modules ignore this entirely and
	// run as-is. See internal/catalog/sandbox.go.
	Sandbox SandboxSpec `yaml:"sandbox"`
}

// CurrentSchemaVersion is the highest service.yaml schema major version this CLI
// understands. A manifest declaring a higher value is still loaded (best-effort)
// but the operator is warned it may use fields this CLI ignores.
//
// v2 adds the optional `sandbox:` block (declarative least-privilege needs for
// untrusted modules). It is purely additive -- a v1 manifest is fully valid.
const CurrentSchemaVersion = 2

// SandboxSpec is the optional declarative least-privilege block of a module
// manifest. Every field is optional; an absent field means "strict default".
type SandboxSpec struct {
	// Capabilities are the Linux capabilities to KEEP (added back via cap_add
	// after cap_drop: ALL). Names may be given with or without the "CAP_" prefix
	// (e.g. "NET_BIND_SERVICE" or "CAP_NET_BIND_SERVICE"); they are emitted
	// verbatim. Empty means no capabilities are kept.
	Capabilities []string `yaml:"capabilities"`
	// Devices are host devices the module legitimately needs (compose `devices:`
	// entries, e.g. "/dev/snd"). Empty means none.
	Devices []string `yaml:"devices"`
	// WritablePaths are in-container paths that must be writable. With a read-only
	// rootfs, each becomes a tmpfs mount so the module can write there. "/tmp" is
	// always writable regardless.
	WritablePaths []string `yaml:"writable_paths"`
	// HostNetwork opts the module into host networking. Default false: host
	// networking is NOT granted unless explicitly declared (and it independently
	// trips the #342 risk scan).
	HostNetwork bool `yaml:"host_network"`
	// Resources declares cgroup limits. Unset fields fall back to conservative
	// defaults in GenerateHardeningOverride.
	Resources SandboxResources `yaml:"resources"`
}

// SandboxResources declares per-module cgroup limits. Zero/empty values fall
// back to conservative defaults.
type SandboxResources struct {
	// CPU is the cpus limit (compose `cpus:`, e.g. "2.0"). Empty -> default.
	CPU string `yaml:"cpu"`
	// Memory is the memory limit (compose `mem_limit:`, e.g. "2g"). Empty -> default.
	Memory string `yaml:"memory"`
	// PIDs is the pids_limit. Zero -> default.
	PIDs int `yaml:"pids"`
}

// Requirements describes what a service needs from the host.
type Requirements struct {
	GPU       bool     `yaml:"gpu"`
	VRAMMinGB float64  `yaml:"vram_min_gb"`
	Arch      []string `yaml:"arch"`
}

// PortMapping describes a port exposed by the service container.
type PortMapping struct {
	Host        int    `yaml:"host"`
	Container   int    `yaml:"container"`
	Protocol    string `yaml:"protocol"`
	Description string `yaml:"description"`
}

// ConfigVar describes a user-configurable environment variable.
type ConfigVar struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Default     string `yaml:"default"`
	Required    bool   `yaml:"required"`
}

// HealthCheck describes how to probe the service for readiness.
type HealthCheck struct {
	Endpoint string `yaml:"endpoint"`
	Port     int    `yaml:"port"`
	Interval string `yaml:"interval"`
	Timeout  string `yaml:"timeout"`
	Retries  int    `yaml:"retries"`
}

// VolumeMount describes a bind mount from host to container.
type VolumeMount struct {
	Name        string `yaml:"name"`
	Host        string `yaml:"host"`
	Container   string `yaml:"container"`
	Description string `yaml:"description"`
}

// GetCatalogPath returns the local catalog cache directory.
// It uses platform.ConfigDir() so that root/sudo/Windows are handled correctly.
func GetCatalogPath() string {
	return filepath.Join(platform.ConfigDir(), catalogSubdir)
}

// IsAvailable returns true if the default catalog has been cloned locally.
func IsAvailable() bool {
	return dirIsAvailable(GetCatalogPath())
}

// dirIsAvailable returns true if path exists and is a directory.
func dirIsAvailable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Update clones or pulls EVERY configured catalog source: the built-in default
// plus any user-added sources. A failure to update one source does not abort the
// others; the first error encountered is returned after all sources are tried,
// so a single broken community repo never blocks refreshing the rest.
func Update() error {
	sources, err := resolvedSources()
	if err != nil {
		return err
	}

	var firstErr error
	for _, src := range sources {
		if err := updateSource(src); err != nil {
			wrapped := fmt.Errorf("catalog source %q: %w", src.Name, err)
			if firstErr == nil {
				firstErr = wrapped
			}
		}
	}
	return firstErr
}

// updateSource clones or pulls a single source's git repo into its cache path.
func updateSource(src resolvedSource) error {
	// Resolve the clone URL (expands the owner/repo shorthand for added sources).
	cloneURL := src.URL
	if !src.Default {
		resolved, err := resolveSourceURL(src.URL)
		if err != nil {
			return err
		}
		cloneURL = resolved
	}

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(src.Path), 0755); err != nil {
		return fmt.Errorf("failed to create catalog parent directory: %w", err)
	}

	if isGitRepo(src.Path) {
		// Pull latest changes.
		cmd := exec.Command("git", "-C", src.Path, "pull", "--ff-only")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git pull failed: %s", strings.TrimSpace(string(output)))
		}
		return nil
	}

	// Fresh clone. Remove any leftover non-git directory first.
	if err := os.RemoveAll(src.Path); err != nil {
		return fmt.Errorf("failed to clean catalog directory: %w", err)
	}

	cmd := exec.Command("git", "clone", "--depth", "1", cloneURL, src.Path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

// LoadRegistry returns the merged registry across every configured catalog
// source (the built-in default plus any user-added sources). Each entry's Source
// field is stamped with the owning source's name. On a name collision the source
// listed first wins -- the default source, then user-added sources in
// registration order. If no source has been cloned yet, it returns a
// catalog-not-found error pointing the operator at `catalog update`.
func LoadRegistry() (*Registry, error) {
	sources, err := resolvedSources()
	if err != nil {
		return nil, err
	}

	var regs []sourceRegistry
	anyAvailable := false
	for _, src := range sources {
		if !dirIsAvailable(src.Path) {
			continue
		}
		anyAvailable = true
		reg, err := loadRegistryFromPath(src.Path)
		if err != nil {
			// A single malformed community source must not take down the whole
			// catalog (incl. the default). Skip it and warn -- mirroring the
			// per-source resilience of Update().
			fmt.Fprintf(os.Stderr, "warning: skipping catalog source %q: %v\n", src.Name, err)
			continue
		}
		regs = append(regs, sourceRegistry{Source: src.Name, Services: reg.Services})
	}

	if !anyAvailable {
		return nil, fmt.Errorf("catalog not found. Run 'citadel service catalog update' first")
	}

	return &Registry{Version: 1, Services: mergeRegistries(regs)}, nil
}

// loadRegistryFromPath reads the registry.yaml index from a single source's
// cache path, falling back to scanning service subdirectories when absent.
func loadRegistryFromPath(catalogPath string) (*Registry, error) {
	registryPath := filepath.Join(catalogPath, "registry.yaml")
	data, err := os.ReadFile(registryPath)
	if err == nil {
		var reg Registry
		if err := yaml.Unmarshal(data, &reg); err != nil {
			return nil, fmt.Errorf("failed to parse registry.yaml: %w", err)
		}
		return &reg, nil
	}

	// Fallback: scan subdirectories for service.yaml files.
	return scanForServices(catalogPath)
}

// LoadServiceManifest reads a specific service's service.yaml, searching across
// every configured catalog source. On a name collision the default source wins,
// then user-added sources in registration order (matching LoadRegistry).
func LoadServiceManifest(name string) (*ServiceManifest, error) {
	sources, err := resolvedSources()
	if err != nil {
		return nil, err
	}
	for _, src := range sources {
		manifest, err := loadManifestFromPath(src.Path, name)
		if err == nil {
			return manifest, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("service '%s' not found in catalog", name)
}

// loadManifestFromPath reads a service's service.yaml from a single source's
// cache path. A missing manifest is reported as an os.IsNotExist error so the
// cross-source loader can keep searching the next source.
func loadManifestFromPath(catalogPath, name string) (*ServiceManifest, error) {
	manifestPath := filepath.Join(catalogPath, servicesSubdir, name, "service.yaml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err
		}
		return nil, fmt.Errorf("failed to read service manifest: %w", err)
	}

	var manifest ServiceManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("failed to parse service manifest for '%s': %w", name, err)
	}
	return &manifest, nil
}

// GetComposeFile returns the path to a service's compose.yml, searching across
// every configured catalog source with the same precedence as LoadRegistry.
func GetComposeFile(name string) (string, error) {
	sources, err := resolvedSources()
	if err != nil {
		return "", err
	}
	for _, src := range sources {
		composePath := filepath.Join(src.Path, servicesSubdir, name, "compose.yml")
		if _, err := os.Stat(composePath); err == nil {
			return composePath, nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("failed to access compose file: %w", err)
		}
	}
	return "", fmt.Errorf("compose.yml not found for service '%s'", name)
}

// Search filters services by a query string, matching against name, tags, category,
// and description (case-insensitive). It searches across all configured sources.
func Search(query string) ([]RegistryEntry, error) {
	reg, err := LoadRegistry()
	if err != nil {
		return nil, err
	}

	q := strings.ToLower(query)
	var results []RegistryEntry

	for _, entry := range reg.Services {
		if matchesQuery(entry, q) {
			results = append(results, entry)
		}
	}
	return results, nil
}

// CheckGPU checks whether an NVIDIA GPU is available and returns VRAM in GB.
func CheckGPU() (hasGPU bool, vramGB float64, err error) {
	cmd := exec.Command("nvidia-smi", "--query-gpu=memory.total", "--format=csv,noheader,nounits")
	output, err := cmd.Output()
	if err != nil {
		return false, 0, nil // nvidia-smi not found or failed -- no GPU
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 {
		return false, 0, nil
	}

	// Sum VRAM across all GPUs.
	var totalMB float64
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var mb float64
		if _, err := fmt.Sscanf(line, "%f", &mb); err == nil {
			totalMB += mb
		}
	}

	if totalMB > 0 {
		return true, totalMB / 1024.0, nil
	}
	return false, 0, nil
}

// CheckPortConflict returns true if the given port is already in use.
func CheckPortConflict(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return true // port is in use
	}
	ln.Close()
	return false
}

// CheckArchCompatible returns true if the current architecture matches any in the list.
// An empty list means any architecture is acceptable.
func CheckArchCompatible(archList []string) bool {
	if len(archList) == 0 {
		return true
	}
	current := runtime.GOARCH
	for _, a := range archList {
		if a == current {
			return true
		}
	}
	return false
}

// --- internal helpers ---

func isGitRepo(path string) bool {
	info, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && info.IsDir()
}

func matchesQuery(entry RegistryEntry, query string) bool {
	if strings.Contains(strings.ToLower(entry.Name), query) {
		return true
	}
	if strings.Contains(strings.ToLower(entry.Category), query) {
		return true
	}
	if strings.Contains(strings.ToLower(entry.Description), query) {
		return true
	}
	for _, tag := range entry.Tags {
		if strings.Contains(strings.ToLower(tag), query) {
			return true
		}
	}
	return false
}

// scanForServices scans the catalog's services directory for service.yaml files
// when registry.yaml is absent. Each immediate subdirectory of <catalog>/services
// containing a service.yaml becomes an entry.
func scanForServices(catalogPath string) (*Registry, error) {
	servicesPath := filepath.Join(catalogPath, servicesSubdir)
	entries, err := os.ReadDir(servicesPath)
	if err != nil {
		return nil, fmt.Errorf("failed to scan catalog directory: %w", err)
	}

	var reg Registry
	reg.Version = 1

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		manifestPath := filepath.Join(servicesPath, entry.Name(), "service.yaml")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue // skip directories without service.yaml
		}

		var manifest ServiceManifest
		if err := yaml.Unmarshal(data, &manifest); err != nil {
			continue // skip malformed manifests
		}

		gpuStr := "no"
		if manifest.Requires.GPU {
			gpuStr = "required"
		}

		reg.Services = append(reg.Services, RegistryEntry{
			Name:        manifest.Name,
			Version:     manifest.Version,
			Category:    manifest.Category,
			GPU:         gpuStr,
			Description: manifest.Description,
			Tags:        manifest.Tags,
		})
	}

	return &reg, nil
}
