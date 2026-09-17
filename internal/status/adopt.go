package status

import "context"

// OpenAICompatServing reports whether an OpenAI-compatible engine is already
// answering GET /v1/models with a 200 and a parseable models list on loopback
// :port, and returns the served model id(s).
//
// This is the adoption SIGNAL for aceteam-ai/citadel-cli#1081: a node that
// already serves a vendor engine (e.g. the RM-01's vLLM on
// CITADEL_VLLM_HOST_PORT / :58000) should adopt it rather than launch a
// competing container. TCP reachability alone is deliberately NOT sufficient:
// an arbitrary listener that does not speak the OpenAI /v1/models dialect (a
// non-200, or a body that does not decode as {"data":[...]}) is never adopted.
// The check reuses ModelDiscovery.DiscoverModels, so it accepts exactly what the
// heartbeat's advertise path (DiscoverLocalEngines) already accepts -- including
// a 200 with an EMPTY data list (a live engine with no model loaded), which is
// real serving state, not a failure.
//
// The name says only what it can verify -- that SOMETHING OpenAI-compatible is
// serving. Whether that something is external (worth adopting) vs. citadel's own
// managed container is the CALLER's gate, decided from container state, not from
// this probe.
//
// Bounded by ModelDiscoveryTimeout so a hung listener cannot stall the caller.
func OpenAICompatServing(ctx context.Context, engineType string, port int) (models []string, serving bool) {
	return openAICompatServing(ctx, NewModelDiscovery(), engineType, port)
}

// openAICompatServing is OpenAICompatServing with the model lister injected so
// the probe is unit-testable against an httptest server without a live engine
// (mirroring discoverLocalEngines' modelLister injection).
func openAICompatServing(ctx context.Context, md modelLister, engineType string, port int) ([]string, bool) {
	if port <= 0 {
		return nil, false
	}
	mctx, cancel := context.WithTimeout(ctx, ModelDiscoveryTimeout)
	defer cancel()
	models, err := md.DiscoverModels(mctx, engineType, port)
	if err != nil {
		// Non-200, an unreachable/closed port, a body that is not OpenAI-shaped,
		// or an unsupported engine type -- none of which is safe to adopt.
		return nil, false
	}
	return models, true
}

// AdoptableExternalEnginePort maps an engine whose already-running EXTERNAL
// instance citadel will ADOPT (advertise + dispatch to) instead of launching its
// own container (aceteam-ai/citadel-cli#1081) to its citadel-resolved host port,
// or returns ok=false for an engine outside the adoption allowlist.
//
// Scoped to vLLM on purpose: its host port is citadel-resolved via
// CITADEL_VLLM_HOST_PORT (aceteam-ai/citadel-cli#1076), so the advertise
// (enginePortIfRunning -> IsNativeServiceServing -> the CITADEL_VLLM_HOST_PORT
// probe) and dispatch (llm_inference's baseURLs["vllm"]) paths already target the
// external instance -- adoption only needs to stop the worker from launching a
// competing container on the same port. Other OpenAI-compat engines are
// deliberately not adopted yet (no dogfood driver, and it would surprise a node
// operator); extend this allowlist when one gains the same
// CITADEL_<ENGINE>_HOST_PORT story and a real need.
func AdoptableExternalEnginePort(name string) (int, bool) {
	switch name {
	case "vllm":
		return managedEngineHostPort("vllm"), true
	default:
		return 0, false
	}
}
