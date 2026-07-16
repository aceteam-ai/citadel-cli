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

	"github.com/aceteam-ai/citadel-cli/internal/compose"
	citadelconfig "github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

// enabledLabel renders a human-readable enabled/disabled label for result lines.
func enabledLabel(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

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
	VNCEnabled              bool     `json:"vncEnabled"`
	VNCPassword             string   `json:"vncPassword,omitempty"`
	// MeetingEnabled is the programmatic path for the `meeting` capability opt-out
	// (aceteam#5098). It is a pointer so an omitted field (nil) leaves the node's
	// persisted toggle untouched — a plain bool would default to false and silently
	// opt every node out the moment any device config is applied. A non-nil value
	// writes the same meeting.yaml the Control Center toggle and detector use.
	MeetingEnabled *bool `json:"meetingEnabled,omitempty"`
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

	// Apply the meeting-capability opt-out (aceteam#5098) when the platform pushed
	// an explicit value. This writes the same persisted meeting.yaml the Control
	// Center toggle and the capability detector read, so the device-config path and
	// the local toggle converge on one effective value (default-on when neither
	// wrote it). Written to platform.ConfigDir() — the per-concern config location,
	// which the detector reads — not h.ConfigDir (the manifest dir).
	if config.MeetingEnabled != nil {
		if err := citadelconfig.SaveMeeting(platform.ConfigDir(), &citadelconfig.Meeting{MeetingEnabled: *config.MeetingEnabled}); err != nil {
			result += fmt.Sprintf("\nWarning: failed to persist meeting toggle: %v", err)
		} else {
			result += fmt.Sprintf("\nMeeting capability %s", enabledLabel(*config.MeetingEnabled))
		}
	}

	// Start services if autoStartServices is enabled
	if config.AutoStartServices && len(config.Services) > 0 {
		if err := h.startServices(configDir, config.Services); err != nil {
			return []byte(result + "\nFailed to start services: " + err.Error()), err
		}
		result += fmt.Sprintf("\nStarted %d service(s): %v", len(config.Services), config.Services)
	}

	// Handle VNC enable/disable (non-fatal: log warning and continue)
	if config.VNCEnabled {
		vncMgr := platform.GetVNCManager()
		if !vncMgr.IsInstalled() {
			if err := vncMgr.Install(); err != nil {
				result += fmt.Sprintf("\nWarning: failed to install VNC server: %v", err)
			}
		}
		if vncMgr.IsInstalled() {
			// Use provided password or generate one
			pw := config.VNCPassword
			if pw == "" {
				generated, err := platform.GenerateVNCPassword()
				if err != nil {
					result += fmt.Sprintf("\nWarning: failed to generate VNC password: %v", err)
				} else {
					pw = generated
				}
			}
			if pw != "" {
				if err := vncMgr.Configure(pw, platform.DefaultVNCPort); err != nil {
					result += fmt.Sprintf("\nWarning: failed to configure VNC: %v", err)
				}
				if err := vncMgr.Start(); err != nil {
					result += fmt.Sprintf("\nWarning: failed to start VNC: %v", err)
				} else {
					result += fmt.Sprintf("\nVNC server enabled on port %d", platform.DefaultVNCPort)
				}
			}
		}
	} else {
		vncMgr := platform.GetVNCManager()
		if vncMgr.IsRunning() {
			if err := vncMgr.Stop(); err != nil {
				result += fmt.Sprintf("\nWarning: failed to stop VNC: %v", err)
			} else {
				result += "\nVNC server disabled"
			}
		}
	}

	return []byte(result), nil
}

// CitadelManifest represents the citadel.yaml configuration file.
//
// It mirrors every field cmd/manifest.go's CitadelManifest models (org_id,
// capabilities, per-service type/port/desired_status): updateManifest
// round-trips the whole document through this struct, so any field missing
// here would be silently DROPPED from citadel.yaml on every
// APPLY_DEVICE_CONFIG (#528). Capabilities is kept as a raw yaml.Node so its
// structure survives without duplicating cmd's types (a VALUE node, not
// *yaml.Node: yaml.v3 does not populate pointer-to-Node fields on decode, so a
// pointer would silently drop the section again).
type CitadelManifest struct {
	Node struct {
		Name  string   `yaml:"name"`
		Tags  []string `yaml:"tags"`
		OrgID string   `yaml:"org_id,omitempty"`
	} `yaml:"node"`
	Services     []ManifestService `yaml:"services,omitempty"`
	Config       ManifestConfig    `yaml:"config,omitempty"`
	Capabilities yaml.Node         `yaml:"capabilities,omitempty"`
}

// ManifestService represents a service entry in the manifest. DesiredStatus is
// the durable stop marker (see cmd/manifest.go Service.DesiredStatus): it must
// round-trip through APPLY_DEVICE_CONFIG or a dashboard config save would
// silently resurrect stopped services on the next boot (#528).
type ManifestService struct {
	Name          string `yaml:"name"`
	Type          string `yaml:"type,omitempty"`
	ComposeFile   string `yaml:"compose_file"`
	Port          int    `yaml:"port,omitempty"`
	DesiredStatus string `yaml:"desired_status,omitempty"`
}

// ManifestConfig represents additional configuration in the manifest.
type ManifestConfig struct {
	SSHEnabled              bool `yaml:"ssh_enabled,omitempty"`
	HealthMonitoringEnabled bool `yaml:"health_monitoring_enabled,omitempty"`
	AlertOnOffline          bool `yaml:"alert_on_offline,omitempty"`
	AlertOnHighTemp         bool `yaml:"alert_on_high_temp,omitempty"`
	VNCEnabled              bool `yaml:"vnc_enabled,omitempty"`
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

	// Update services: MERGE the config's service list into the existing one
	// (#528). The previous implementation did `manifest.Services = nil` and
	// rebuilt from config.Services alone, which (a) wiped every durable
	// desired_status stop marker and (b) dropped every service the onboarding
	// wizard doesn't know about -- most importantly module-installed services
	// (registered via MODULE_SET / `citadel module install`), which would be
	// silently de-registered by any dashboard config save. The wizard config is
	// therefore treated as ADDITIVE for the service list: listed services are
	// materialized and registered if missing, existing entries (and their
	// DesiredStatus/Type/Port) are preserved untouched, and removal remains the
	// job of the explicit uninstall paths (MODULE_SET, `citadel module remove`).
	servicesDir := filepath.Join(configDir, "services")
	os.MkdirAll(servicesDir, 0755)

	existing := make(map[string]bool, len(manifest.Services))
	for _, s := range manifest.Services {
		existing[s.Name] = true
	}
	for _, svcName := range config.Services {
		// Validate service name to prevent path traversal
		if !serviceNamePattern.MatchString(svcName) {
			continue // Skip invalid service names
		}

		// Check if service is available in the whitelist
		if _, ok := services.ServiceMap[svcName]; !ok {
			continue // Skip unknown services
		}

		// Write service compose file (0600 to protect any sensitive env vars)
		composeFile := filepath.Join(servicesDir, svcName+".yml")
		if content, ok := services.ServiceMap[svcName]; ok {
			if err := os.WriteFile(composeFile, []byte(content), 0600); err != nil {
				return fmt.Errorf("failed to write compose file for %s: %w", svcName, err)
			}
		}

		if existing[svcName] {
			continue // Already registered: preserve the entry as-is.
		}
		manifest.Services = append(manifest.Services, ManifestService{
			Name:        svcName,
			ComposeFile: filepath.Join("./services", svcName+".yml"),
		})
		existing[svcName] = true
	}

	// Update config options
	manifest.Config = ManifestConfig{
		SSHEnabled:              config.SSHEnabled,
		HealthMonitoringEnabled: config.HealthMonitoringEnabled,
		AlertOnOffline:          config.AlertOnOffline,
		AlertOnHighTemp:         config.AlertOnHighTemp,
		VNCEnabled:              config.VNCEnabled,
	}

	// Write updated manifest
	data, err := yaml.Marshal(&manifest)
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}

	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
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

		// Transitional (#528): remove any container still under the legacy
		// "citadel-<svcName>" project so the no-`-p` up below does not conflict
		// on the pinned container_name.
		compose.RemoveLegacyProjectContainers("docker", svcName)

		// Start the service. No -p: the default compose project (dir basename,
		// "services") is the standardized convention shared with the boot/run/
		// stop/job paths and the no-`-p` status reads (#528). The old
		// `-p citadel-<name>` here created containers the stop paths could not
		// see.
		cmd := exec.Command("docker", "compose",
			"-f", composeFile,
			"up", "-d",
		)

		// Set working directory for relative paths in compose files
		cmd.Dir = configDir

		// Start from the process env and supply the citadel-owned host ports so
		// compose files that defer their host publish to ${CITADEL_*_HOST_PORT}
		// (llamacpp/vllm/extraction) resolve.
		env := append(os.Environ(), services.HostPortEnv()...)
		// Configure GPU runtime if on Linux
		if platform.IsLinux() {
			env = append(env, "DOCKER_DEFAULT_RUNTIME=nvidia")
		}
		cmd.Env = env

		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to start %s: %w\nOutput: %s", svcName, err, string(output))
		}
	}

	return nil
}

// Ensure ConfigHandler implements JobHandler
var _ JobHandler = (*ConfigHandler)(nil)
