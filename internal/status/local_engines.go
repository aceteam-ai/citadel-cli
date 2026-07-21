package status

import (
	"context"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
)

// LocalEngine describes a managed serving engine currently running on THIS node
// and the model(s) it is serving. It is the discovery surface used by
// `citadel chat` (aceteam-ai/citadel-cli#575) to find an engine to talk to.
//
// Name is the managed-engine name ("vllm", "ollama", "llamacpp", "bonsai"),
// Port is the citadel-assigned host port the engine's OpenAI-compatible API
// listens on (localhost), and Models are the loaded model id(s) discovered from
// that API. Models may be empty for an engine that is up but has no model
// loaded (e.g. llama.cpp in deferred-load mode).
type LocalEngine struct {
	Name   string
	Port   int
	Models []string
}

// DiscoverLocalEngines probes the managed serving engines (managedProbeEngines)
// for a live signal on this node and returns those that are running, along with
// each engine's currently loaded model(s). It reuses the same running-check and
// model-discovery path the heartbeat uses (managedEnginePortIfRunning +
// ModelDiscovery), so `citadel chat` sees exactly what the fleet sees.
//
// An engine is included when it is running and its port is known, regardless of
// whether model discovery returned any models — a running-but-empty engine is
// real state the caller may still want to surface. Model discovery is bounded by
// ModelDiscoveryTimeout so a slow/hung engine never stalls discovery; a failed
// probe simply leaves Models empty.
func DiscoverLocalEngines(ctx context.Context) []LocalEngine {
	engineBin := catalog.SelectContainerRuntime().EngineBin
	md := NewModelDiscovery()

	var out []LocalEngine
	for _, name := range managedProbeEngines {
		port, running := managedEnginePortIfRunning(engineBin, name)
		if !running || port <= 0 {
			continue
		}

		eng := LocalEngine{Name: name, Port: port}

		mctx, cancel := context.WithTimeout(ctx, ModelDiscoveryTimeout)
		models, err := md.DiscoverModels(mctx, name, port)
		cancel()
		if err == nil {
			eng.Models = models
		}

		out = append(out, eng)
	}
	return out
}
