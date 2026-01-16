// Package jobs provides job handlers for the Citadel worker.
//
// This file implements the APPLY_DEVICE_CONFIG job handler which applies
// device configuration received from the Python worker after device authorization.
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

// serviceNamePattern validates service names to prevent path traversal.
// Only allows lowercase alphanumeric characters and hyphens.
var serviceNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// DeviceConfig represents the configuration received from the onboarding wizard.
type DeviceConfig struct {
	DeviceName              string   `json:"deviceName"`
	Services                []string `json:"services"`
	AutoStartServices       bool     `json:"autoStartServices"`
	SSHEnabled              bool     `json:"sshEnabled"`
	CustomTags              []string `json:"customTags"`
	HealthMonitoringEnabled bool     `json:"healthMonitoringEnabled"`
	AlertOnOffline          bool     `json:"alertOnOffline"`
	AlertOnHighTemp         bool     `json:"alertOnHighTemp"`
}

// ConfigHandler handles APPLY_DEVICE_CONFIG jobs.
// It implements the jobs.JobHandler interface.
type ConfigHandler struct {
	// ConfigDir is the directory where citadel configuration is stored.
	// If empty, defaults to ~/citadel-node
	ConfigDir string
}

// NewConfigHandler creates a new config handler.
func NewConfigHandler(configDir string) *ConfigHandler {
	return &ConfigHandler{
		ConfigDir: configDir,
	}
}

// Execute applies the device configuration.
// It implements the jobs.JobHandler interface.
func (h *ConfigHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	// Parse configuration from payload
	configJSON, ok := job.Payload["config"]
	if !ok {
		return nil, fmt.Errorf("missing 'config' field in job payload")
	}

	var config DeviceConfig
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return nil, fmt.Errorf("failed to parse config JSON: %w", err)
	}

	// Determine config directory
	configDir := h.ConfigDir
	if configDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get home directory: %w", err)
		}
		configDir = filepath.Join(homeDir, "citadel-node")
	}

	// Update manifest with new configuration
	if err := h.updateManifest(configDir, &config); err != nil {
		return nil, fmt.Errorf("failed to update manifest: %w", err)
	}

	result := fmt.Sprintf("Manifest updated successfully for device '%s'", config.DeviceName)

	// Start services if autoStartServices is enabled
	if config.AutoStartServices && len(config.Services) > 0 {
		if err := h.startServices(configDir, config.Services); err != nil {
			return []byte(result + "\nFailed to start services: " + err.Error()), err
		}
		result += fmt.Sprintf("\nStarted %d service(s): %v", len(config.Services), config.Services)
	}

	return []byte(result), nil
}

// CitadelManifest represents the citadel.yaml configuration file.
type CitadelManifest struct {
	Node struct {
		Name string   `yaml:"name"`
		Tags []string `yaml:"tags"`
	} `yaml:"node"`
	Services []ManifestService `yaml:"services,omitempty"`
	Config   ManifestConfig    `yaml:"config,omitempty"`
}

// ManifestService represents a service entry in the manifest.
type ManifestService struct {
	Name        string `yaml:"name"`
	ComposeFile string `yaml:"compose_file"`
}

// ManifestConfig represents additional configuration in the manifest.
type ManifestConfig struct {
	SSHEnabled              bool `yaml:"ssh_enabled,omitempty"`
	HealthMonitoringEnabled bool `yaml:"health_monitoring_enabled,omitempty"`
	AlertOnOffline          bool `yaml:"alert_on_offline,omitempty"`
	AlertOnHighTemp         bool `yaml:"alert_on_high_temp,omitempty"`
}

// updateManifest updates the citadel.yaml with the new configuration.
func (h *ConfigHandler) updateManifest(configDir string, config *DeviceConfig) error {
	manifestPath := filepath.Join(configDir, "citadel.yaml")

	// Read existing manifest or create new one
	var manifest CitadelManifest
	if data, err := os.ReadFile(manifestPath); err == nil {
		if err := yaml.Unmarshal(data, &manifest); err != nil {
			return fmt.Errorf("failed to parse existing manifest: %w", err)
		}
	}

	// Update node name if provided
	if config.DeviceName != "" {
		manifest.Node.Name = config.DeviceName
	}

	// Update tags
	if len(config.CustomTags) > 0 {
		// Merge custom tags with existing base tags
		baseTags := []string{"gpu", "provisioned-by-citadel"}
		manifest.Node.Tags = append(baseTags, config.CustomTags...)
	}

	// Update services
	servicesDir := filepath.Join(configDir, "services")
	os.MkdirAll(servicesDir, 0755)

	manifest.Services = nil
	for _, svcName := range config.Services {
		// Validate service name to prevent path traversal
		if !serviceNamePattern.MatchString(svcName) {
			continue // Skip invalid service names
		}

		// Check if service is available in the whitelist
		if _, ok := services.ServiceMap[svcName]; !ok {
			continue // Skip unknown services
		}

		// Write service compose file
		composeFile := filepath.Join(servicesDir, svcName+".yml")
		if content, ok := services.ServiceMap[svcName]; ok {
			if err := os.WriteFile(composeFile, []byte(content), 0644); err != nil {
				return fmt.Errorf("failed to write compose file for %s: %w", svcName, err)
			}
		}

		manifest.Services = append(manifest.Services, ManifestService{
			Name:        svcName,
			ComposeFile: filepath.Join("./services", svcName+".yml"),
		})
	}

	// Update config options
	manifest.Config = ManifestConfig{
		SSHEnabled:              config.SSHEnabled,
		HealthMonitoringEnabled: config.HealthMonitoringEnabled,
		AlertOnOffline:          config.AlertOnOffline,
		AlertOnHighTemp:         config.AlertOnHighTemp,
	}

	// Write updated manifest
	data, err := yaml.Marshal(&manifest)
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}

	if err := os.WriteFile(manifestPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}

	return nil
}

// startServices starts the configured services using docker compose.
func (h *ConfigHandler) startServices(configDir string, serviceNames []string) error {
	servicesDir := filepath.Join(configDir, "services")

	for _, svcName := range serviceNames {
		// Validate service name to prevent path traversal
		if !serviceNamePattern.MatchString(svcName) {
			continue // Skip invalid service names
		}

		composeFile := filepath.Join(servicesDir, svcName+".yml")

		// Check if compose file exists
		if _, err := os.Stat(composeFile); os.IsNotExist(err) {
			continue
		}

		// Start the service
		projectName := fmt.Sprintf("citadel-%s", svcName)
		cmd := exec.Command("docker", "compose",
			"-f", composeFile,
			"-p", projectName,
			"up", "-d",
		)

		// Set working directory for relative paths in compose files
		cmd.Dir = configDir

		// Configure GPU runtime if on Linux
		if platform.IsLinux() {
			cmd.Env = append(os.Environ(), "DOCKER_DEFAULT_RUNTIME=nvidia")
		}

		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to start %s: %w\nOutput: %s", svcName, err, string(output))
		}
	}

	return nil
}

// Ensure ConfigHandler implements JobHandler
var _ JobHandler = (*ConfigHandler)(nil)
