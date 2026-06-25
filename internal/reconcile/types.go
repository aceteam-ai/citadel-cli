// Package reconcile implements the node-side desired-state reconciler for
// remote-managed nodes (issue #353, part of epic #352 — GitOps for compute).
//
// A node converges its installed service-modules to a control-plane-assigned
// DesiredState: install missing modules, update changed source-ref/config,
// start/stop to match DesiredStatus, and uninstall removed modules. The local
// citadel.yaml / modules.lock becomes a PROJECTION of this desired state, not
// the source of truth.
//
// This package is deliberately self-contained and decoupled from the live
// docker/catalog/worker code:
//
//   - It identifies module sources by an opaque string (catalog name |
//     owner/repo@ref | git URL) — the SAME grammar internal/catalog.ParseSource
//     accepts — so the wire contract does not leak catalog struct internals.
//   - All side effects go through the ModuleOps interface, so the diff/converge
//     engine is unit-testable without docker, git, or a real fabric.
//   - The control-plane transport goes through the DesiredStateProvider
//     interface; the live HTTP implementation (authed by node device identity)
//     is a LATER issue (aceteam-ai/aceteam#4273). Only the interface and a
//     fake are provided here.
package reconcile

// ----------------------------------------------------------------------------
// WIRE CONTRACT  (load-bearing across repos — keep in sync with aceteam#4273)
//
// These types define the JSON contract between the node reconciler and the
// control plane (/fabric backend). The control plane MUST serve DesiredState
// from `GET <plane>/nodes/{id}/desired-state` and accept ActualState at
// `POST <plane>/nodes/{id}/actual-state`, both authenticated by the node's
// existing device identity. The exact routes are owned by aceteam#4273; the
// SHAPES below are owned jointly and must not drift silently.
// ----------------------------------------------------------------------------

// DesiredStatus is the operator-assigned target run-state of a module.
type DesiredStatus string

const (
	// StatusRunning means the module must be installed and running.
	StatusRunning DesiredStatus = "running"
	// StatusStopped means the module must be installed but NOT running.
	StatusStopped DesiredStatus = "stopped"
)

// ModuleAssignment is one entry of a node's desired state: a module the control
// plane has assigned to this node, by source, with config overrides and a
// target run-state.
//
// JSON shape (wire contract):
//
//	{
//	  "name":           "embedding",          // optional canonical name; see below
//	  "source":         "owner/repo@v1.2.0",  // catalog name | owner/repo@ref | git URL
//	  "config":         {"PORT": "8080"},     // config overrides (key=value)
//	  "desired_status": "running"             // "running" | "stopped"
//	}
type ModuleAssignment struct {
	// Name is the canonical module name used as the reconciliation key (it is
	// what shows up in citadel.yaml / modules.lock as the service name). When
	// empty, the reconciler derives it from Source via NameFromSource, but the
	// control plane SHOULD set it explicitly so the key is stable across
	// source-ref changes (renaming a ref must not be read as remove+add).
	Name string `json:"name,omitempty"`

	// Source identifies where the module comes from. It uses the same grammar
	// as internal/catalog.ParseSource: a bare catalog name ("embedding"), an
	// "owner/repo@ref" GitHub reference, or a full git clone URL (optionally
	// with "@ref"). A change to Source (including its ref) is a drift that the
	// reconciler converges by updating the module in place.
	Source string `json:"source"`

	// Config holds config-var overrides (the same key=value pairs accepted by
	// `citadel module install --set K=V`). A change here is drift to converge.
	Config map[string]string `json:"config,omitempty"`

	// DesiredStatus is the target run-state. Empty defaults to StatusRunning.
	DesiredStatus DesiredStatus `json:"desired_status,omitempty"`
}

// EffectiveStatus returns DesiredStatus, defaulting an empty value to running.
func (m ModuleAssignment) EffectiveStatus() DesiredStatus {
	if m.DesiredStatus == "" {
		return StatusRunning
	}
	return m.DesiredStatus
}

// Key returns the reconciliation key for an assignment: the explicit Name if
// set, otherwise the name derived from Source.
func (m ModuleAssignment) Key() string {
	if m.Name != "" {
		return m.Name
	}
	return NameFromSource(m.Source)
}

// DesiredState is the full set of modules the control plane has assigned to a
// node. It is authoritative: anything installed locally that is not present
// here is drift to be uninstalled.
//
// JSON shape (wire contract):
//
//	{ "modules": [ <ModuleAssignment>, ... ] }
type DesiredState struct {
	Modules []ModuleAssignment `json:"modules"`
}

// ----------------------------------------------------------------------------
// Actual-state report (node -> control plane)
// ----------------------------------------------------------------------------

// ModuleHealth is the observed health of an installed module.
type ModuleHealth string

const (
	// HealthRunning means the module is installed and its containers are up.
	HealthRunning ModuleHealth = "running"
	// HealthStopped means the module is installed but not running.
	HealthStopped ModuleHealth = "stopped"
	// HealthError means the last reconcile of this module failed; see Error.
	HealthError ModuleHealth = "error"
	// HealthUnknown means health could not be determined.
	HealthUnknown ModuleHealth = "unknown"
)

// InstalledModule is the actual on-node state of a single module, as reported
// back to the control plane for drift display.
//
// JSON shape (wire contract):
//
//	{
//	  "name":          "embedding",
//	  "source":        "owner/repo@v1.2.0",
//	  "ref":           "v1.2.0",
//	  "commit":        "abc123...",          // resolved git commit, if known
//	  "config":        {"PORT": "8080"},
//	  "image_digests": ["sha256:..."],
//	  "health":        "running",            // running|stopped|error|unknown
//	  "error":         ""                    // populated when health == error
//	}
type InstalledModule struct {
	Name string `json:"name"`
	// Source is the normalized source string the module was installed from.
	//
	// CANONICAL-FORM CONTRACT (load-bearing for idempotency): the desired
	// ModuleAssignment.Source and this actual Source MUST be expressed in the
	// SAME canonical form, or the engine sees drift on every pass and re-updates
	// forever. Specifically, both sides MUST use the REQUESTED ref form, not a
	// resolved one. A constraint/channel (e.g. "owner/repo@^1.2") is stored in
	// the lockfile alongside its ResolvedRef (e.g. "v1.4.0"); the actual Source
	// reported here is the REQUESTED string ("owner/repo@^1.2"), and the control
	// plane MUST assign the same requested string — NOT the resolved tag. The
	// resolved commit/tag is reported separately via Commit (and Ref) for drift
	// display, and is NOT part of the source-equality diff key.
	Source string `json:"source"`
	// Ref is the requested ref (tag/branch/sha/constraint), if any.
	Ref string `json:"ref,omitempty"`
	// Commit is the resolved git commit, if known.
	Commit string `json:"commit,omitempty"`
	// Config is the effective config-override set the module was installed with.
	Config map[string]string `json:"config,omitempty"`
	// ImageDigests are the deployed image digests (sha256:...), for tamper
	// evidence / drift detection.
	ImageDigests []string `json:"image_digests,omitempty"`
	// Health is the observed run-state / error state.
	Health ModuleHealth `json:"health"`
	// Error carries the failure detail when Health == HealthError. It is the
	// per-module failure-isolation surface: a module that failed to converge
	// reports its error here without blocking the others.
	Error string `json:"error,omitempty"`
}

// ActualState is the node's report of what it actually has installed, posted
// back to the control plane so /fabric can show status and drift.
//
// JSON shape (wire contract):
//
//	{ "node": "node-id", "modules": [ <InstalledModule>, ... ] }
type ActualState struct {
	// Node is the reporting node's identifier (device identity). It may be left
	// empty by the engine and filled in by the transport layer.
	Node string `json:"node,omitempty"`
	// Modules is the observed set of installed modules.
	Modules []InstalledModule `json:"modules"`
}
