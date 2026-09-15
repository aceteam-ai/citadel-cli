//go:build darwin

package services

import _ "embed"

// compose_variants_darwin.go swaps in the macOS CPU/arm64 compose variants for
// the two darwin-capable LLM engines whose default (linux) compose files carry a
// mandatory nvidia GPU reservation that fails container creation on Docker
// Desktop for macOS (which exposes no GPU to Linux containers). citadel-cli#1048.
//
// These are embedded into the SAME OllamaCompose/LlamacppCompose names the
// non-darwin build uses (compose_variants_other.go), so ServiceMap and every
// consumer are unchanged; only the embedded bytes differ on a darwin build. The
// two variant files differ from their linux originals by exactly the GPU
// reservation (and, for llamacpp, the CPU image tag) -- pinned by
// TestDarwinComposeVariantsOnlyDropGPU.

//go:embed compose/ollama.darwin.yml
var OllamaCompose string

//go:embed compose/llamacpp.darwin.yml
var LlamacppCompose string
