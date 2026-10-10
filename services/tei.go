// services/tei.go
//
// GPU-aware image-tag selection and CPU thread policy for the embedded TEI
// (text-embeddings-inference) embedding service (aceteam-ai/citadel-cli#1269,
// epic aceteam#10876 slice C5).
//
// The problem: tei.yml pins the CPU image (ghcr.io/huggingface/
// text-embeddings-inference:cpu-1.6) with single-thread MKL/OMP/RAYON, so even a
// GPU node embeds ~125x slower than the matching CUDA image (measured on node
// 1795 / RTX 3090 / sm86: 41k chunks ~25h vs ~30min, identical vectors, cosine
// 1.000000). The fix keeps the compose template as the single source of truth
// and injects the GPU image tag via a ${CITADEL_TEI_IMAGE_TAG:-cpu-1.6}
// substitution, chosen from the node's detected GPU compute capability. The
// substitution's :- default is the CPU tag, so a hand-run `docker compose up`
// with no citadel env injection is byte-identical to the pre-#1269 behavior.
//
// `--dtype float32` is preserved on EVERY path (GPU and CPU): vector parity
// across heterogeneous nodes is verified (cosine 1.000000) and load-bearing for
// the sovereign-RAG index, which must be queryable regardless of which node
// embedded a chunk.
//
// The CUDA tag naming scheme is version-independent and sourced from the TEI
// v1.6 README Docker table
// (https://github.com/huggingface/text-embeddings-inference/blob/v1.6.0/README.md):
// CPU -> cpu-1.6, Turing/sm75 -> turing-1.6 (experimental upstream), Ampere
// 80/sm80 -> 1.6 (the bare version tag), Ampere 86/sm86 -> 86-1.6, Ada
// Lovelace/sm89 -> 89-1.6, Hopper/sm90 -> hopper-1.6 (experimental upstream).
package services

import (
	"strconv"
	"strings"
)

const (
	// TEIServiceName is the ServiceMap key for the embedding service.
	TEIServiceName = "tei"

	// TEIImageRepo is the registry-qualified TEI image repository (so a rootless
	// Podman host without unqualified-search-registries still resolves it; see
	// TestCheckedInComposeImagesAreRegistryQualified).
	TEIImageRepo = "ghcr.io/huggingface/text-embeddings-inference"

	// teiImageVersion is the TEI release both the CPU and CUDA tags pin. Bumping
	// this is a deliberate, tested change (new image, re-verify vector parity),
	// so it lives in one place rather than being spread across the tag literals.
	teiImageVersion = "1.6"

	// TEICPUImageTag is the CPU image tag AND the compose ${...:-} default, so a
	// node with no usable GPU (or a hand-run compose) uses the segfault-safe CPU
	// image unchanged (aceteam-ai/citadel-services#14).
	TEICPUImageTag = "cpu-" + teiImageVersion

	// EnvTEIImageTag is the compose env var the image substitution consumes:
	//   image: ghcr.io/huggingface/text-embeddings-inference:${CITADEL_TEI_IMAGE_TAG:-cpu-1.6}
	// Citadel injects it ONLY when a usable GPU tag resolves on a GPU-capable
	// runtime; unset falls through to the CPU default.
	EnvTEIImageTag = "CITADEL_TEI_IMAGE_TAG"

	// EnvTEINumThreads is the compose env var the MKL/OMP/RAYON thread-count
	// substitution consumes (MKL_NUM_THREADS=${CITADEL_TEI_NUM_THREADS:-1} and
	// siblings). The compose default is 1 (the only combination proven stable on
	// cpu-1.6, citadel-services#14); citadel injects a higher value ONLY when the
	// operator opts in via the manifest `threads:` field.
	EnvTEINumThreads = "CITADEL_TEI_NUM_THREADS"

	// teiCPUThreadHeadroom is the number of cores TEICPUThreads reserves for the
	// OS and other node processes when the operator asks for "auto" threads.
	teiCPUThreadHeadroom = 1
)

// TEICUDATagForComputeCap maps an NVIDIA compute capability string (as reported
// by `nvidia-smi --query-gpu=compute_cap`, e.g. "8.6" for an RTX 3090) to the
// matching TEI CUDA image tag for teiImageVersion. Returns "" for a compute
// capability with no published CUDA image at that version (older Volta/sm70,
// newer Blackwell sm100+ on the 1.6 pin, or a malformed value), so the caller
// falls back to the CPU image rather than pulling a tag that does not exist.
//
// Source: TEI v1.6 README Docker table (see file header).
func TEICUDATagForComputeCap(computeCap string) string {
	switch strings.TrimSpace(computeCap) {
	case "7.5": // Turing (T4, RTX 2000 series) — experimental upstream
		return "turing-" + teiImageVersion
	case "8.0": // Ampere 80 (A100, A30) — the bare version tag
		return teiImageVersion
	case "8.6": // Ampere 86 (A10, A40, RTX 3090) — the node 1795 verified case
		return "86-" + teiImageVersion
	case "8.9": // Ada Lovelace (RTX 4000 series, L4, L40)
		return "89-" + teiImageVersion
	case "9.0": // Hopper (H100) — experimental upstream
		return "hopper-" + teiImageVersion
	default:
		return ""
	}
}

// ResolveTEIImageTag picks the GPU CUDA image tag for a node given its GPUs'
// compute capabilities (in nvidia-smi order). It keys on GPU 0 ONLY: TEI binds
// CUDA device 0 by default and the compose declares no device filter, so a tag
// chosen for a different device's capability would ship kernels the serving GPU
// cannot run. Returns ("", false) when there are no GPUs, or GPU 0's capability
// has no published CUDA image at teiImageVersion — the CPU-fallback signal.
func ResolveTEIImageTag(computeCaps []string) (tag string, gpu bool) {
	if len(computeCaps) == 0 {
		return "", false
	}
	t := TEICUDATagForComputeCap(computeCaps[0])
	if t == "" {
		return "", false
	}
	return t, true
}

// TEICPUThreads returns the recommended MKL/OMP/RAYON thread count for a
// CPU-only TEI node: the core count minus a small headroom, floored at 1. It is
// the value injected when an operator sets `threads: 0` (auto) on the tei
// service in citadel.yaml; a positive `threads:` value is used verbatim instead.
func TEICPUThreads(numCPU int) int {
	n := numCPU - teiCPUThreadHeadroom
	if n < 1 {
		return 1
	}
	return n
}

// TEIComposeEnv returns the "KEY=value" env entries citadel injects at
// `docker compose up` for the tei service (nil for any other service name, so
// callers can invoke it unconditionally). The decision:
//
//   - GPU path — a CUDA tag resolves for GPU 0 (ResolveTEIImageTag) AND
//     runtimeProvidesGPU (the container runtime hands a GPU to an unqualified
//     container; see below): inject CITADEL_TEI_IMAGE_TAG=<tag>. Thread env is
//     left at the compose default (1); threads are irrelevant on GPU.
//   - CPU path — no usable GPU tag, or a runtime that cannot hand a GPU to the
//     reservation-less tei.yml: inject CITADEL_TEI_NUM_THREADS=<threads> ONLY
//     when threads > 0 (the operator opted in via the manifest `threads:`
//     field). Unset leaves the compose default (1), byte-identical to the
//     pre-#1269 behavior and segfault-safe on cpu-1.6 (citadel-services#14).
//
// runtimeProvidesGPU gates the GPU path because tei.yml carries NO GPU
// reservation (it must also start on CPU-only nodes, where a mandatory nvidia
// reservation would fail container creation). It therefore only sees a GPU on a
// runtime that provides one by default — Docker with default-runtime=nvidia,
// which `citadel init --provision` configures. Rootless Podman hands a GPU only
// to a container that declares a CDI device, which an env substitution cannot
// add, so a Podman GPU node keeps the CPU image until that follow-up lands.
func TEIComposeEnv(serviceName string, computeCaps []string, runtimeProvidesGPU bool, threads int) []string {
	if serviceName != TEIServiceName {
		return nil
	}
	if tag, gpu := ResolveTEIImageTag(computeCaps); gpu && runtimeProvidesGPU {
		return []string{EnvTEIImageTag + "=" + tag}
	}
	if threads > 0 {
		return []string{EnvTEINumThreads + "=" + strconv.Itoa(threads)}
	}
	return nil
}

// ParseTEIServingImage extracts the image tag and compute device from a resolved
// TEI container image reference (e.g. the .Config.Image of a running
// `citadel-tei` container, which docker/podman store with the ${...} already
// resolved to a concrete tag). Returns ("", "") for a reference that is not a
// TEI image. Device is "cpu" for a cpu-* tag, "cuda" otherwise (every published
// non-CPU TEI image is CUDA). Used to report which build and device are actually
// serving (citadel-cli#1269 part 4), independent of how the container started.
func ParseTEIServingImage(image string) (tag, device string) {
	image = strings.TrimSpace(image)
	if !strings.Contains(image, "text-embeddings-inference") {
		return "", ""
	}
	// The tag is the segment after the last ':' that follows the last '/', so a
	// registry host:port prefix (ghcr.io has none, but be exact) is not mistaken
	// for the tag.
	lastSlash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon <= lastSlash {
		return "", "" // no tag component
	}
	tag = image[colon+1:]
	if tag == "" {
		return "", ""
	}
	return tag, TEIDeviceForTag(tag)
}

// TEIDeviceForTag classifies a TEI image tag as its compute device: "cpu" for
// the cpu-* tags, "cuda" for every GPU tag (turing-/86-/89-/hopper-/the bare
// version). It is the single place that mapping lives so the reporting path and
// any future consumer agree.
func TEIDeviceForTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "cpu" || strings.HasPrefix(tag, "cpu-") {
		return "cpu"
	}
	return "cuda"
}
