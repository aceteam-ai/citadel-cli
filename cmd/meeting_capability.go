package cmd

import (
	"context"
	"sync"

	"github.com/aceteam-ai/citadel-cli/internal/capabilities"
)

// init wires capabilities.NodeDirHook (citadel-cli#1758). internal/capabilities
// is a leaf that cannot import cmd, so the node dir the manifest is read from
// (--node-dir/CITADEL_NODE_DIR, else the node_config_dir indirection behind
// findAndReadManifest) is resolved here. The meeting module's install marker is
// <nodeDir>/services/meeting.yml, written by `citadel module install` into that
// same dir.
func init() {
	capabilities.NodeDirHook = func() string {
		dir, _, err := resolveNodeConfigDirReadOnly()
		if err != nil {
			return ""
		}
		return dir
	}
}

// meetingCapReconciler re-evaluates the `meeting` capability on the heartbeat's
// existing tick and fires onCapable once when it first becomes true.
//
// Why (citadel-cli#1758): capability detection runs once at `citadel work`
// startup, but the meeting module's container needs its start_period (~20s) to
// turn healthy, so on the first start after install the node never advertised
// `meeting`. Re-running detection on the OnStatus tick (rather than hooking
// module health transitions) is the simplest correct option: it needs no new
// health-change plumbing and covers every way the module can become healthy.
//
// Bounded: detect is only DetectMeetingCapability (marker stat, one local
// /health probe when installed, LookPaths) -- no docker sweep. At most one
// probe is in flight, and once capable the reconciler is done (subscribe-only,
// like worker.InferenceQueueReconciler).
type meetingCapReconciler struct {
	detect    func() bool
	onCapable func(ctx context.Context)

	mu      sync.Mutex
	probing bool
	done    bool
}

func newMeetingCapReconciler(detect func() bool, onCapable func(ctx context.Context)) *meetingCapReconciler {
	return &meetingCapReconciler{detect: detect, onCapable: onCapable}
}

// Reconcile is safe to call concurrently and on every tick.
func (r *meetingCapReconciler) Reconcile(ctx context.Context) {
	r.mu.Lock()
	if r.done || r.probing {
		r.mu.Unlock()
		return
	}
	r.probing = true
	r.mu.Unlock()

	ok := r.detect()

	r.mu.Lock()
	r.probing = false
	if ok {
		r.done = true
	}
	r.mu.Unlock()

	if ok {
		r.onCapable(ctx)
	}
}
