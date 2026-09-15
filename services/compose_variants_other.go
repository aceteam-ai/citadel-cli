//go:build !darwin

package services

import _ "embed"

// compose_variants_other.go holds the ollama and llamacpp compose embeds for
// every non-darwin OS (linux, windows). These are byte-identical to the files
// citadel has always shipped; the split exists only so darwin can swap in
// CPU/arm64 variants at build time via the same OllamaCompose/LlamacppCompose
// names (see compose_variants_darwin.go and citadel-cli#1048). ServiceMap in
// embed.go references those names directly, so no read site, composerefresh, the
// bind sweep, or the known-hash bootstrap needs to know which OS it is on.

//go:embed compose/ollama.yml
var OllamaCompose string

//go:embed compose/llamacpp.yml
var LlamacppCompose string
