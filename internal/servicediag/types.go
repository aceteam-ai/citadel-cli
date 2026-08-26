// Package servicediag implements the core, read-only diagnostic logic behind
// `citadel service diagnose <name>` (citadel #852). It exists to answer, in one
// shot, "why is this managed service down/unhealthy?" -- the real incident that
// motivated it took three manual `docker logs` invocations, a compose-command
// grep, and a manual `nvidia-smi` to find three separate root causes (a missing
// --trust-remote-code flag, an embedding model served with a chat command, and
// insufficient free VRAM).
//
// This package is deliberately pure/injectable: Diagnose takes an Input (every
// fact already gathered by the caller -- manifest membership, compose file
// bytes, resolved env, free VRAM) and an Inspector (the live docker seam), so
// it is fully unit-testable without a real docker daemon and is reusable by
// other callers (citadel doctor, citadel services) per the issue's ask, not
// just the new CLI command. The cmd/ layer (cmd/service_diagnose.go) is the
// only place that shells out to docker, reads the manifest, or touches the
// filesystem -- this package only computes.
//
// Every check degrades to "unknown" on missing input rather than erroring: a
// diagnostic tool that panics or refuses because ONE signal (docker, GPU,
// declared VRAM table) is unavailable is worse than useless.
package servicediag

// ContainerState is the result of inspecting a managed service's container.
type ContainerState struct {
	// Found is false when no container with this name exists (never started,
	// or removed). Distinct from a plumbing failure (Error non-empty).
	Found bool `json:"found"`
	// Status is the raw docker/podman State.Status ("running", "exited",
	// "created", "paused", "restarting", "dead", ...). Empty when !Found.
	Status string `json:"status,omitempty"`
	// ExitCode is State.ExitCode. Only meaningful when Status is "exited"
	// (0 for services that were still running/never exited).
	ExitCode int `json:"exit_code,omitempty"`
	// Running mirrors State.Running.
	Running bool `json:"running"`
	// Error is set when the inspect call itself failed for a reason other than
	// "no such container" (e.g. docker daemon unreachable). Found/Status/
	// ExitCode/Running are zero-value in that case -- callers must treat this
	// check as "unknown", not "not found".
	Error string `json:"error,omitempty"`
}

// LogTail is a bounded tail of a container's combined stdout/stderr, plus the
// heuristically-extracted root error line.
type LogTail struct {
	// Lines is the raw tail (oldest first), bounded to the requested line
	// count. Nil when logs were unavailable.
	Lines []string `json:"lines,omitempty"`
	// RootError is the single most salient error line found in Lines --
	// heuristic, never a claim of certainty. Empty when nothing matched (the
	// honest fallback is "see the log tail above").
	RootError string `json:"root_error,omitempty"`
	// Available reports whether any log lines were retrieved at all.
	Available bool `json:"available"`
	// Error is set when fetching logs itself failed (docker unavailable, no
	// such container, etc.) -- distinct from "container has no logs yet".
	Error string `json:"error,omitempty"`
}

// ComposeInfo is the effective (variable-substituted) compose command and
// environment for the service, with secret-shaped values redacted.
type ComposeInfo struct {
	// ComposeFilePath is the on-disk compose file path, when one exists.
	// Empty when the service's compose has never been materialized to disk
	// (a catalog service not yet started) -- Source explains why.
	ComposeFilePath string `json:"compose_file_path,omitempty"`
	// Source is "manifest" (compose read from the node's materialized
	// citadel.yaml-declared file), "embedded" (read from the in-binary
	// catalog, not yet materialized to disk), or "" when no compose content
	// was available at all.
	Source string `json:"source,omitempty"`
	// Command is the service's `command:` after best-effort ${VAR}
	// substitution against the resolved env. Empty when the compose has no
	// command or could not be parsed.
	Command string `json:"command,omitempty"`
	// Env is the service's declared `environment:` entries after
	// substitution, with secret-shaped keys redacted. Nil when the compose
	// declares no environment section.
	Env map[string]string `json:"env,omitempty"`
	// ParseError is set when the compose content could not be parsed as
	// YAML. Command/Env are empty in that case.
	ParseError string `json:"parse_error,omitempty"`
}

// PreflightCheck is one best-effort preflight verdict.
type PreflightCheck struct {
	// Name identifies the check, e.g. "vram_fit" or "required_env:CITADEL_VLLM_HOST_PORT".
	Name string `json:"name"`
	// Verdict is "ok", "fail", "warn", or "unknown". "unknown" means the
	// check's input was unavailable, not that anything is wrong.
	Verdict string `json:"verdict"`
	// Detail is a one-line human-readable explanation.
	Detail string `json:"detail"`
}

const (
	VerdictOK      = "ok"
	VerdictFail    = "fail"
	VerdictWarn    = "warn"
	VerdictUnknown = "unknown"
)

// Input is every fact the caller has already gathered, handed to Diagnose.
// Gathering these (reading the manifest, resolving env, shelling to docker for
// container state) is the cmd/ layer's job; this package only reasons about
// the results.
type Input struct {
	// ServiceName is the manifest/catalog service name (e.g. "vllm").
	ServiceName string
	// ContainerName is the resolved container name ("citadel-" + ServiceName
	// by this repo's convention). Passed explicitly rather than derived here
	// so a future non-standard naming scheme doesn't require touching this
	// package.
	ContainerName string
	// ManifestServiceNames is every service name declared in citadel.yaml,
	// used by IsManaged. Empty/nil when no manifest could be read.
	ManifestServiceNames []string

	// ComposeFilePath / ComposeContent / ComposeSource describe the compose
	// definition for this service. ComposeContent is nil when neither an
	// on-disk file nor an embedded catalog entry was found.
	ComposeFilePath string
	ComposeContent  []byte
	ComposeSource   string // "manifest" | "embedded" | ""

	// ResolvedEnv is the UNREDACTED process/compose environment used for
	// ${VAR} substitution and the required-var check. Redaction happens
	// inside this package at render time, keyed by variable name, so callers
	// do not need to pre-redact.
	ResolvedEnv map[string]string

	// MaxLogLines bounds the log tail fetched/scanned. <=0 defaults to 200.
	MaxLogLines int

	// FreeVRAMBytes / FreeVRAMKnown carry the node's current free VRAM
	// (citadel #833 signal, see resmon.Snapshot.GPU.FreeBytes). Known=false
	// means no GPU/nvidia-smi signal was available -- never treated as zero.
	FreeVRAMBytes uint64
	FreeVRAMKnown bool

	// DeclaredVRAMNeedMB / DeclaredVRAMNeedKnown carry the service's coarse
	// provisioning-budget VRAM estimate (status.EngineVRAMEstimateMB), the
	// only legitimate "need" signal available for a service that isn't
	// currently running. Known=false when the engine type isn't in that
	// table -- reported as unknown, never guessed.
	DeclaredVRAMNeedMB    int
	DeclaredVRAMNeedKnown bool
}

// Inspector is the live-docker seam Diagnose depends on, so it is unit
// -testable without a real docker/podman daemon. The real implementation
// (DockerInspector, docker.go) shells out through the citadel-selected
// container runtime.
type Inspector interface {
	// Inspect returns the container's run state. A returned error means the
	// inspect call itself could not be completed (daemon unreachable, engine
	// missing) -- "no such container" is reported via
	// ContainerState{Found:false}, nil, never an error.
	Inspect(containerName string) (ContainerState, error)
	// LogTail returns up to maxLines of the container's most recent combined
	// stdout/stderr, oldest first. An error means the fetch itself failed;
	// "no logs yet" is a nil/empty slice with a nil error.
	LogTail(containerName string, maxLines int) ([]string, error)
}

// Report is the full result of Diagnose.
type Report struct {
	Service       string `json:"service"`
	Managed       bool   `json:"managed"`
	ManagedSource string `json:"managed_source,omitempty"` // "manifest" | "catalog"

	Container ContainerState   `json:"container"`
	Logs      LogTail          `json:"logs"`
	Compose   ComposeInfo      `json:"compose"`
	Checks    []PreflightCheck `json:"checks"`
	// Hints are best-effort, heuristic error-pattern observations (e.g. an
	// OOM signature). Never a claim that the pattern IS the cause.
	Hints []string `json:"hints,omitempty"`

	// Verdict is the single synthesized most-likely cause.
	Verdict string `json:"verdict"`
	// NextAction is a short suggested next step.
	NextAction string `json:"next_action"`
}
