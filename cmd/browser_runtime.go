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
	slots := 0
	if dc := getDeviceConfigFromFile(); dc != nil {
		slots = dc.BrowserSlots
	}
	cfg := platform.BrowserRuntimeConfig{Image: browserruntime.PinnedImage, Slots: slots}
	if cfg.Image != "" {
		cfg.Engine = catalog.SelectContainerRuntime()
	}
	platform.ConfigureBrowserRuntime(cfg)
}
