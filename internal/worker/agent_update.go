// internal/worker/agent_update.go
//
// AGENT_UPDATE job handler (issue aceteam#4427). Lets the control plane update
// a node's Citadel agent remotely, closing the "silent version skew" gap that
// made aceteam#4373 undiagnosable (a node stuck on an old release, invisible
// from the dashboard, with no remote way to fix it short of SSH).
//
// The handler reuses the exact same update machinery as `citadel update
// install` (internal/update): check GitHub Releases, download + checksum-verify
// the platform asset, atomically swap the running binary, and keep the previous
// binary for rollback. It supports an optional pinned `target_version` in the
// payload and defaults to the latest release.
//
// # The self-restart crux
//
// This handler runs *inside* the worker process it must restart. If it restarted
// synchronously, the job result would never be published — the dispatcher would
// see a timeout (exactly the failure mode we're trying to eliminate). So the
// ordering guarantee is:
//
//  1. Execute() installs the new binary and returns a SUCCESS result
//     ({new_version, old_version}) through the normal result path.
//  2. The runner publishes that result (stream.WriteEnd) and ACKs the job.
//  3. Only THEN does a restart fire.
//
// The post-ack signal we lean on is the runner's activeJobs counter: it is
// decremented in a `defer` that runs *after* WriteEnd + Ack (see
// runner.processJob). So observing ActiveJobs()==0 from the arming goroutine
// proves this job's own result was already published and acked. We Drain first
// (stop fetching new jobs) to close the pickup race, wait for idle, then
// restart — the same discipline the AutoUpdater already uses.
//
// Restart re-execs the current process onto the freshly-installed binary via
// update.RestartProcess (syscall.Exec) — the same mechanism the production
// AutoUpdater already uses. This is deliberately NOT a SIGTERM-and-exit: the
// systemd unit ships Restart=on-failure (internal/service/systemd.go), which
// does not relaunch on a clean exit, so a graceful exit would leave the node
// permanently down — strictly worse than the version-skew bug this fixes.
// syscall.Exec preserves the PID and replaces the image in place, so it does
// not depend on the supervisor's restart policy at all. Crucially, it does NOT
// orphan the VPN identity: the tsnet node key is persisted to the node's state
// directory (internal/network), so the new image re-inits the mesh from the
// same identity. We only self-restart when running as a managed service
// (CITADEL_SERVICE=true); in a foreground/interactive run we report "updated,
// restart required" and leave the operator's session alone.
//
// Privilege gating: AGENT_UPDATE is only honored when the job arrives on the
// per-node stream (jobs:v1:shell:org_<id>:node:<nodeid>), never the shared org
// pool — updating a node is a privileged, node-targeted operation. Server-side
// this should additionally require a scope / signed capability (#4313); that is
// aceteam-side and tracked separately.
package worker

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/update"
)

// AgentUpdateConfig configures an AgentUpdateHandler. Every side-effecting
// dependency is injectable so the handler can be unit-tested without touching
// the network, the filesystem, or the running process.
type AgentUpdateConfig struct {
	// Version is the currently-running agent version (e.g. "v2.46.0"). Reported
	// back as old_version and used to decide "already latest".
	Version string

	// GetRelease returns the release to install. `target` is the requested
	// version tag (empty means "latest"). It returns (nil, nil) when already on
	// the requested/latest version. Defaults to a GitHub-backed lookup.
	GetRelease func(target string) (*update.Release, error)

	// Download downloads + checksum-verifies the release asset to destPath.
	// Defaults to update.NewClient(...).DownloadAndVerify.
	Download func(release *update.Release, destPath string) error

	// Apply atomically swaps the running binary with the verified pending
	// binary (keeping the previous copy for rollback). Defaults to
	// update.ApplyUpdate.
	Apply func(pendingPath string) error

	// PendingPath is where the downloaded binary is staged.
	// Defaults to update.GetPendingBinaryPath().
	PendingPath string

	// IsService reports whether we are running under a managed service
	// supervisor (systemd/launchd/Windows SCM) that will relaunch us on a clean
	// exit. Defaults to a CITADEL_SERVICE=true env-var check. Only when true do
	// we self-restart; otherwise we report "restart required".
	IsService func() bool

	// Drain stops the runner from fetching new jobs so no work is picked up
	// between our ack and the restart. Wired to runner.Drain.
	Drain func()

	// ActiveJobs reports the number of in-flight jobs. Reaching 0 proves our own
	// result was published + acked (the counter is decremented after ack), so we
	// only restart once it hits 0. Wired to runner.ActiveJobs.
	ActiveJobs func() int

	// Restart re-execs the process onto the freshly-installed binary. Defaults to
	// update.RestartProcess (syscall.Exec, in-place image swap, PID-preserving —
	// the same mechanism the AutoUpdater uses). Overridable so tests can assert
	// restart ordering without actually exec'ing.
	Restart func() error

	// IdlePollInterval is how often to re-check ActiveJobs while waiting to
	// restart. Zero uses a sensible default (500ms).
	IdlePollInterval time.Duration

	// IdleTimeout bounds how long the arming goroutine waits for the node to go
	// idle before restarting anyway. Zero uses a sensible default (2m). Our own
	// job acking should make the node idle almost immediately.
	IdleTimeout time.Duration

	// Log reports progress. If nil a no-op logger is used.
	Log func(format string, args ...any)

	// RecordState persists the update to disk (previous/current version) so the
	// new process and `citadel update status` reflect it. Defaults to writing
	// update state; overridable/no-op in tests.
	RecordState func(oldVersion, newVersion string)
}

// AgentUpdateHandler processes AGENT_UPDATE jobs.
type AgentUpdateHandler struct {
	cfg AgentUpdateConfig
}

// NewAgentUpdateHandler constructs an AGENT_UPDATE handler, applying defaults
// that wire it to the real update package and the running process. Runner-bound
// dependencies (Drain, ActiveJobs) must be set by the caller after the runner
// exists — see cmd/work.go.
func NewAgentUpdateHandler(cfg AgentUpdateConfig) *AgentUpdateHandler {
	if cfg.GetRelease == nil {
		cfg.GetRelease = func(target string) (*update.Release, error) {
			client := update.NewClientWithTimeout(cfg.Version, 60*time.Second)
			if target == "" {
				// Latest, with the same "nil means already up to date" contract.
				return client.CheckForUpdate()
			}
			rel, err := client.GetReleaseByTag(target)
			if err != nil {
				return nil, err
			}
			// Honor the "already on requested version" case symmetrically with
			// the latest path so the caller gets (nil, nil) → "already-latest".
			newer, err := update.IsNewerVersion(cfg.Version, rel.TagName)
			if err != nil {
				return nil, err
			}
			if !newer {
				return nil, nil
			}
			return rel, nil
		}
	}
	if cfg.Download == nil {
		cfg.Download = func(release *update.Release, destPath string) error {
			return update.NewClientWithTimeout(cfg.Version, 5*time.Minute).DownloadAndVerify(release, destPath)
		}
	}
	if cfg.Apply == nil {
		cfg.Apply = update.ApplyUpdate
	}
	if cfg.PendingPath == "" {
		cfg.PendingPath = update.GetPendingBinaryPath()
	}
	if cfg.IsService == nil {
		cfg.IsService = defaultIsService
	}
	if cfg.Restart == nil {
		cfg.Restart = update.RestartProcess
	}
	if cfg.IdlePollInterval <= 0 {
		cfg.IdlePollInterval = 500 * time.Millisecond
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 2 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	if cfg.RecordState == nil {
		cfg.RecordState = func(oldVersion, newVersion string) {
			state, err := update.LoadState()
			if err != nil {
				return
			}
			update.RecordUpdate(state, oldVersion, newVersion)
			state.AvailableUpdate = ""
			update.UpdateLastCheck(state)
			_ = update.SaveState(state)
		}
	}
	return &AgentUpdateHandler{cfg: cfg}
}

// CanHandle reports whether this handler processes the given job type.
func (h *AgentUpdateHandler) CanHandle(jobType string) bool {
	return jobType == JobTypeAgentUpdate
}

// Execute installs the requested (or latest) agent release and, when running as
// a managed service, arms a graceful self-restart that fires only after this
// job's result is published and acked. See the package doc for the ordering
// guarantee.
func (h *AgentUpdateHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	// Privilege gate: AGENT_UPDATE must arrive on the per-node stream, not the
	// shared org pool. isPerNodeStream reports a structural reason on failure so
	// the dispatcher surfaces "why" rather than hanging.
	if !isPerNodeStream(job.SourceQueue) {
		return h.failure(fmt.Errorf(
			"AGENT_UPDATE refused: must be dispatched to the per-node stream, got source queue %q", job.SourceQueue)), nil
	}

	target := payloadString(job.Payload, "target_version")
	h.cfg.Log("AGENT_UPDATE: checking for release (target=%q, current=%s)", target, h.cfg.Version)

	release, err := h.cfg.GetRelease(target)
	if err != nil {
		return h.failure(fmt.Errorf("update check failed: %w", err)), nil
	}
	if release == nil {
		// Already on the requested/latest version — a structured, non-error
		// terminal state rather than a hang or a failure.
		h.cfg.Log("AGENT_UPDATE: already on %s; nothing to do", h.cfg.Version)
		return h.success(map[string]any{
			"updated":     false,
			"reason":      "already-latest",
			"old_version": h.cfg.Version,
			"new_version": h.cfg.Version,
		}), nil
	}

	h.cfg.Log("AGENT_UPDATE: downloading %s", release.TagName)
	if err := h.cfg.Download(release, h.cfg.PendingPath); err != nil {
		return h.failure(fmt.Errorf("download/verify failed for %s: %w", release.TagName, err)), nil
	}

	h.cfg.Log("AGENT_UPDATE: installing %s", release.TagName)
	if err := h.cfg.Apply(h.cfg.PendingPath); err != nil {
		// Apply validates the new binary and rolls back internally on failure, so
		// the current version keeps running.
		return h.failure(fmt.Errorf("install failed for %s (kept %s): %w", release.TagName, h.cfg.Version, err)), nil
	}

	h.cfg.RecordState(h.cfg.Version, release.TagName)

	isService := h.cfg.IsService()
	h.cfg.Log("AGENT_UPDATE: installed %s (was %s); service=%v", release.TagName, h.cfg.Version, isService)

	result := map[string]any{
		"updated":     true,
		"old_version": h.cfg.Version,
		"new_version": release.TagName,
		"restarting":  isService,
	}
	if !isService {
		// Foreground / interactive run: don't kill the operator's session. Report
		// that a manual restart is needed to load the new binary.
		result["reason"] = "updated, restart required (not running as a managed service)"
		return h.success(result), nil
	}

	// Managed service: arm the restart BEFORE returning. This job is still
	// counted active (its defers have not run yet), so the goroutine cannot fire
	// until ActiveJobs()==0 — which is only reached after the runner publishes
	// this result and acks the job. Draining first closes the pickup race.
	h.armRestart(ctx, release.TagName)
	return h.success(result), nil
}

// armRestart launches the background goroutine that waits for the node to go
// idle (proving this job's result was published + acked) and then triggers a
// graceful restart. It returns immediately so Execute can return its result.
func (h *AgentUpdateHandler) armRestart(ctx context.Context, newVersion string) {
	if h.cfg.Drain != nil {
		h.cfg.Drain()
	}
	go func() {
		if err := h.waitForIdle(ctx); err != nil {
			h.cfg.Log("AGENT_UPDATE: %v; restarting anyway to load %s", err, newVersion)
		}
		h.cfg.Log("AGENT_UPDATE: restarting to load %s", newVersion)
		if err := h.cfg.Restart(); err != nil {
			// The new binary is already in place; the supervisor will pick it up
			// on the next natural restart. Log and keep running on the old image.
			h.cfg.Log("AGENT_UPDATE: restart failed: %v (new binary loads on next restart)", err)
		}
	}()
}

// waitForIdle blocks until ActiveJobs reports 0, ctx is cancelled, or the idle
// timeout elapses. A nil ActiveJobs is treated as immediately idle.
func (h *AgentUpdateHandler) waitForIdle(ctx context.Context) error {
	if h.cfg.ActiveJobs == nil {
		return nil
	}
	deadline := time.Now().Add(h.cfg.IdleTimeout)
	ticker := time.NewTicker(h.cfg.IdlePollInterval)
	defer ticker.Stop()
	for {
		if h.cfg.ActiveJobs() == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %d in-flight job(s)", h.cfg.IdleTimeout, h.cfg.ActiveJobs())
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled while waiting for idle")
		case <-ticker.C:
		}
	}
}

func (h *AgentUpdateHandler) success(output map[string]any) *JobResult {
	return &JobResult{Status: JobStatusSuccess, Output: output}
}

func (h *AgentUpdateHandler) failure(err error) *JobResult {
	return &JobResult{
		Status: JobStatusFailure,
		Error:  err,
		Output: map[string]any{"error": err.Error()},
	}
}

// isPerNodeStream reports whether a source queue name is a per-node shell
// stream (jobs:v1:shell:org_<id>:node:<nodeid>) rather than the shared org
// pool. The per-node marker is the ":node:" segment appended by the platform
// when routing a job at a specific node (see internal/worker AddQueue and the
// aceteam dispatcher).
func isPerNodeStream(sourceQueue string) bool {
	return strings.Contains(sourceQueue, ":node:")
}

// payloadString reads a string value from a job payload, coercing common
// JSON-decoded types (numbers arrive as float64). Missing/empty yields "".
func payloadString(payload map[string]any, key string) string {
	v, ok := payload[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// defaultIsService reports whether we are running under a managed service
// supervisor. The service unit/plist/SCM all set CITADEL_SERVICE=true (see
// internal/service).
func defaultIsService() bool {
	return os.Getenv("CITADEL_SERVICE") == "true"
}

// Ensure AgentUpdateHandler implements JobHandler.
var _ JobHandler = (*AgentUpdateHandler)(nil)
