// Package engine is the Phase A "adapter shell" from
// docs/design-engine-adapter.md (citadel #685 slice 1 of ~5).
//
// It is a PURE, ADDITIVE TRANSLATION of the ~dozen hand-synced per-engine
// tables the design doc inventories (services/ports.go, services/caches.go,
// internal/status/{engines,hotswap,models}.go, internal/worker's readiness
// and load-estimate tables, internal/jobs's self-provisioning table) into one
// EngineSpec struct per engine, held in a Registry populated at package
// init(). Nothing in this slice calls Registry -- no caller migration, zero
// behavior change. See TestRegistryEquivalence for the safety net that proves
// this translation is faithful to the tables it mirrors.
//
// Scope discipline (moved, not changed): a value copied here is EXACTLY what
// the source table returns today, gaps and all -- e.g. llamacpp's absent
// DefaultModel (citadel #685 §1a) is mirrored as absent, not filled in. Do
// not "fix" a gap discovered while translating; file it as a follow-up
// instead (see the PR body for the list found while building this).
//
// Leaf constraint: this package may import services (and nothing else
// project-internal today), and must NOT be imported by internal/worker or
// internal/jobs yet -- that stays true until a later slice migrates a real
// caller there. Two of the tables this package translates (internal/worker's
// engineReadyPath/defaultLoadEstimate, internal/jobs's
// selfProvisioningEngines) live in packages internal/engine must NOT import,
// since internal/worker and internal/jobs are expected to import
// internal/engine in a later slice (Phase C) and importing them here today
// would make that a cycle. Their values are therefore literal copies within
// this package (see registry.go's readyPathByEngine/loadEstimateByEngine/
// selfProvisioningEngines vars); TestRegistryEquivalence lives in this
// package's own EXTERNAL test binary (package engine_test, in
// registry_test.go) which CAN import internal/worker/internal/jobs/
// internal/status without creating a cycle, and checks those literal copies
// against the real tables via the exported accessors added alongside this
// package (worker.EngineReadyPath, worker.DefaultLoadEstimate,
// jobs.IsSelfProvisioningEngine).
//
// internal/status was ALSO one of this package's inputs in slice 1 (Phase A):
// buildSpec called status.EngineDefaultModel/ManagedEngineHostPort/
// EngineVRAMEstimateMB/EngineModelEnvVars/IdleCapableEngines/
// EmbeddingProbeServices/ManagedProbeEngines to populate EngineSpec. Slice 2
// (citadel #685) reversed that: internal/status's own read-path consumers
// (EngineTypeFromName, DiscoverModels, CheckServiceHealth, and those same
// membership lists) now need to read FROM this package's Registry, which is
// only possible if this package no longer imports internal/status (a cycle
// otherwise: internal/status -> internal/engine -> internal/status). So this
// package now owns those tables directly (tables.go) as literal copies of
// what internal/status used to hand-maintain, and internal/status's
// same-named package vars are derived FROM this package's exported
// accessors at init instead. See tables.go's own doc comment for the detail;
// this paragraph exists so a reader of THIS file doesn't conclude
// internal/status still feeds this package the way slice 1 described.
package engine

import (
	"time"

	"github.com/aceteam-ai/citadel-cli/services"
)

// EngineKind classifies how an engine's process is managed.
type EngineKind string

const (
	// ComposeService is a docker-compose-managed engine (services.ServiceMap
	// entry with a compose file citadel materializes and brings up).
	ComposeService EngineKind = "compose_service"
	// NativeProcess is an engine citadel does not own the lifecycle of via
	// compose -- ollama is the only one today (see design doc §2's coverage
	// table: "ollama | NativeProcess (fixed port)").
	NativeProcess EngineKind = "native_process"
)

// RequestDialect identifies the wire dialect an engine's Serve() (a later
// slice; see design doc §1e) would speak. citadel #685 §1e found only THREE
// real dialects behind six existing execute* functions in
// internal/worker/llm_inference.go.
//
// The zero value ("") is deliberate, not an oversight: it marks an engine
// this concept does not apply to (tei/diffusers are not chat-completions
// engines; lmstudio has no dispatch path anywhere in the codebase today --
// design doc §2's coverage table calls it "unclear/likely OpenAIChat" and
// leaves it an open question, §6 Q2). Assigning it a real dialect here would
// be inventing a fact this slice's "translation only" policy forbids.
type RequestDialect string

const (
	// OpenAICompletions is the OpenAI /v1/completions (prompt) dialect.
	OpenAICompletions RequestDialect = "openai_completions"
	// OpenAIChat is the OpenAI /v1/chat/completions (messages) dialect.
	OpenAIChat RequestDialect = "openai_chat"
	// OllamaNative is ollama's own /api/generate + /api/chat dialect.
	OllamaNative RequestDialect = "ollama_native"
	// CompletionsOnly marks an engine that has ONLY the completions dialect
	// wired today, with no chat-completions equivalent -- sglang, per design
	// doc §1e/§2 ("SGLang | ComposeService | OpenAICompletions only") and its
	// open question §6 Q1 (whether that is a deliberate scope or a gap).
	// Kept distinct from OpenAICompletions so a later slice can tell "this
	// engine's only dialect is completions" from "this engine's PRIMARY
	// dialect today happens to be completions, chat may follow" without
	// re-deriving it from the design doc.
	CompletionsOnly RequestDialect = "completions_only"
)

// Readiness is the probe-result vocabulary a later slice's Engine.Probe(ctx)
// (design doc §2's proposed interface) will return. Forward-declared here,
// alongside the rest of the type shape this slice proves out, but UNUSED by
// EngineSpec/Engine in slice 1 -- there is no Probe() method yet (see the
// Engine interface below: v1 is deliberately Name/Kind/Spec only).
type Readiness string

const (
	ReadinessReady    Readiness = "ready"
	ReadinessStarting Readiness = "starting"
	ReadinessDown     Readiness = "down"
	ReadinessUnknown  Readiness = "unknown"
)

// EngineSpec is the per-engine fact sheet this slice collapses the existing
// hand-synced tables into. Every field below cites the table it translates in
// registry.go; see that file for the actual per-engine values and their
// sourcing.
type EngineSpec struct {
	// Name is the engine's canonical name (services.ServiceMap key).
	Name string
	// Kind classifies process management (compose vs. native).
	Kind EngineKind
	// HostPort is the citadel-assigned or native host port this engine's API
	// answers on. 0 when unknown/unmanaged.
	HostPort int
	// HostPortEnvVar is the compose env-var name that carries HostPort, when
	// citadel injects it via ${CITADEL_*_HOST_PORT} substitution (empty for
	// engines with a fixed/native port, e.g. ollama, sglang, lmstudio, tei,
	// transcribe -- see services/ports.go's HostPortEnvVarName).
	HostPortEnvVar string
	// CacheDir is the subdirectory of ~/citadel-cache this engine's weights
	// (or native store) live in (services.EngineCacheDirs).
	CacheDir string
	// CacheFamily is the on-disk layout at CacheDir (services.CacheFamily).
	CacheFamily services.CacheFamily
	// Dialect is the request wire dialect (see RequestDialect's doc comment
	// for what an empty value means).
	Dialect RequestDialect
	// ReadyPath is the endpoint that answers 200 once the engine's API is
	// serving (internal/worker's engineReadyPath). Empty when the engine has
	// no readiness-probe entry today.
	ReadyPath string
	// LoadEstimate is the coarse per-engine cold-start estimate
	// (internal/worker's defaultLoadEstimate). That function is a switch with
	// a default case -- a total function, not a lookup with real "absent"
	// entries -- so EVERY engine here carries the value the real switch would
	// return for it, including the six that fall through to its 60s default
	// branch (registry.go's loadEstimateByEngine is the literal copy).
	LoadEstimate time.Duration
	// VRAMEstimateMB is the coarse per-engine VRAM provisioning budget
	// (tables.go's vramEstimateMBByEngine -- internal/status.EngineVRAMEstimateMB
	// is now a thin wrapper reading this same value back out). 0 when unknown.
	VRAMEstimateMB int
	// ModelEnvVar is the <name>.env variable(s), in preference order, that
	// select this engine's served model (tables.go's modelEnvVarsByEngine;
	// internal/status.EngineModelEnvVars wraps it). Nil when the engine has
	// no such override.
	ModelEnvVar []string
	// DefaultModel is the served model id this engine falls back to when
	// ModelEnvVar resolves nothing (tables.go's defaultModelByEngine;
	// internal/status.EngineDefaultModel wraps it). A POINTER,
	// deliberately: nil means "no entry in the source table" (llamacpp's
	// documented gap, citadel #685 §1a -- the engine cannot express a stable
	// default and none is invented), which is a different fact from a
	// present-but-empty string. Never dereference without a nil check.
	DefaultModel *string
	// IdleCapable reports membership in tables.go's idleCapableEngineNames
	// (internal/status.IdleCapableEngines wraps
	// IdleCapableEngineNames()) -- engines with a reliable SCRAPED
	// idle/request signal.
	IdleCapable bool
	// EmbeddingCapable reports membership in tables.go's
	// embeddingCapableEngineNames (internal/status.EmbeddingProbeServices
	// wraps EmbeddingCapableEngineNames()).
	EmbeddingCapable bool
	// ManagedProbe reports membership in tables.go's managedProbeEngineNames
	// (internal/status.ManagedProbeEngines wraps ManagedProbeEngineNames())
	// -- the engines the heartbeat's model/health probe iterates.
	ManagedProbe bool
	// MetricsPort is the host port this engine exposes a Prometheus /metrics
	// endpoint on (services.InferenceMetricsPorts()), or 0 when it has none.
	MetricsPort int
	// SelfProvisioning reports whether this engine's compose file owns its
	// weights (internal/jobs's selfProvisioningEngines), so a
	// MODEL_CACHE_PULL for it is a no-op.
	SelfProvisioning bool
}

// Engine is the v1 (Phase A) adapter surface: identity + fact sheet only.
// Deliberately NOT the fuller interface design doc §2 proposes
// (Start/Stop/Probe/Serve) -- those land in later slices (Phase B/C), once
// this shape has proven it can express all twelve ServiceMap engines,
// including the asymmetric ones (bonsai's build files, sglang's
// completions-only dialect, ollama's native protocol, lmstudio's
// near-total absence).
type Engine interface {
	Name() string
	Kind() EngineKind
	Spec() EngineSpec
}

// specEngine is the only Engine implementation in this slice: a thin wrapper
// around a pre-built EngineSpec. There is nothing behavioral to implement yet
// (no Start/Stop/Probe/Serve), so this is intentionally trivial.
type specEngine struct {
	spec EngineSpec
}

func (e specEngine) Name() string     { return e.spec.Name }
func (e specEngine) Kind() EngineKind { return e.spec.Kind }
func (e specEngine) Spec() EngineSpec { return e.spec }
