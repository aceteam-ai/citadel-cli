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
}

// CurrentSchemaVersion is the highest service.yaml schema major version this CLI
// understands. A manifest declaring a higher value is still loaded (best-effort)
// but the operator is warned it may use fields this CLI ignores.
const CurrentSchemaVersion = 1

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

// IsAvailable returns true if the catalog has been cloned locally.
func IsAvailable() bool {
	info, err := os.Stat(GetCatalogPath())
	return err == nil && info.IsDir()
}

// Update clones or pulls the catalog repository into the local cache.
func Update() error {
	catalogPath := GetCatalogPath()

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(catalogPath), 0755); err != nil {
		return fmt.Errorf("failed to create catalog parent directory: %w", err)
	}

	if isGitRepo(catalogPath) {
		// Pull latest changes.
		cmd := exec.Command("git", "-C", catalogPath, "pull", "--ff-only")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git pull failed: %s", strings.TrimSpace(string(output)))
		}
		return nil
	}

	// Fresh clone. Remove any leftover non-git directory first.
	if err := os.RemoveAll(catalogPath); err != nil {
		return fmt.Errorf("failed to clean catalog directory: %w", err)
	}

	cmd := exec.Command("git", "clone", "--depth", "1", DefaultCatalogURL, catalogPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

// LoadRegistry reads the registry.yaml index file.
// If registry.yaml is absent, it falls back to scanning service subdirectories.
func LoadRegistry() (*Registry, error) {
	catalogPath := GetCatalogPath()
	if !IsAvailable() {
		return nil, fmt.Errorf("catalog not found. Run 'citadel service catalog update' first")
	}

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

// LoadServiceManifest reads a specific service's service.yaml.
func LoadServiceManifest(name string) (*ServiceManifest, error) {
	catalogPath := GetCatalogPath()
	manifestPath := filepath.Join(catalogPath, name, "service.yaml")

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("service '%s' not found in catalog", name)
		}
		return nil, fmt.Errorf("failed to read service manifest: %w", err)
	}

	var manifest ServiceManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("failed to parse service manifest for '%s': %w", name, err)
	}
	return &manifest, nil
}

// GetComposeFile returns the path to a service's compose.yml inside the catalog.
func GetComposeFile(name string) (string, error) {
	catalogPath := GetCatalogPath()
	composePath := filepath.Join(catalogPath, name, "compose.yml")

	if _, err := os.Stat(composePath); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("compose.yml not found for service '%s'", name)
		}
		return "", fmt.Errorf("failed to access compose file: %w", err)
	}
	return composePath, nil
}

// Search filters services by a query string, matching against name, tags, category,
// and description (case-insensitive).
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

// scanForServices scans the catalog directory for service.yaml files when registry.yaml
// is absent. Each immediate subdirectory containing a service.yaml becomes an entry.
func scanForServices(catalogPath string) (*Registry, error) {
	entries, err := os.ReadDir(catalogPath)
	if err != nil {
		return nil, fmt.Errorf("failed to scan catalog directory: %w", err)
	}

	var reg Registry
	reg.Version = 1

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		manifestPath := filepath.Join(catalogPath, entry.Name(), "service.yaml")
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
