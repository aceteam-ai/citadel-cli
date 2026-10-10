// internal/jobs/tei.go
//
// jobs-side glue for the aceteam-ai/citadel-cli#1269 GPU-aware TEI image tag and
// CPU-thread opt-in. The pure decision lives in services.TEIComposeEnv; this
// file gathers the two live inputs that decision needs -- the node's GPU compute
// capabilities and whether the container runtime hands a GPU to an unqualified
// container -- behind package-var seams so a compose-up wiring test never shells
// out to the real GPU (node 1795 is a live GPU node). Mirrors cmd/tei.go for the
// cmd-side start paths.
package jobs

import (
	"os"
	"runtime"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

// manifestServiceThreadsFromFile reads the manifest at manifestPath (best-effort)
// and returns the aceteam-ai/citadel-cli#1269 `threads:` value for serviceName,
// or nil when the file is unreadable/unparseable, the service is absent, or its
// threads field is unset. Mirrors manifestServiceBindFromFile (bind.go) for the
// APPLY_DEVICE_CONFIG compose-up, which does not route through serviceStart.
func manifestServiceThreadsFromFile(manifestPath, serviceName string) *int {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil
	}
	var doc struct {
		Services []struct {
			Name    string `yaml:"name"`
			Threads *int   `yaml:"threads"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	for _, s := range doc.Services {
		if s.Name == serviceName {
			return s.Threads
		}
	}
	return nil
}

// teiGPUComputeCaps is the GPU compute-cap source for TEI image-tag resolution.
// Package var seam so compose-up wiring tests inject fixtures instead of shelling
// out to nvidia-smi.
var teiGPUComputeCaps = platform.GetGPUComputeCaps

// teiRuntimeProvidesGPU reports whether the node's container runtime hands a GPU
// to the reservation-less tei.yml by default. Package var seam for tests.
var teiRuntimeProvidesGPU = defaultTEIRuntimeProvidesGPU

// defaultTEIRuntimeProvidesGPU: only Docker with default-runtime=nvidia (what
// `citadel init --provision` configures) gives a GPU to a container that
// declares no device/reservation; rootless Podman needs a CDI device an env
// substitution cannot add. "docker" is the proxy -- the same assumption vLLM's
// own deploy.reservations already ride.
func defaultTEIRuntimeProvidesGPU() bool {
	return catalog.SelectContainerRuntime().EngineBin == "docker"
}

// teiComposeEnvEntries returns the env entries to inject at `docker compose up`
// for serviceName (nil for non-tei). Any var the operator already set in the
// process environment is left untouched (operator override).
func teiComposeEnvEntries(serviceName string, threads *int) []string {
	// Short-circuit before touching the GPU/runtime seams so a non-tei service
	// start never shells out to nvidia-smi (TEIComposeEnv would discard the
	// result anyway).
	if serviceName != services.TEIServiceName {
		return nil
	}
	entries := services.TEIComposeEnv(serviceName, teiGPUComputeCaps(), teiRuntimeProvidesGPU(), resolveTEIThreads(threads))
	return dropEnvKeysAlreadySet(entries)
}

// resolveTEIThreads turns the manifest `threads:` *int into a concrete count:
// nil => 0 (not injected, compose default 1); <=0 => auto (services.TEICPUThreads
// over the node's cores); >0 => verbatim.
func resolveTEIThreads(threads *int) int {
	if threads == nil {
		return 0
	}
	if *threads <= 0 {
		return services.TEICPUThreads(runtime.NumCPU())
	}
	return *threads
}

// dropEnvKeysAlreadySet removes any "KEY=value" entry whose KEY the operator has
// already set in the process environment, so an explicit override is never
// clobbered by citadel's last-wins append onto an os.Environ()-based env.
func dropEnvKeysAlreadySet(entries []string) []string {
	if len(entries) == 0 {
		return entries
	}
	var out []string
	for _, e := range entries {
		key, _, _ := strings.Cut(e, "=")
		if _, set := os.LookupEnv(key); set {
			continue
		}
		out = append(out, e)
	}
	return out
}
