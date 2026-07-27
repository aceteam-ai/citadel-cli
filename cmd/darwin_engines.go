// cmd/darwin_engines.go
package cmd

import (
	"os"
	"strings"
)

// nvidiaDockerOnlyEngines are containerized inference engines whose only images
// require an NVIDIA GPU plus a running Docker daemon (vLLM and SGLang are
// CUDA-only; the llamacpp image is the server-cuda build). On macOS none of
// these can run, so auto-starting them only produces a "Cannot connect to the
// Docker daemon" error that reads like a broken node (issue #608). The supported
// local-inference path on macOS is native ollama.
var nvidiaDockerOnlyEngines = map[string]bool{
	"vllm":     true,
	"sglang":   true,
	"llamacpp": true,
}

// skipEngineOnDarwin reports whether auto-starting the named service should be
// skipped because it is an NVIDIA/docker-only engine that cannot run on the
// given OS. It only ever returns true on darwin: every other platform (Linux
// with NVIDIA, Windows with WSL2) starts these engines as before. goos is passed
// in (rather than read from runtime.GOOS) so the decision is a pure function and
// can be unit-tested on any host. The force flag is an escape hatch to attempt
// the start anyway.
//
// The caller additionally gates this on the service resolving to the docker
// runtime, so a native llamacpp/ollama install on a Mac is never skipped: only
// the CUDA-image docker fallback is suppressed.
func skipEngineOnDarwin(goos, serviceName string, force bool) bool {
	if force {
		return false
	}
	if goos != "darwin" {
		return false
	}
	return nvidiaDockerOnlyEngines[strings.ToLower(strings.TrimSpace(serviceName))]
}

// forceGPUEngines reports whether the operator has opted to attempt starting
// NVIDIA/docker-only engines even on a platform where they normally cannot run
// (kill switch for the darwin skip in skipEngineOnDarwin). Off by default.
func forceGPUEngines() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CITADEL_FORCE_GPU_ENGINES"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
