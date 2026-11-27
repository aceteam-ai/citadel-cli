package cmd

import (
	"github.com/aceteam-ai/citadel-cli/internal/browserruntime"
	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// configureBrowserRuntime wires the node-wide co-browse manager to the container
// runtime (citadel-cli#1760). The engine is resolved through the
// catalog.ContainerRuntime seam; platform cannot import catalog. With no pinned
// image in this release the host runners stay in charge.
func configureBrowserRuntime() {
	values, _ := readApplianceConfig(nodePermissionsDir())
	slots, window := browserRuntimeSettings(getDeviceConfigFromFile(), values)
	platform.SetBrowserWindowSize(window)
	cfg := platform.BrowserRuntimeConfig{Image: browserruntime.PinnedImage, Slots: slots}
	if cfg.Image != "" {
		cfg.Engine = catalog.SelectContainerRuntime()
	}
	platform.ConfigureBrowserRuntime(cfg)
}

func browserRuntimeSettings(dc *DeviceConfig, values map[string]any) (slots int, window string) {
	slots = 0
	if dc != nil {
		slots = dc.BrowserSlots
	}
	// Appliance settings also work before enrollment; the credential reader
	// deliberately ignores a config file without credentials.
	if values["browser_appliance"] == true {
		if n, ok := values["browser_slots"].(int); ok && n >= 1 && n <= 64 {
			slots = n
		}
		if size, ok := values["browser_window_size"].(string); ok && validateApplianceOptions(applianceOptions{slots: 1, window: size}) == nil {
			window = size
		}
	}
	return slots, window
}
