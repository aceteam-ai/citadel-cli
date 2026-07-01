// Package footprint samples per-service and node-level resource footprints over
// time and appends them to a rotated, DuckDB/pandas-queryable CSV store under
// ~/citadel-node/footprints/. It is the historical/persistence half of the
// footprint-logging feature (citadel-cli#422); the live TUI view is #421.
//
// Motivation: when a serving engine idled holding VRAM and drove the node to a
// runaway load average, there was NO history to query after the fact. This
// package writes a lightweight time-series so operators can answer "what was
// service X's RSS/VRAM over the last hour?" long after the incident.
//
// The package is intentionally self-contained: it reads the same read-only OS /
// nvidia-smi sources the status collector reads (via internal/platform) but owns
// all of its own code so it never collides with the #421 live-view collector.
package footprint

import "time"

// Sample is one row of the footprint time-series. Per-service rows carry the
// service name and its docker/podman-stats-derived CPU% and RSS; the node-level
// row (Service == NodeService) additionally carries node GPU utilisation and
// total VRAM used. Fields that are not applicable to a given row are left empty
// (represented as an unset *float64 / *int) so the CSV distinguishes "zero" from
// "not measured".
type Sample struct {
	// Timestamp is when the sample was taken (written RFC3339, UTC).
	Timestamp time.Time
	// NodeID is the node identity (Headscale hostname) shared by every row in a
	// tick, so a multi-node footprints/*.csv glob can be grouped by node.
	NodeID string
	// Service is the managed-service name, or NodeService for the node-level row.
	Service string
	// Running reports whether a container for this service was present in the
	// stats snapshot. Always true for the node-level row.
	Running bool

	// CPUPercent is the container's (or host's, for the node row) CPU percentage.
	CPUPercent *float64
	// RSSMB is resident memory in MB.
	RSSMB *float64
	// VRAMMB is total GPU memory used in MB. Only set on the node-level row.
	VRAMMB *int
	// GPUUtilPercent is aggregate GPU utilisation. Only set on the node-level row.
	GPUUtilPercent *float64
	// IdleSeconds is how long the node has been idle, when a readily-available
	// idle signal is wired in (#420). Left empty otherwise — this package does
	// NOT reimplement idle detection.
	IdleSeconds *int
}

// NodeService is the reserved Service value for the synthetic node-level row that
// carries host CPU/RSS + GPU util/VRAM. It is not a real managed service name.
const NodeService = "_node"
