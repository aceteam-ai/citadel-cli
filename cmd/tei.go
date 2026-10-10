// cmd/tei.go
//
// cmd-side glue for the aceteam-ai/citadel-cli#1269 GPU-aware TEI image tag and
// CPU-thread opt-in, mirroring internal/jobs/tei.go for the cmd start paths
// (`citadel run`, boot-time startManagedServices, the port-drift recreate,
// controlcenter, module_update). The pure decision lives in
// services.TEIComposeEnv; this file gathers the GPU compute capabilities and the
// runtime-provides-GPU signal behind package-var seams so a compose-up wiring
// test never shells out to the real GPU.
package cmd

import (
	"os"
	"runtime"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	svcports "github.com/aceteam-ai/citadel-cli/services"
)

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
// for serviceName (nil for non-tei): the GPU image tag when a usable NVIDIA GPU
// is present on a GPU-capable runtime, else the operator-opted-in CPU thread
// count. Any var the operator already set in the process environment is left
// untouched (operator override, e.g. forcing cpu-1.6 on a VRAM-contended box).
func teiComposeEnvEntries(serviceName string, threads *int) []string {
	// Short-circuit before touching the GPU/runtime seams so a non-tei service
	// start never shells out to nvidia-smi (TEIComposeEnv would discard the
	// result anyway).
	if serviceName != svcports.TEIServiceName {
		return nil
	}
	entries := svcports.TEIComposeEnv(serviceName, teiGPUComputeCaps(), teiRuntimeProvidesGPU(), resolveTEIThreads(threads))
	return dropEnvKeysAlreadySet(entries)
}

// resolveTEIThreads turns the manifest `threads:` *int into a concrete count for
// services.TEIComposeEnv: nil => 0 (not injected, compose default 1); <=0 =>
// auto (services.TEICPUThreads over the node's cores); >0 => verbatim.
func resolveTEIThreads(threads *int) int {
	if threads == nil {
		return 0
	}
	if *threads <= 0 {
		return svcports.TEICPUThreads(runtime.NumCPU())
	}
	return *threads
}

// dropEnvKeysAlreadySet removes any "KEY=value" entry whose KEY the operator has
// already set in the process environment, so an explicit override is never
// clobbered by citadel's last-wins append onto composeEnv() (which starts from
// os.Environ()).
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
