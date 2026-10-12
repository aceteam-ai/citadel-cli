package cmd

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/nvr"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/services"
)

// nvrBridgeContainer is the wyze-bridge container name, pinned by the nvr
// module compose (services/nvr-service/compose.yml). Catalog/third-party
// module compose files author their own container_name and are NOT namespaced
// by --node-dir (citadel#860), and `citadel work` refuses --node-dir anyway, so
// this is a fixed global name.
const nvrBridgeContainer = "citadel-nvr-wyze-bridge"

// newNVRSelfHealMonitor builds the nvr per-camera self-heal monitor
// (citadel-cli#1295), or returns nil when the operator disabled self-heal
// (CITADEL_NVR_SELF_HEAL falsey) or the node has no NVR. Returning nil costs
// non-nvr nodes exactly zero: no heartbeat field, no goroutine, no per-tick
// probe.
//
// The "has an NVR" gate is the module lockfile OR the wyze-bridge container
// actually existing. The container fallback is load-bearing: a node that runs
// the NVR via `citadel run` / `citadel init` / a hand `docker compose up` has
// NO lockfile entry (the #733/#739 class), and the mini-NVR this issue fixes
// was a hand-built SJC node -- gating on the lockfile alone would leave
// self-heal silently OFF on exactly the node it exists for. The probe runs
// once at startup, never per tick (only when the cheap lockfile check misses),
// so a non-nvr node still pays almost nothing and a module-installed node pays
// nothing. A node that gains an NVR mid-session picks it up on the next
// `citadel work` restart.
func newNVRSelfHealMonitor() *nvr.Monitor {
	if !nvr.SelfHealEnabled() {
		return nil
	}

	rt := catalog.SelectContainerRuntime()
	if !nvrSelfHealGate(true, nvrModuleInstalled(), func() bool { return nvrBridgeContainerExists(rt) }) {
		return nil
	}

	// Fetch per-camera camera_fps from Frigate's /api/stats over the module's
	// loopback host port. host 8212 maps to Frigate's container port 5000 --
	// its UNAUTHENTICATED internal API port -- so a same-host GET needs no
	// token. No Docker socket, no runtime: a plain HTTP read.
	fetch := func(ctx context.Context) (map[string]float64, error) {
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		url := fmt.Sprintf("http://127.0.0.1:%d/api/stats", services.FrigateHostPort)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("frigate /api/stats returned status %d", resp.StatusCode)
		}
		return nvr.ReadFrigateCameraFPS(resp.Body)
	}

	// Inspect the wyze-bridge container's running state through the runtime
	// seam (never a literal exec.Command("docker"), per the #1041 guard). An
	// absent/stopped container fails closed to "not running", so the monitor
	// never restarts a module an operator deliberately stopped.
	bridgeRunning := func(ctx context.Context) (bool, error) {
		insCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := rt.EngineCommandContext(insCtx, "inspect", "-f", "{{.State.Running}}", nvrBridgeContainer).Output()
		if err != nil {
			return false, err
		}
		return strings.TrimSpace(string(out)) == "true", nil
	}

	// Restart the whole wyze-bridge container through the runtime seam. (A
	// per-camera stream restart via the wyze-bridge REST API is deferred -- it
	// needs an API-key/web-port config the compose does not set and is a
	// Jason-gated node-1314 verification step; see the PR body.)
	restart := func(ctx context.Context, reason string) error {
		runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		cmd := rt.EngineCommandContext(runCtx, "restart", nvrBridgeContainer)
		if out, err := cmd.CombinedOutput(); err != nil {
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				return err
			}
			return fmt.Errorf("%v: %s", err, msg)
		}
		return nil
	}

	threshold := nvr.StallThresholdFromEnv()
	m := nvr.NewMonitor(nvr.MonitorConfig{
		Fetch:          fetch,
		BridgeRunning:  bridgeRunning,
		RestartBridge:  restart,
		StallThreshold: threshold,
		Log:            func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	})
	fmt.Printf("   - NVR self-heal: ENABLED (stall threshold %s, runtime %s)\n", threshold, rt.Label())
	return m
}

// nvrSelfHealGate is the pure gate decision (citadel-cli#1295): run self-heal
// when enabled AND the node has an NVR -- either a lockfile module entry OR,
// only if that misses, the wyze-bridge container actually exists. bridgeExists
// is a func so the container probe is skipped entirely (not just ignored) when
// the lockfile already answers yes, and so this stays unit-testable.
func nvrSelfHealGate(enabled, moduleInstalled bool, bridgeExists func() bool) bool {
	if !enabled {
		return false
	}
	if moduleInstalled {
		return true
	}
	return bridgeExists()
}

// nvrModuleInstalled reports whether the node's module lockfile records an
// `nvr` module -- the canonical "this node runs the NVR as a module" signal
// (installed via MODULE_SET -> liveModuleOps.Install -> recordLock). It does
// NOT cover a hand-built / `citadel run` NVR with no lockfile entry; see the
// container fallback in newNVRSelfHealMonitor.
func nvrModuleInstalled() bool {
	lf, err := catalog.LoadLockfile()
	if err != nil || lf == nil {
		return false
	}
	for _, e := range lf.Modules {
		if e.Name == "nvr" {
			return true
		}
	}
	return false
}

// nvrBridgeContainerExists reports whether the wyze-bridge container exists at
// all (running OR stopped) via a single `inspect` through the runtime seam. An
// `inspect` of a missing container exits non-zero ("no such object"), so exit 0
// means it exists. Used once at startup as the lockfile-miss fallback gate.
func nvrBridgeContainerExists(rt catalog.ContainerRuntime) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return rt.EngineCommandContext(ctx, "inspect", "-f", "{{.State.Running}}", nvrBridgeContainer).Run() == nil
}

// nvrHealthFrom projects an nvr.Snapshot onto the heartbeat-facing
// status.NVRHealth (citadel-cli#1295), mirroring swapStatsFrom/laneActivityFrom.
// internal/status cannot import internal/nvr, so this hand-mapping (pinned by
// TestNVRHealthShapeParity) is the only link between the two shapes.
//
// It returns nil until the monitor has run at least one pass (UpdatedAt zero),
// so the heartbeat field stays omitted on a just-started worker rather than
// reporting an empty, meaningless block.
func nvrHealthFrom(snap nvr.Snapshot) *status.NVRHealth {
	if snap.UpdatedAt.IsZero() {
		return nil
	}
	h := &status.NVRHealth{
		BridgeRunning: snap.BridgeRunning,
		LastRestartAt: snap.LastRestartAt,
		LastError:     snap.LastError,
		UpdatedAt:     snap.UpdatedAt,
	}
	if len(snap.Cameras) > 0 {
		h.Cameras = make([]status.NVRCameraHealth, len(snap.Cameras))
		for i, c := range snap.Cameras {
			h.Cameras[i] = status.NVRCameraHealth{
				Name:          c.Name,
				State:         string(c.State),
				FPS:           c.FPS,
				RestartCount:  c.RestartCount,
				StalledSince:  c.StalledSince,
				LastRestartAt: c.LastRestartAt,
			}
		}
	}
	return h
}
