package status

import "time"

// NVRHealth is the heartbeat-facing mirror of the nvr module's self-heal
// snapshot (citadel-cli#1295). internal/status cannot import internal/nvr (it
// would pull the nvr package's growing surface onto the heartbeat path and
// invert the dependency direction cmd already establishes for AppPods/
// Reservations), so this is a hand-maintained mirror of nvr.Snapshot; cmd's
// nvrHealthFrom builds it and TestNVRHealthShapeParity pins the JSON-field
// parity, exactly like SwapActivity mirrors worker.SwapStats.
//
// Additive/omitempty: a node without the nvr module installed (or with
// self-heal disabled) wires no provider, so NodeStatus.NVR stays nil and the
// heartbeat payload is unchanged.
type NVRHealth struct {
	// Cameras is the per-camera recording/stalled/recovering/offline/unknown
	// view so an owner can see a dead camera without opening the Frigate UI.
	Cameras []NVRCameraHealth `json:"cameras,omitempty"`
	// BridgeRunning reports whether the wyze-bridge container was running at
	// the last probe. False means the module is stopped (self-heal does
	// nothing while it is down).
	BridgeRunning bool `json:"bridge_running"`
	// LastRestartAt is when the monitor last restarted the bridge, if ever.
	LastRestartAt time.Time `json:"last_restart_at,omitempty"`
	// LastError is the most recent probe error (Frigate unreachable, etc.),
	// empty on a clean probe.
	LastError string `json:"last_error,omitempty"`
	// UpdatedAt is when the monitor last ran a detection pass.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// NVRCameraHealth is the per-camera mirror of nvr.CameraHealth.
type NVRCameraHealth struct {
	Name          string    `json:"name"`
	State         string    `json:"state"`
	FPS           float64   `json:"fps"`
	RestartCount  int       `json:"restart_count,omitempty"`
	StalledSince  time.Time `json:"stalled_since,omitempty"`
	LastRestartAt time.Time `json:"last_restart_at,omitempty"`
}
