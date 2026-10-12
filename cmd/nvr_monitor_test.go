package cmd

import (
	"reflect"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nvr"
	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// TestNVRHealthShapeParity guards the hand-maintained mirror between
// nvr.Snapshot/CameraHealth (the source of truth the monitor owns) and
// status.NVRHealth/NVRCameraHealth (the heartbeat-facing projection
// nvrHealthFrom builds). internal/status cannot import internal/nvr, so nothing
// but this test keeps the two shapes honest: a field added to one without the
// other would silently never reach the heartbeat. Mirrors TestSwapShapeParity.
func TestNVRHealthShapeParity(t *testing.T) {
	assertFieldsMatch := func(t *testing.T, source, mirror reflect.Type) {
		t.Helper()
		sourceFields := jsonFieldNames(source)
		mirrorFields := jsonFieldNames(mirror)
		for name := range sourceFields {
			if _, ok := mirrorFields[name]; !ok {
				t.Errorf("%s has JSON field %q with no counterpart in %s -- "+
					"a field was added to the nvr monitor's source-of-truth type "+
					"without updating the heartbeat mirror (and nvrHealthFrom), "+
					"so it will never reach the heartbeat", source, name, mirror)
			}
		}
		for name := range mirrorFields {
			if _, ok := sourceFields[name]; !ok {
				t.Errorf("%s has JSON field %q with no counterpart in %s -- "+
					"the heartbeat mirror drifted ahead of its source of truth; "+
					"verify nvrHealthFrom still maps this field", mirror, name, source)
			}
		}
	}

	t.Run("Snapshot/NVRHealth", func(t *testing.T) {
		assertFieldsMatch(t,
			reflect.TypeOf(nvr.Snapshot{}),
			reflect.TypeOf(status.NVRHealth{}))
	})
	t.Run("CameraHealth/NVRCameraHealth", func(t *testing.T) {
		assertFieldsMatch(t,
			reflect.TypeOf(nvr.CameraHealth{}),
			reflect.TypeOf(status.NVRCameraHealth{}))
	})
}

// TestNVRSelfHealGate pins the self-heal gate decision (citadel-cli#1295):
// disabled is always off; a lockfile module entry short-circuits (never probes
// the container); a lockfile miss falls back to the container-exists probe so a
// hand-built / citadel-run NVR with no lockfile entry is still covered.
func TestNVRSelfHealGate(t *testing.T) {
	t.Run("disabled is always off and never probes", func(t *testing.T) {
		probed := false
		if nvrSelfHealGate(false, true, func() bool { probed = true; return true }) {
			t.Error("disabled gate returned true")
		}
		if probed {
			t.Error("bridgeExists probed while self-heal disabled")
		}
	})
	t.Run("module installed short-circuits the probe", func(t *testing.T) {
		probed := false
		if !nvrSelfHealGate(true, true, func() bool { probed = true; return false }) {
			t.Error("module-installed gate returned false")
		}
		if probed {
			t.Error("bridgeExists probed when the module is already in the lockfile")
		}
	})
	t.Run("lockfile miss falls back to container probe", func(t *testing.T) {
		if !nvrSelfHealGate(true, false, func() bool { return true }) {
			t.Error("gate off even though the wyze-bridge container exists")
		}
		if nvrSelfHealGate(true, false, func() bool { return false }) {
			t.Error("gate on with neither a lockfile entry nor a container")
		}
	})
}

// TestNVRHealthFrom pins the nvr.Snapshot -> status.NVRHealth projection,
// including the "omit until the monitor has run" rule (UpdatedAt zero -> nil),
// mirroring reconcileHealthFrom's conditional-nil contract.
func TestNVRHealthFrom(t *testing.T) {
	// Never reconciled: nil so NodeStatus.NVR stays omitted.
	if got := nvrHealthFrom(nvr.Snapshot{}); got != nil {
		t.Errorf("nvrHealthFrom(zero) = %+v, want nil", got)
	}

	now := time.Unix(1000, 0)
	restarted := time.Unix(900, 0)
	stalled := time.Unix(800, 0)
	in := nvr.Snapshot{
		BridgeRunning: true,
		LastRestartAt: restarted,
		LastError:     "boom",
		UpdatedAt:     now,
		Cameras: []nvr.CameraHealth{
			{Name: "front", State: nvr.CameraOffline, FPS: 0, RestartCount: 2, StalledSince: stalled, LastRestartAt: restarted},
			{Name: "side", State: nvr.CameraRecording, FPS: 5.0},
		},
	}
	want := &status.NVRHealth{
		BridgeRunning: true,
		LastRestartAt: restarted,
		LastError:     "boom",
		UpdatedAt:     now,
		Cameras: []status.NVRCameraHealth{
			{Name: "front", State: "offline", FPS: 0, RestartCount: 2, StalledSince: stalled, LastRestartAt: restarted},
			{Name: "side", State: "recording", FPS: 5.0},
		},
	}
	if got := nvrHealthFrom(in); !reflect.DeepEqual(got, want) {
		t.Errorf("nvrHealthFrom() = %+v, want %+v", got, want)
	}
}
