// cmd/work.go
package cmd

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/apps"
	"github.com/aceteam-ai/citadel-cli/internal/capabilities"
	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/footprint"
	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/heartbeat"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/nodestate"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/power"
	"github.com/aceteam-ai/citadel-cli/internal/provision"
	"github.com/aceteam-ai/citadel-cli/internal/reconcile"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/service"
	internalServices "github.com/aceteam-ai/citadel-cli/internal/services"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/terminal"
	"github.com/aceteam-ai/citadel-cli/internal/tlscert"
	"github.com/aceteam-ai/citadel-cli/internal/tmux"
	"github.com/aceteam-ai/citadel-cli/internal/tmuxinstall"
	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/aceteam-ai/citadel-cli/internal/usage"
	"github.com/aceteam-ai/citadel-cli/internal/websockify"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/aceteam-ai/citadel-cli/internal/workflow"
	"github.com/aceteam-ai/citadel-cli/internal/worklock"
	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	workRedisURL   string
	workRedisPass  string
	workQueue      string
	workGroup      string
	workPollMs     int
	workMaxRetries int

	// Status server flags
	workStatusPort int
	workBaseURL    string
	workAPIKey     string
	workNodeName   string

	// SSH key sync flags
	workSSHSync     bool
	workSSHSyncMins int

	// Redis status publishing flags
	workRedisStatus   bool
	workDeviceCode    string
	workStatusChannel string

	// Terminal server flags
	workTerminal      bool
	workNoTerminal    bool
	workTerminalHost  string
	workTerminalPort  int
	workTerminalDebug bool

	// Service auto-start flags
	workNoServices   bool
	workWaitServices bool

	// Update check flag
	workNoUpdate bool

	// Footprint sampler flag
	workNoFootprint bool

	// Single-instance guard flag (issues #443 / #435): when true, skip the
	// per-node worker lock so a second worker may run intentionally (e.g. a debug
	// direct-Redis worker alongside the API-mode one in development).
	workNoSingleInstance bool

	// Discover-and-attach overrides (issue #524): when a worker is already
	// running, --attach forces the friendly status banner (even off a TTY) and
	// --no-attach forces the legacy exit-1 refusal. Default is TTY-gated.
	workAttach   bool
	workNoAttach bool

	// Node-key renewer flag (epic #4583): disable the background loop that
	// refreshes the Headscale node key before expiry while online.
	workNoKeyRenew bool

	// Auto-update (periodic self-update) flags
	workAutoUpdate         bool
	workAutoUpdateInterval string

	// Capability detection flags
	workCapabilities string
	workAutoDetect   bool

	// Concurrency flag
	workMaxConcurrency int

	// Workspace directory for file-operation handlers
	workWorkspaceDir string

	// Allow read-only file handlers to access paths outside the workspace
	workAllowReadOutsideWorkspace bool

	// Gateway flags
	workGateway          bool
	workNoGateway        bool
	workGatewayPort      int
	workGatewayBind      string
	workGatewayVNC       int
	workGatewayEmbedding int
	workGatewayNoTLS     bool
	workGatewayCertDir   string
)

var workCmd = &cobra.Command{
	Use:   "work",
	Short: "Start services and run the job worker (primary node command)",
	Long: `Unified Citadel worker that starts services and processes jobs.

This is the primary command for running a Citadel node. It:
  1. Auto-starts services from manifest (use --no-services to skip)
  2. Connects to the AceTeam API and consumes jobs via secure proxy
  3. Reports status via pub/sub (enabled by default)
  4. Subscribes to real-time config updates
  5. Starts HTTPS gateway on port 8443 (use --no-gateway to skip)
  6. Starts terminal server on port 7860 (use --no-terminal to skip)

Run 'citadel init' first to authenticate and configure the node.

Examples:
  # Run after citadel init (recommended — gateway + terminal enabled)
  citadel work

  # Disable the HTTPS gateway
  citadel work --no-gateway

  # Disable terminal access
  citadel work --no-terminal

  # Worker only (no gateway, no terminal)
  citadel work --no-gateway --no-terminal

  # Disable status publishing
  citadel work --redis-status=false

  # Run without auto-starting services
  citadel work --no-services`,
	Run: runWork,
}

// Connect-retry backoff bounds. A failed control-plane connect is recoverable,
// so we retry in-process rather than exiting and letting systemd storm.
// These are vars (not consts) so tests can shrink them without real-time sleeps.
var (
	connectBackoffInitial = 2 * time.Second
	connectBackoffMax     = 2 * time.Minute
	// connectRateLimitChunk bounds a SINGLE sleep even when the server asks for
	// a very long wait (e.g. retry_after: 86400). We sleep in chunks and re-check
	// ctx between them so a 24h reset never blocks shutdown -- but we keep
	// sleeping until the full server-requested interval has elapsed before
	// retrying, so we never poll tighter than retry_after (no hammering, #443).
	connectRateLimitChunk = 90 * time.Second
)

// apiConnector is the subset of APISource that connectWithBackoff needs.
// Kept as an interface so tests can exercise the backoff loop without a live API.
type apiConnector interface {
	Connect(ctx context.Context) error
}

// connectWithBackoff retries source.Connect until it succeeds or ctx is
// cancelled. Transient failures use exponential backoff with jitter; a 429
// rate-limit response is honored via its retry_after/reset_at hint (capped per
// sleep so shutdown stays responsive). Returns a non-nil error only when the
// context is cancelled (clean shutdown), never on a connect failure -- the
// process stays up and keeps retrying rather than dying into a restart storm.
func connectWithBackoff(ctx context.Context, source apiConnector) error {
	backoff := connectBackoffInitial
	attempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		attempt++
		err := source.Connect(ctx)
		if err == nil {
			if attempt > 1 {
				Log("connected to Redis API after %d attempt(s)", attempt)
			}
			return nil
		}

		// If the context was cancelled mid-connect, this is a clean shutdown.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Decide how long to wait before the next attempt.
		if rle, ok := redisapi.AsRateLimitError(err); ok {
			// Honor the server's backoff IN FULL: sleep the whole requested
			// interval before retrying so we never poll tighter than retry_after
			// (that tight-loop is exactly what burned the daily quota, #443). The
			// sleep is chunked and ctx-aware so a 24h reset still yields instantly
			// to shutdown. Reset the generic backoff -- the server is the
			// authority here.
			wait := rle.Wait(time.Now())
			if wait <= 0 {
				wait = backoff
			}
			backoff = connectBackoffInitial
			Log("Redis API rate limited (limit=%d window=%q); honoring server backoff of %s before retry: %v",
				rle.Limit, rle.Window, wait, err)
			if !sleepUntilCtx(ctx, time.Now().Add(wait)) {
				return ctx.Err()
			}
			continue
		}

		// Generic transient failure: exponential backoff with jitter.
		sleep := jitter(backoff)
		Log("Redis API connect failed (attempt %d), retrying in %s: %v", attempt, sleep, err)
		backoff *= 2
		if backoff > connectBackoffMax {
			backoff = connectBackoffMax
		}
		if !sleepCtx(ctx, sleep) {
			return ctx.Err()
		}
	}
}

// sleepUntilCtx sleeps until deadline, waking in bounded chunks so it stays
// responsive to ctx cancellation even for very long waits (e.g. a 24h 429
// reset). Returns true if the deadline was reached, false if ctx was cancelled.
func sleepUntilCtx(ctx context.Context, deadline time.Time) bool {
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ctx.Err() == nil
		}
		chunk := remaining
		if chunk > connectRateLimitChunk {
			chunk = connectRateLimitChunk
		}
		if !sleepCtx(ctx, chunk) {
			return false
		}
	}
}

// jitter returns d perturbed by +/-20% to avoid a thundering herd of nodes
// retrying in lockstep after a shared backend blip.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	delta := float64(d) * 0.2
	return time.Duration(float64(d) - delta + rand.Float64()*2*delta)
}

// sleepCtx sleeps for d or until ctx is cancelled. Returns true if the full
// sleep elapsed, false if the context was cancelled first (so backoff loops
// stay immediately responsive to SIGTERM / graceful shutdown).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func runWork(cmd *cobra.Command, args []string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Single-instance guard (issues #443 / #435). Refuse to start a second worker
	// for the same node so a stale duplicate can never run beside the systemd unit.
	// Duplicate workers split job routing non-deterministically, double the
	// control-plane request volume (contributing to the 429 self-DoS in #443), and
	// race on the shared tsnet state dir — the amplifier behind the identity churn.
	// Keyed to the resolved network state dir, so every invocation on this box that
	// converges on the same node identity also converges on the same lock. The lock
	// is an OS advisory lock, released by the kernel on process death, so a crashed
	// worker never blocks its own restart. Skipped with --no-single-instance for
	// intentional side-by-side runs (e.g. a second debug-redis worker in dev).
	if !workNoSingleInstance {
		lock, lockErr := worklock.Acquire(network.GetStateDir(), Version, Log)
		if lockErr != nil {
			var running *worklock.ErrAlreadyRunning
			if errors.As(lockErr, &running) {
				// Discover-and-attach (issue #524, increment 1): on an interactive
				// TTY, print the running worker's status instead of a bare error
				// (docker-daemon style). Non-TTY (systemd, scripts) keeps today's
				// exit-1 refusal UNCHANGED so a misconfigured double systemd unit
				// still fails visibly. --attach / --no-attach override the heuristic.
				if decideAttach(stdoutIsTTY(), workAttach, workNoAttach) {
					renderWorkAttach(network.GetStateDir(), running)
					os.Exit(0)
				}
				fmt.Fprintf(os.Stderr, "Error: %v\n", running)
				fmt.Fprintln(os.Stderr, "\nAnother 'citadel work' is already serving this node.")
				fmt.Fprintln(os.Stderr, "Stop it first (e.g. 'systemctl --user stop citadel' or kill the PID above),")
				fmt.Fprintln(os.Stderr, "or pass --no-single-instance to run a second worker intentionally.")
				os.Exit(1)
			}
			// Non-contention lock error (e.g. unwritable dir): warn but do not block
			// the worker — the guard is defense in depth, not a hard prerequisite.
			fmt.Fprintf(os.Stderr, "   - Warning: could not acquire single-instance lock: %v\n", lockErr)
		} else {
			Log("acquired single-instance worker lock (%s)", lock.Path())
			defer lock.Release()
		}
	}

	// Managed services this worker started (populated by the async startup
	// goroutine below, read by the shutdown hook). Guarded by a mutex because
	// the signal handler may fire while the startup goroutine is still filling
	// it in — we only ever want to tear down services we actually launched.
	var (
		startedServicesMu sync.Mutex
		startedServices   []startedService
	)
	recordStartedServices := func(svcs []startedService) {
		startedServicesMu.Lock()
		startedServices = append(startedServices, svcs...)
		startedServicesMu.Unlock()
	}
	teardownStartedServices := func() {
		startedServicesMu.Lock()
		svcs := startedServices
		startedServices = nil
		startedServicesMu.Unlock()
		stopManagedServices(svcs)
	}
	// Tear down managed services on any exit path (signal handler below also
	// invokes this before the watchdog fires; the second call is a no-op because
	// the slice is drained on first use).
	defer teardownStartedServices()

	// Apply --no-gateway / --no-terminal overrides (take precedence over defaults)
	if workNoGateway {
		workGateway = false
	}
	if workNoTerminal {
		workTerminal = false
	}

	// Setup signal handling. The first signal cancels the root context so every
	// subsystem (worker runner, status/terminal/gateway servers, heartbeat,
	// auto-updater, usage syncer) observes cancellation and unwinds. As a
	// bounded safety net (issue #312), a watchdog force-exits if graceful
	// shutdown does not complete within the grace period, and a second signal
	// force-exits immediately — so a misbehaving goroutine can never leave an
	// orphaned node holding the VPN identity or the terminal-server port.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Println("\n   - Received shutdown signal...")
		// Kill any managed co-browse Chromium so a headed browser does not orphan
		// across worker restarts (the persistent profile keeps logins; only the
		// live process is torn down). No-op when no session was ever started.
		if err := platform.GetCobrowseManager().Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "   - Warning: co-browse browser stop: %v\n", err)
		}
		// Cancel first so the async service-startup goroutine stops launching any
		// further services.
		cancel()
		// Arm the watchdog BEFORE tearing down managed services. Service teardown
		// shells out to `docker compose down` (potentially several times) and a
		// wedged docker daemon could hang this handler; the watchdog (second
		// signal + grace-period force-exit, issue #312) must already be running so
		// a stuck teardown can never leave an orphaned node holding the VPN
		// identity or the terminal-server port.
		go func() {
			select {
			case <-sigs:
				fmt.Fprintln(os.Stderr, "   - Second signal received; force-exiting")
				os.Exit(1)
			case <-time.After(shutdownGracePeriod):
				fmt.Fprintf(os.Stderr, "   - Shutdown did not complete within %s; force-exiting\n", shutdownGracePeriod)
				os.Exit(1)
			}
		}()
		// Tear down the services we started. Like co-browse, this stops the
		// containers/processes we spawned but preserves their data (no `-v`), and
		// only touches services this worker launched.
		teardownStartedServices()
	}()

	// Note: Update check is now handled by root.go's PersistentPreRun

	// Keep the machine awake while serving the Fabric, but only when the
	// operator has opted in (keep_awake_on_ac) and only on AC power. The
	// monitor holds a process-scoped OS sleep assertion that is released on
	// battery and on exit, so we never override the user's power policy without
	// consent or leave the machine permanently un-sleepable. Default is OFF.
	keepAwake := config.LoadKeepAwake(platform.ConfigDir())
	keepAwakeMonitor := power.NewMonitor(
		keepAwake.KeepAwakeOnAC,
		power.WithTransitionHook(func(active bool, src power.Source) {
			if active {
				fmt.Printf("   - Keep-awake: on (%s)\n", src)
			} else {
				fmt.Printf("   - Keep-awake: off (%s)\n", src)
			}
		}),
	)
	go keepAwakeMonitor.Run(ctx)
	// Release the assertion synchronously on shutdown, before runWork returns,
	// so the OS inhibitor process is never orphaned past Citadel's lifetime.
	defer keepAwakeMonitor.Stop()

	// Re-materialize managed systemd unit files so unit-template changes in this
	// binary (e.g. the #444 crash-restart-storm hardening) reach nodes deployed
	// by an older binary. The primary upgrade path (auto-update re-exec) swaps
	// the binary in place and never re-runs install.sh, so the on-disk unit would
	// otherwise keep the old 10s-restart-storm policy forever. This is the unit
	// analogue of the compose refresh below (#426). Idempotent: a unit already
	// carrying the hardening is left untouched with no daemon-reload churn. We do
	// NOT restart the worker here -- the new policy applies on the next restart.
	if rewritten, err := service.RematerializeManagedUnits(func(format string, args ...any) {
		Log(format, args...)
	}); err != nil {
		Log("unit-refresh: sweep error: %v", err)
	} else if len(rewritten) > 0 {
		Log("unit-refresh: refreshed managed units: %s", strings.Join(rewritten, ", "))
	}

	// Refresh citadel-owned embedded compose files from the binary's templates
	// when the version changed since this node last materialized them (#426).
	// Must run BEFORE startManagedServices so the fresh templates (host-port fix
	// etc.) are what compose brings up. Version-gated => a cheap no-op on an
	// unchanged boot. Skipped entirely with --no-services.
	if !workNoServices {
		if _, configDir, err := findAndReadManifest(); err == nil {
			refreshManagedComposeFiles(configDir)
		}
	}

	// Start managed services from the manifest (unless --no-services is set).
	//
	// Job serving is the node's core reason to exist and must never be gated on
	// optional heavy services (e.g. vLLM pulling an image or loading a model onto
	// the GPU). By default we launch services in a background goroutine and press
	// on to VPN connect + job-stream subscription immediately, so the node comes
	// online and serves jobs while services are still warming up (issue #384).
	// Per-service status is surfaced by the status collector, which polls actual
	// container state, so a slow or failed service simply reports as not-running
	// rather than blocking startup. Use --wait-services to restore the old
	// blocking behavior (subscribe only after every service is up).
	switch {
	case workNoServices:
		// Skip service startup entirely.
	case workWaitServices:
		recordStartedServices(startManagedServices(ctx))
	default:
		go func() {
			recordStartedServices(startManagedServices(ctx))
		}()
	}

	// Resolve node capabilities early (used for queue routing and heartbeat)
	var nodeCaps *capabilities.NodeCapabilities
	workManifest, workConfigDir, _ := findAndReadManifest()
	if workManifest != nil && workManifest.Capabilities != nil {
		nodeCaps = manifestToNodeCapabilities(workManifest.Capabilities)
		fmt.Println("   - Capabilities: loaded from manifest")
	} else {
		nodeCaps = capabilities.DetectNodeCapabilities()
		fmt.Println("   - Capabilities: auto-detected")
	}
	if nodeCaps != nil && nodeCaps.GPU != nil && len(nodeCaps.GPU.Devices) > 0 {
		fmt.Printf("   - GPUs: %d detected", nodeCaps.GPU.Count)
		fmt.Printf(" (%s", nodeCaps.GPU.Devices[0].Name)
		if nodeCaps.GPU.Devices[0].VRAMTag != "" {
			fmt.Printf(", %s", strings.ToUpper(nodeCaps.GPU.Devices[0].VRAMTag))
		}
		fmt.Printf(")\n")
	}
	if len(nodeCaps.Engines) > 0 {
		fmt.Printf("   - Engines: %s\n", strings.Join(nodeCaps.Engines, ", "))
	}
	if len(nodeCaps.Tags) > 0 {
		Debug("capability tags: %v", nodeCaps.Tags)
	}

	// Detect Proxmox hypervisor (host fact, checked regardless of manifest vs auto-detect)
	var proxmoxInfo *platform.ProxmoxInfo
	if pveInfo, err := platform.DetectProxmox(); err == nil && pveInfo.IsInstalled {
		proxmoxInfo = pveInfo
		nodeCaps.Tags = append(nodeCaps.Tags, "hypervisor:proxmox")
		fmt.Printf("   - Hypervisor: Proxmox VE")
		if pveInfo.Version != "" {
			fmt.Printf(" (%s)", pveInfo.Version)
		}
		fmt.Printf("\n")
		if pveInfo.VMCount > 0 || pveInfo.CTCount > 0 {
			fmt.Printf("   - Guests: %d VMs, %d containers\n", pveInfo.VMCount, pveInfo.CTCount)
		}
	}

	// Log macOS developer toolchains (tags already added by DetectNodeCapabilities)
	for _, tag := range nodeCaps.Tags {
		switch {
		case strings.HasPrefix(tag, "tool:xcode:"):
			fmt.Printf("   - Toolchain: Xcode %s\n", strings.TrimPrefix(tag, "tool:xcode:"))
		case tag == "tool:xcode":
			// Only log if there's no versioned tag (avoid duplicate)
			hasVersion := false
			for _, t := range nodeCaps.Tags {
				if strings.HasPrefix(t, "tool:xcode:") {
					hasVersion = true
					break
				}
			}
			if !hasVersion {
				fmt.Println("   - Toolchain: Xcode")
			}
		case tag == "tool:android-sdk":
			fmt.Println("   - Toolchain: Android SDK")
		}
	}

	// Convert capabilities to status types for heartbeat
	var statusCaps *status.NodeCapabilities
	if nodeCaps != nil {
		statusCaps = &status.NodeCapabilities{
			Engines: nodeCaps.Engines,
			Tags:    nodeCaps.Tags,
		}
		if nodeCaps.GPU != nil {
			for _, dev := range nodeCaps.GPU.Devices {
				statusCaps.GPUs = append(statusCaps.GPUs, status.GPUCapability{
					Name:    dev.Name,
					VRAMMb:  dev.VRAMMb,
					Tag:     dev.Tag,
					VRAMTag: dev.VRAMTag,
				})
			}
		}
		if proxmoxInfo != nil {
			statusCaps.Hypervisor = &status.HypervisorInfo{
				Type:      "proxmox",
				Version:   proxmoxInfo.Version,
				NodeName:  proxmoxInfo.NodeName,
				NodeCount: proxmoxInfo.NodeCount,
				VMCount:   proxmoxInfo.VMCount,
				CTCount:   proxmoxInfo.CTCount,
			}
		}
	}

	// Resolve job source: API mode (device_api_token) vs direct Redis (legacy)
	Debug("resolving job source mode...")

	// Load device config from file
	deviceConfig := getDeviceConfigFromFile()
	if deviceConfig != nil {
		Debug("config loaded: device_api_token=%q, api_base_url=%q, redis_url=%q",
			maskToken(deviceConfig.DeviceAPIToken),
			deviceConfig.APIBaseURL,
			deviceConfig.RedisURL)
	} else {
		Debug("config file not found or empty")
	}

	// Check for API mode: device_api_token takes precedence (unless forced to direct Redis)
	var source worker.JobSource
	var streamFactory func(job *worker.Job) worker.StreamWriter
	var useAPIMode bool
	var apiSource *worker.APISource               // Keep reference for heartbeat
	var setNodeMeta func(nodeID, nodeName string) // Set after node identity is resolved

	// Live worker introspection state for the out-of-band control path
	// (issue #236). Created here so the same pointer is shared by the runner
	// (job counts, poll time) and the status server's /agent/* endpoints, which
	// answer over the tsnet mesh even when Redis job consumption is broken.
	workerState := worker.NewWorkerState()

	if workRedisURL == "" && deviceConfig != nil && deviceConfig.DeviceAPIToken != "" {
		// API mode: use secure HTTP API instead of direct Redis.
		Debug("using API mode (device_api_token found)")
		useAPIMode = true

		apiBaseURL := deviceConfig.APIBaseURL
		if apiBaseURL == "" {
			apiBaseURL = authServiceURL // Default to auth service URL
		}
		Debug("API base URL: %s", apiBaseURL)

		// Fetch worker config from API (queue, org).
		// This replaces the need for WORKER_QUEUE env vars.
		// Consumer group is resolved earlier from node identity.
		if workQueue == "" || deviceConfig.OrgID == "" {
			tempClient := redisapi.NewClient(redisapi.ClientConfig{
				BaseURL:   apiBaseURL,
				Token:     deviceConfig.DeviceAPIToken,
				DebugFunc: Debug,
			})
			workerCfg, err := tempClient.FetchWorkerConfig(ctx)
			if err != nil {
				Debug("worker-config fetch failed: %v (using defaults)", err)
			} else if workerCfg != nil {
				Debug("worker-config: queue=%s, group=%s, org=%s",
					workerCfg.Queue, workerCfg.ConsumerGroup, workerCfg.OrgID)
				if workQueue == "" && workerCfg.Queue != "" {
					workQueue = workerCfg.Queue
				}
				// Store org_id from API if not already in config
				if deviceConfig.OrgID == "" && workerCfg.OrgID != "" {
					deviceConfig.OrgID = workerCfg.OrgID
				}
			} else {
				Debug("worker-config: endpoint not available, using defaults")
			}
			_ = tempClient.Close()
		}

		// Build queue list: primary queue + per-org shell queue.
		// Ensure a base queue is always present so that appending
		// the shell queue does not suppress the NewAPISource default.
		var apiQueueNames []string
		if workQueue != "" {
			apiQueueNames = append(apiQueueNames, workQueue)
		}
		orgID := deviceConfig.OrgID
		if orgID != "" {
			shellQueue := shellQueueName(orgID)
			if len(apiQueueNames) == 0 {
				apiQueueNames = []string{"jobs:v1:cpu-general"}
			}
			apiQueueNames = append(apiQueueNames, shellQueue)
			Debug("shell queue: %s", shellQueue)
		}

		apiSource = worker.NewAPISource(worker.APISourceConfig{
			BaseURL:       apiBaseURL,
			Token:         deviceConfig.DeviceAPIToken,
			QueueNames:    apiQueueNames,
			ConsumerGroup: workGroup,
			BlockMs:       workPollMs,
			MaxAttempts:   workMaxRetries,
			DebugFunc:     Debug,
			// Route the source's info logs (subscribed queues, consumer group,
			// per-node AddQueue) through Log() so they land in latest.log. The
			// default sink is stdout, which the worker's log file does not
			// capture -- masking whether the per-node shell stream was actually
			// subscribed during a live node-routing test (issue #3924).
			LogFn: func(_ string, msg string) { Log("%s", msg) },
		})

		// Connect early so client is available for heartbeat.
		//
		// A failed control-plane connect is expected and recoverable (auth blip,
		// mesh churn, backend deploy, rate limit), NOT fatal. Exiting here would
		// let systemd relaunch us every ~10s; each restart re-pings the same
		// endpoint, and if it is rate limited (HTTP 429) that restart storm burns
		// the node's daily Redis-API quota and hard-locks it for ~24h -- the node
		// self-DoSes (issue #443). Instead retry IN-PROCESS with exponential
		// backoff + jitter, honoring any 429 retry_after/reset_at, and stay
		// responsive to shutdown via ctx.
		if err := connectWithBackoff(ctx, apiSource); err != nil {
			// The only way this returns an error is context cancellation
			// (SIGTERM / graceful shutdown), which is a clean exit, not a crash.
			Log("worker connect aborted: %v", err)
			return
		}

		// Enable WebSocket for real-time pub/sub (heartbeat, streaming responses)
		if err := apiSource.Client().EnableWebSocket(ctx); err != nil {
			// WebSocket is optional - fall back to HTTP
			Debug("WebSocket not available, using HTTP for pub/sub: %v", err)
		} else {
			Debug("WebSocket enabled for real-time pub/sub")
		}

		source = apiSource
		streamFactory = worker.CreateAPIStreamWriterFactory(ctx, apiSource)
		setNodeMeta = func(nodeID, nodeName string) {
			apiSource.Client().SetNodeMeta(nodeID, nodeName)
		}

		fmt.Println("   - Mode: Redis API (secure)")
		if deviceConfig.UserEmail != "" {
			if deviceConfig.UserName != "" {
				fmt.Printf("   - Account: %s (%s)\n", deviceConfig.UserEmail, deviceConfig.UserName)
			} else {
				fmt.Printf("   - Account: %s\n", deviceConfig.UserEmail)
			}
		}
	} else if workRedisURL != "" || (deviceConfig != nil && deviceConfig.RedisURL != "") {
		// Direct Redis mode: either --debug-redis-url flag or legacy redis_url from config
		if workRedisURL == "" {
			workRedisURL = deviceConfig.RedisURL
			fmt.Fprintln(os.Stderr, "WARNING: Using legacy Redis URL from config. Run 'citadel init' again to upgrade to API mode.")
		} else {
			fmt.Fprintln(os.Stderr, "WARNING: Direct Redis mode is for debugging only. Run 'citadel init' for production use.")
		}
		Debug("using direct Redis mode")
		Debug("Redis URL: %s", workRedisURL)

		// Resolve queue names: explicit --queue takes priority, otherwise use capabilities
		var queueNames []string
		if workQueue != "" {
			// Explicit queue specified -- use it directly (backwards compat)
			queueNames = []string{workQueue}
		} else {
			// Use resolved node capabilities for queue routing
			var allCaps []capabilities.Capability

			// Add capabilities from auto-detected nodeCaps (already resolved above)
			if nodeCaps != nil && len(nodeCaps.Tags) > 0 {
				for _, tag := range nodeCaps.Tags {
					category := tag
					if idx := strings.Index(tag, ":"); idx > 0 {
						category = tag[:idx]
					}
					allCaps = append(allCaps, capabilities.Capability{Tag: tag, Category: category})
				}
			}

			// Also honor --capabilities flag for manual overrides
			if workCapabilities != "" {
				manual := capabilities.ParseTags(workCapabilities)
				allCaps = append(allCaps, manual...)
				for _, c := range manual {
					fmt.Printf("   - Manual tag: %s\n", c.Tag)
				}
			}

			if len(allCaps) > 0 {
				baseQueue := "jobs:v1:gpu-general"
				queueNames = capabilities.ResolveQueues(allCaps, baseQueue)
			}
		}

		// Add per-org shell queue if org_id is known.
		// Ensure the default base queue is present when no explicit queue
		// or capabilities were resolved (otherwise shell-only list would
		// suppress the default fallback inside NewRedisSource).
		orgID := ""
		if deviceConfig != nil {
			orgID = deviceConfig.OrgID
		}
		if orgID != "" {
			shellQ := shellQueueName(orgID)
			if len(queueNames) == 0 {
				queueNames = []string{"jobs:v1:gpu-general"}
			}
			queueNames = append(queueNames, shellQ)
			Debug("shell queue: %s", shellQ)
		}

		// Create Redis job source
		redisSource := worker.NewRedisSource(worker.RedisSourceConfig{
			URL:           workRedisURL,
			Password:      workRedisPass,
			QueueName:     workQueue,
			QueueNames:    queueNames,
			ConsumerGroup: workGroup,
			BlockMs:       workPollMs,
			MaxAttempts:   workMaxRetries,
			// Route source info logs (queues, consumer group, per-node AddQueue)
			// to latest.log instead of stdout so subscription activity is
			// visible in the worker log (issue #3924).
			LogFn: func(_ string, msg string) { Log("%s", msg) },
		})
		source = redisSource
		streamFactory = worker.CreateRedisStreamWriterFactory(ctx, redisSource)
		setNodeMeta = func(nodeID, nodeName string) {
			if c := redisSource.Client(); c != nil {
				c.SetNodeMeta(nodeID, nodeName)
			}
		}

		fmt.Println("   - Mode: Direct Redis (debug)")
	} else {
		// Neither API token nor debug Redis URL available
		fmt.Fprintf(os.Stderr, "Error: Node not initialized. Run 'citadel init' first.\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "  citadel init    Authenticate and connect to the AceTeam Network\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "For development/debugging, use --debug-redis-url to connect directly.\n")
		os.Exit(1)
	}

	// Log mode for debugging
	_ = useAPIMode

	// Create worker ID
	workerID := fmt.Sprintf("citadel-%s", uuid.New().String()[:8])

	// Ensure network connection is established (reconnects if state exists).
	// Uses attemptVPNRecovery (shared with 'citadel reconnect') to handle
	// stale state: IP-preserving reconnect first, then clear + fresh connect.
	network.SetLogf(Debug)
	Log("verifying network connection (state_dir=%s, has_state=%v)...",
		network.GetStateDir(), network.HasState())
	connected, err := network.VerifyOrReconnect(ctx)
	if err != nil && errors.Is(err, network.ErrStaleState) {
		Log("network state is stale after retries, attempting auto-recovery...")
		fmt.Println("   - VPN state is stale, attempting auto-recovery...")

		apiBaseURL := ""
		if deviceConfig != nil {
			apiBaseURL = deviceConfig.APIBaseURL
		}
		if apiBaseURL == "" {
			apiBaseURL = authServiceURL
		}

		result := recoverStaleVPN(ctx, deviceConfig, getWorkHostname(), apiBaseURL)
		connected = result.Connected
		if result.Connected {
			if result.IPPreserved {
				Log("VPN reconnected (IP preserved)")
				fmt.Println("   - VPN reconnected (IP preserved)")
			} else {
				Log("VPN reconnected (fresh state, new IP)")
				fmt.Println("   - VPN reconnected (fresh state)")
			}
		} else {
			if result.Err != nil {
				Log("VPN auto-recovery failed: %v", result.Err)
				fmt.Fprintf(os.Stderr, "   - Warning: VPN auto-recovery failed: %v\n", result.Err)
			}
			Log("VPN connection could not be restored automatically")
			fmt.Fprintln(os.Stderr, "   - Warning: VPN connection could not be restored automatically.")
			fmt.Fprintln(os.Stderr, "     Run 'citadel reconnect' or 'citadel login --authkey <key>' to fix.")
		}
	} else if err != nil {
		Log("network reconnect failed: %v", err)
	} else if connected {
		Log("network connected")
	} else {
		Log("network not configured (no saved state)")
	}

	// Get node name and Headscale node ID from network status
	nodeName := workNodeName
	var headscaleNodeID string // Headscale numeric node ID (e.g., "758")
	Log("resolving node name...")
	Debug("workNodeName flag: %q", workNodeName)
	Debug("CITADEL_NODE_NAME env: %q", os.Getenv("CITADEL_NODE_NAME"))
	if nodeName == "" {
		nodeName = os.Getenv("CITADEL_NODE_NAME")
	}
	if nodeName == "" {
		// Try to get the actual registered hostname from network status
		// This returns the Headscale-assigned name (e.g., "ubuntu-gpu-8gluaaom") not just the requested name
		if netStatus, err := network.GetGlobalStatus(ctx); err == nil && netStatus.Connected && netStatus.Hostname != "" {
			Debug("got hostname from network status: %s", netStatus.Hostname)
			nodeName = netStatus.Hostname
			if netStatus.NodeID != "" {
				headscaleNodeID = netStatus.NodeID
				Debug("got Headscale node ID from network status: %s", headscaleNodeID)
			}
		} else {
			// Fallback to local hostname
			hostname, _ := os.Hostname()
			Debug("using local hostname fallback: %s", hostname)
			nodeName = hostname
		}
	} else {
		// Even with an explicit node name, resolve the Headscale node ID
		headscaleNodeID = network.GetGlobalNodeID(ctx)
		if headscaleNodeID != "" {
			Debug("got Headscale node ID: %s", headscaleNodeID)
		}
	}
	Debug("final node name: %s", nodeName)
	if headscaleNodeID != "" {
		Debug("final Headscale node ID: %s", headscaleNodeID)
	}

	// The Headscale numeric node ID is required to subscribe to this node's
	// per-node shell stream (issues #3914/#3924). Right after a (re)connect the
	// tsnet netmap may not be populated yet, so the single resolution above can
	// return an empty ID even though the node is otherwise online and heartbeats
	// fine (the platform re-resolves the heartbeat by hostname). An empty ID
	// here permanently skips the per-node subscription for the worker's whole
	// lifetime, silently downgrading node-targeted jobs (terminal_exec, code_*,
	// file reads, node attachments) to the shared org stream where a peer node
	// can claim them. Retry a few times with a short backoff to close that
	// startup race before we build the queue list.
	if headscaleNodeID == "" {
		for attempt := 1; attempt <= 5; attempt++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt) * time.Second):
			}
			if id := network.GetGlobalNodeID(ctx); id != "" {
				headscaleNodeID = id
				Debug("resolved Headscale node ID on retry %d: %s", attempt, headscaleNodeID)
				break
			}
			Debug("Headscale node ID still empty on retry %d", attempt)
		}
		if headscaleNodeID == "" {
			Log("Headscale node ID unresolved after retries; node-targeted jobs will fall back to the shared org stream")
			fmt.Fprintln(os.Stderr, "   - Warning: Headscale node ID unresolved after retries; "+
				"node-targeted jobs will fall back to the shared org stream")
		}
	}

	// Log the final registration outcome for post-mortem diagnosis (issue #246).
	Log("registered: online=%v node_name=%s headscale_id=%s state_dir=%s",
		connected, nodeName, headscaleNodeID, network.GetStateDir())

	// Start the background resource-footprint sampler (issue #422). It appends a
	// lightweight per-service + node-level time-series to rotated, DuckDB-queryable
	// CSVs under ~/citadel-node/footprints/, so an idle service hoarding RSS/VRAM
	// leaves a queryable trail (queryable via `citadel footprints`). Cheap by
	// design: one container-stats exec + one GPU read per tick (default 60s), never
	// per-service. Disabled by --no-footprint or CITADEL_FOOTPRINT_INTERVAL<=0.
	startFootprintSampler(ctx, nodeName, workManifest)

	// Start the background node-key renewer (epic #4583). While the node is
	// healthy and online, it refreshes the Headscale node key before it expires,
	// re-authorizing in place with the node's own device token — so a long-lived
	// session never lapses and an ordinary restart re-establishes via the
	// no-authkey reconnect, never hitting the offline authkey-mint 403 path.
	// No-op in direct-Redis mode (no device token) or when disabled.
	startNodeKeyRenewer(ctx, deviceConfig)

	// Default consumer group to node identity when --group is not explicitly set.
	workGroup = resolveConsumerGroup(workGroup, headscaleNodeID, nodeName)
	Debug("consumer group: %s", workGroup)
	// Set node identity on the stream event publisher for operator attribution.
	// Note: We keep using nodeName (hostname) here rather than the Headscale
	// numeric ID, because the usage/earnings pipeline may key on hostname.
	// The Headscale numeric ID is passed in the heartbeat payload via
	// headscaleNodeId for the Python NodeStatusWorker to use directly.
	if setNodeMeta != nil {
		setNodeMeta(nodeName, nodeName)
		Debug("node meta set: node_id=%s, node_name=%s", nodeName, nodeName)
	}

	// Subscribe to this node's per-node shell stream so node-targeted MCP jobs
	// (terminal_exec, code_*, file reads, node attachments) reach ONLY this node
	// instead of being claimed by a greedy peer on the shared shell stream
	// (issue #3914). Keyed by the Headscale numeric node ID, which matches the
	// platform's fabric_node_status.node_id used for targeting. We still consume
	// the shared shell stream for untargeted work, so this is purely additive and
	// safe for older platform deployments that don't yet route per-node.
	//
	// AddQueue is a Redis-only concept, so it's accessed via an optional
	// interface rather than the JobSource contract (the Nexus HTTP source does
	// not implement it). Done before the run loop starts, so no locking needed.
	if headscaleNodeID != "" {
		perNodeOrgID := ""
		if deviceConfig != nil {
			perNodeOrgID = deviceConfig.OrgID
		}
		if perNodeOrgID == "" {
			if manifest, _, mErr := findAndReadManifest(); mErr == nil && manifest != nil {
				perNodeOrgID = manifest.Node.OrgID
			}
		}
		if perNodeOrgID != "" {
			perNodeQueue := nodeQueueName(perNodeOrgID, headscaleNodeID)
			switch src := source.(type) {
			case *worker.APISource:
				src.AddQueue(perNodeQueue)
			case *worker.RedisSource:
				if err := src.AddQueue(ctx, perNodeQueue); err != nil {
					fmt.Fprintf(os.Stderr, "   - Warning: per-node stream subscribe failed: %v\n", err)
				}
			}
			workerState.SetPerNodeQueue(perNodeQueue)
			fmt.Printf("   - Per-node shell stream: %s\n", perNodeQueue)
			Debug("per-node shell stream: %s", perNodeQueue)
		} else {
			fmt.Fprintln(os.Stderr, "   - Warning: per-node shell stream skipped (org id unknown); "+
				"node-targeted jobs will fall back to the shared org stream")
			Debug("per-node shell stream skipped: org id unknown")
		}
	} else {
		fmt.Fprintln(os.Stderr, "   - Warning: per-node shell stream skipped (Headscale node ID "+
			"unavailable); node-targeted jobs will fall back to the shared org stream")
		Debug("per-node shell stream skipped: Headscale node ID unavailable")
	}

	// Subscribe to the org-scoped meeting-notetaker queue when this node can
	// actually run a meeting bot. The backend routes MEETING_JOIN jobs to the
	// per-org tag queue jobs:v1:tag:meeting:org_{org_id} (a security fix rejected
	// the bare cross-org queue), so a node has to consume that exact stream for
	// auto-join dispatch to reach it (aceteam-ai/aceteam#5098).
	//
	// Gated on the node advertising the "meeting" capability tag rather than
	// subscribed unconditionally like the shell stream: a node lacking the
	// audio-capture stack, Chromium, or Xvfb cannot run a meeting bot, so
	// claiming the job would only fail it. The capability detector adds this tag
	// only when that stack is present, and the MEETING_JOIN handler ships behind
	// the same gate, so subscription, capability, and handler activate together.
	if nodeCaps != nil && hasCapabilityTag(nodeCaps.Tags, "meeting") {
		meetingOrgID := ""
		if deviceConfig != nil {
			meetingOrgID = deviceConfig.OrgID
		}
		if meetingOrgID == "" {
			if manifest, _, mErr := findAndReadManifest(); mErr == nil && manifest != nil {
				meetingOrgID = manifest.Node.OrgID
			}
		}
		if meetingOrgID != "" {
			meetingQueue := meetingQueueName(meetingOrgID)
			switch src := source.(type) {
			case *worker.APISource:
				src.AddQueue(meetingQueue)
			case *worker.RedisSource:
				if err := src.AddQueue(ctx, meetingQueue); err != nil {
					fmt.Fprintf(os.Stderr, "   - Warning: meeting queue subscribe failed: %v\n", err)
				}
			}
			fmt.Printf("   - Meeting notetaker queue: %s\n", meetingQueue)
			Debug("meeting queue: %s", meetingQueue)
		} else {
			fmt.Fprintln(os.Stderr, "   - Warning: meeting queue skipped (org id unknown); "+
				"meeting auto-join dispatch will not reach this node")
			Debug("meeting queue skipped: org id unknown")
		}
	}

	// Populate the worker introspection state with the resolved identity and
	// the full subscribed queue list (issue #236). This drives /agent/worker-status
	// and the doctor diagnosis below.
	{
		stateOrgID := ""
		if deviceConfig != nil {
			stateOrgID = deviceConfig.OrgID
		}
		consumerGroup := workGroup
		if consumerGroup == "" {
			consumerGroup = "citadel-workers"
		}
		var queues []string
		switch src := source.(type) {
		case *worker.APISource:
			queues = src.QueueNames()
		case *worker.RedisSource:
			queues = src.QueueNames()
		}
		workerState.SetIdentity(workerID, source.Name(), consumerGroup, headscaleNodeID, stateOrgID)
		workerState.SetQueues(queues)
	}

	// Open usage store for per-job compute tracking
	var usageStore *usage.Store
	if nodeDir, err := platform.DefaultNodeDir(""); err != nil {
		fmt.Fprintf(os.Stderr, "   - Warning: Usage tracking disabled (no node dir): %v\n", err)
	} else {
		usageDBPath := filepath.Join(nodeDir, "usage.db")
		store, err := usage.OpenStore(usageDBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "   - Warning: Usage tracking disabled: %v\n", err)
		} else {
			usageStore = store
			defer usageStore.Close()
			Debug("usage store: %s", usageDBPath)
		}
	}

	// Resolve API key and base URL (used by status server, SSH sync, and terminal)
	apiKey := workAPIKey
	if apiKey == "" {
		apiKey = os.Getenv("CITADEL_API_KEY")
	}

	baseURL := workBaseURL
	if baseURL == "" {
		baseURL = os.Getenv("ACETEAM_URL")
	}
	if baseURL == "" {
		baseURL = os.Getenv("HEARTBEAT_URL") // backward compat
	}
	if baseURL == "" && deviceConfig != nil && deviceConfig.APIBaseURL != "" {
		baseURL = deviceConfig.APIBaseURL
	}
	if baseURL == "" {
		baseURL = "https://aceteam.ai"
	}

	// When --gateway is set, auto-enable the status server on port 8080 so the
	// gateway has a working upstream. This must happen before the status server
	// block below so that the full status server (with token cache, VPN listener,
	// desktop API, etc.) is started correctly.
	if workGateway && workStatusPort == 0 {
		workStatusPort = 8080
	}

	// Live worker consume-loop liveness for the heartbeat (issue #548). Reads the
	// same *WorkerState the runner updates, so the heartbeat can surface a
	// "green but wedged" node (heartbeating from a separate goroutine while the
	// consume loop is blocked and draining nothing).
	workerLivenessFn := func() *status.WorkerLiveness {
		snap := workerState.Snapshot()
		return &status.WorkerLiveness{
			Consuming:         snap.Consuming,
			LastJobConsumedAt: snap.LastJobAt,
			LastPollAt:        snap.LastPollAt,
			InFlight:          snap.InFlight,
			Processed:         snap.Processed,
			Failed:            snap.Failed,
		}
	}

	// Create status collector (used by status server and Redis status publisher)
	var collector *status.Collector
	if workStatusPort > 0 {
		collector = status.NewCollector(status.CollectorConfig{
			NodeName:       nodeName,
			ConfigDir:      "",
			Services:       nil,
			Capabilities:   statusCaps,
			WorkerLiveness: workerLivenessFn,
		})
	}

	// Resolve workspace dir early — needed by both file-operation handlers and
	// the workflow executor, and the workflow executor must be created before the
	// status server starts (its AddRouteRegistrar closure references it).
	wsDir := resolveWorkspaceDir()

	// Workflow executor for WORKFLOW_RUN jobs (#105). Created here so the status
	// server's AddRouteRegistrar closure and the job handler share a single instance.
	wfExec := workflow.NewExecutor(workflow.ExecutorConfig{
		Shell: workflow.ShellConfig{WorkspaceDir: wsDir},
	})

	// Build the agent introspection & control providers (issue #236). These
	// back the status server's /agent/* endpoints, which the aceteam MCP server
	// wraps as citadel_* tools. They read the shared workerState and act on the
	// live source — never via the Redis job queue, so they work even when job
	// consumption is broken.
	agentProviders := buildAgentProviders(ctx, agentProviderDeps{
		state:           workerState,
		source:          source,
		nodeName:        nodeName,
		headscaleNodeID: headscaleNodeID,
		baseURL:         baseURL,
		deviceConfig:    deviceConfig,
	})

	// Start status server if enabled
	if workStatusPort > 0 {
		serverCfg := status.ServerConfig{
			Port:    workStatusPort,
			Version: Version,
			Agent:   agentProviders,
		}

		// Publish the gateway's self-signed leaf cert at GET /gateway-cert.pem so
		// the backend can trust it out-of-band and re-fetch on rotation. The path
		// is the same one the gateway block below serves from; empty when the
		// gateway runs --gateway-no-tls (endpoint then returns 204 = use http).
		if workGateway && !workGatewayNoTLS {
			serverCfg.GatewayCertPath = tlscert.CertPath(workGatewayCertDir)
		}

		// Wire up desktop API auth if org ID is available
		statusOrgID := ""
		if deviceConfig != nil {
			statusOrgID = deviceConfig.OrgID
		}
		if statusOrgID == "" {
			if manifest, _, err := findAndReadManifest(); err == nil {
				statusOrgID = manifest.Node.OrgID
			}
		}
		if statusOrgID != "" && baseURL != "" {
			statusAPIToken := ""
			if deviceConfig != nil {
				statusAPIToken = deviceConfig.DeviceAPIToken
			}
			serverCfg.TokenValidator = terminal.NewCachingTokenValidator(baseURL, statusOrgID, statusAPIToken, 30*time.Second)
			serverCfg.OrgID = statusOrgID
		}

		// Register desktop endpoints when permissions allow and auth is available.
		// The handlers perform per-request readiness checks, so VNC does not need
		// to be running at startup -- a VNC server started later will be picked up
		// automatically without restarting citadel work.
		permsForDesktop := config.LoadPermissions(platform.ConfigDir())
		if permsForDesktop.Desktop {
			if serverCfg.TokenValidator != nil {
				serverCfg.EnableDesktop = true
				Debug("desktop API enabled (per-request VNC readiness checks)")
				// Bundle VNC startup so the shared desktop works with zero
				// manual `citadel vnc enable` (#483). Idempotent, headless-safe,
				// loopback-bound; non-fatal so a missing x11vnc never blocks the worker.
				if verr := platform.EnsureGatewayVNC(platform.DefaultVNCPort); verr != nil {
					fmt.Fprintf(os.Stderr, "   - desktop VNC not auto-started: %v\n", verr)
				}
			} else {
				fmt.Fprintln(os.Stderr, "   - desktop permission granted but API disabled (no org ID for auth)")
			}
		}

		// Gate the MUTATING control endpoints (SSH-key injection) behind a
		// coordinator mTLS client certificate on a dedicated control listener
		// (issue #5028). Enabled only when a fabric CA bundle AND coordinator
		// identities are configured. If construction fails, we log and leave the
		// control listener OFF -- the mutating endpoints then stay refused on the
		// plaintext path (fail closed), never reverting to VPN-origin trust.
		controlPortForVPN := 0
		caBundle := strings.TrimSpace(os.Getenv("CITADEL_FABRIC_CA_BUNDLE"))
		if caBundle == "" {
			// Provision the fabric CA trust root so the mTLS control listener has
			// a trust anchor without manual env config (#5028). Fetched once at
			// startup from the coordinator; a cached bundle is reused on fetch
			// failure so a transient backend blip does not disable SSH-deploy on
			// an already-provisioned node. A CA rotation requires a restart to
			// re-fetch. Fails closed (listener stays off) only on a first-ever
			// cold start with no cache and no reachable backend.
			if bundlePath, berr := status.EnsureFabricCABundle(baseURL, platform.ConfigDir()); berr != nil {
				fmt.Fprintf(os.Stderr, "   - ⚠️ Fabric CA bundle unavailable (%v); mTLS control listener disabled, SSH-key injection refused\n", berr)
			} else {
				caBundle = bundlePath
			}
		}
		if caBundle != "" {
			coordinatorSANs := splitAndTrim(os.Getenv("CITADEL_COORDINATOR_SANS"))
			if len(coordinatorSANs) == 0 {
				// Default to the platform coordinator identity so upgraded nodes
				// allowlist the relay out of the box (#5028). Without at least one
				// SAN the verifier refuses to construct and the listener stays off.
				coordinatorSANs = []string{status.DefaultCoordinatorSAN}
			}
			verifier, verr := status.NewFabricCAVerifier(caBundle, coordinatorSANs)
			if verr != nil {
				fmt.Fprintf(os.Stderr, "   - ⚠️ Coordinator mTLS control listener NOT enabled (%v); SSH-key injection stays refused\n", verr)
			} else {
				var ctrlIPs []net.IP
				if ip4, _ := network.GetGlobalIPv4(); ip4 != "" {
					if ip := net.ParseIP(ip4); ip != nil {
						ctrlIPs = append(ctrlIPs, ip)
					}
				}
				ctrlCert, certErr := tlscert.EnsureCert(tlscert.Config{
					Hostname:    nodeName,
					IPAddresses: ctrlIPs,
					CertDir:     filepath.Join(platform.ConfigDir(), "control-tls"),
				})
				if certErr != nil {
					fmt.Fprintf(os.Stderr, "   - ⚠️ Coordinator mTLS control listener NOT enabled (server cert: %v)\n", certErr)
				} else {
					controlPort := status.DefaultControlPort
					if p := strings.TrimSpace(os.Getenv("CITADEL_CONTROL_PORT")); p != "" {
						if parsed, perr := strconv.Atoi(p); perr == nil && parsed > 0 {
							controlPort = parsed
						}
					}
					serverCfg.CAVerifier = verifier
					serverCfg.ControlServerCert = &ctrlCert
					serverCfg.ControlPort = controlPort
					controlPortForVPN = controlPort
					fmt.Printf("   - Coordinator mTLS control listener enabled on :%d for mutating endpoints (#5028)\n", controlPort)
				}
			}
		}

		statusServer := status.NewServer(serverCfg, collector)

		// Register provisioning API routes on the status server
		if provisionHandler := initProvisionHandler(); provisionHandler != nil {
			statusServer.AddRouteRegistrar(provisionHandler.RegisterRoutes)
		}

		// Register workflow API routes, gated by requireVPNOrAuth (#259).
		statusServer.AddRouteRegistrar(func(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
			wfServer := workflow.NewServer(wfExec)
			wfServer.RegisterRoutes(mux, auth)
		})

		// Add VPN listener so the status server is reachable over the tsnet VPN.
		// Bind to the explicit assigned VPN IP (not ":port"); see network.ListenVPN
		// and issue #286.
		if network.IsGlobalConnected() {
			vpnPort := fmt.Sprintf("%d", workStatusPort)
			if vpnLn, vpnIP, err := network.ListenVPN("tcp", vpnPort); err != nil {
				Log("status server VPN listener failed (LAN-only): %v", err)
				fmt.Fprintf(os.Stderr, "   - ⚠️ Status server VPN listener failed (LAN-only): %v\n", err)
			} else {
				statusServer.AddListener(vpnLn)
				Log("status server VPN listener on %s:%s", vpnIP, vpnPort)
				fmt.Printf("   - Status server VPN: http://%s:%d\n", vpnIP, workStatusPort)
			}

			// Also expose the mTLS control listener over the mesh so the
			// coordinator can reach the mutating endpoints via the VPN (#5028).
			if controlPortForVPN > 0 {
				ctrlPort := fmt.Sprintf("%d", controlPortForVPN)
				if ctrlLn, ctrlIP, err := network.ListenVPN("tcp", ctrlPort); err != nil {
					Log("control server VPN listener failed (LAN-only): %v", err)
					fmt.Fprintf(os.Stderr, "   - ⚠️ Control server VPN listener failed (LAN-only): %v\n", err)
				} else {
					statusServer.AddControlListener(ctrlLn)
					Log("control server VPN listener on %s:%s", ctrlIP, ctrlPort)
					fmt.Printf("   - Control server VPN (mTLS): https://%s:%d\n", ctrlIP, controlPortForVPN)
				}
			}
		}

		go func() {
			if serverCfg.TokenValidator != nil {
				if cachingVal, ok := serverCfg.TokenValidator.(*terminal.CachingTokenValidator); ok {
					if err := cachingVal.Start(); err != nil {
						fmt.Fprintf(os.Stderr, "   - ⚠️ Desktop API token cache error: %v\n", err)
					}
				}
				if serverCfg.EnableDesktop {
					fmt.Printf("   - Status server: http://localhost:%d (desktop API enabled)\n", workStatusPort)
				} else {
					fmt.Printf("   - Status server: http://localhost:%d (auth enabled)\n", workStatusPort)
				}
			} else {
				fmt.Printf("   - Status server: http://localhost:%d\n", workStatusPort)
			}
			if err := statusServer.Start(ctx); err != nil && err != context.Canceled {
				fmt.Fprintf(os.Stderr, "   - ⚠️ Status server error: %v\n", err)
			}
		}()
	}

	// Start SSH key sync if enabled
	if workSSHSync && apiKey != "" {
		syncInterval := time.Duration(workSSHSyncMins) * time.Minute
		sshConfig := nexus.SSHSyncConfig{
			APIToken: apiKey,
			NodeID:   nodeName,
			BaseURL:  baseURL,
		}

		go func() {
			fmt.Printf("   - SSH key sync: enabled (every %dm)\n", workSSHSyncMins)

			// Initial sync
			if err := nexus.SyncAuthorizedKeys(sshConfig); err != nil {
				fmt.Fprintf(os.Stderr, "   - ⚠️ Initial SSH sync failed: %v\n", err)
			} else {
				fmt.Println("   - ✅ SSH keys synchronized")
			}

			// Periodic sync
			ticker := time.NewTicker(syncInterval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := nexus.SyncAuthorizedKeys(sshConfig); err != nil {
						fmt.Fprintf(os.Stderr, "   - ⚠️ SSH sync failed: %v\n", err)
					}
				}
			}
		}()
	} else if workSSHSync && apiKey == "" {
		fmt.Fprintln(os.Stderr, "   - ⚠️ SSH sync enabled but no API key configured")
	}

	// Start status publisher if enabled
	if workRedisStatus {
		// Create collector if not already created
		if collector == nil {
			collector = status.NewCollector(status.CollectorConfig{
				NodeName:       nodeName,
				ConfigDir:      "",
				Services:       nil,
				Capabilities:   statusCaps,
				WorkerLiveness: workerLivenessFn,
			})
		}

		// Log VNC status for heartbeat visibility
		heartbeatVNC := platform.GetVNCManager()
		if heartbeatVNC.IsRunning() {
			fmt.Printf("   - VNC detected: port %d (included in heartbeats)\n", heartbeatVNC.Port())
		}

		// Optional, config-gated (default OFF) auto-stop-when-idle reconciler
		// (citadel #416). Built once and registered on whichever publisher runs so
		// it acts on the status the heartbeat already collected (no extra
		// stats/nvidia-smi sweep). nil when the operator has not opted in.
		autoStop := newAutoStopReconciler()

		if useAPIMode && apiSource != nil {
			// API mode: use secure API publisher
			// Get org ID from device config (saved during init)
			orgID := ""
			if deviceConfig != nil {
				orgID = deviceConfig.OrgID
			}
			// Fallback to manifest if not in device config
			if orgID == "" {
				if manifest, _, err := findAndReadManifest(); err == nil {
					orgID = manifest.Node.OrgID
				}
			}

			if orgID == "" {
				fmt.Fprintln(os.Stderr, "   - ⚠️ API status publisher requires org-id (run 'citadel init' first)")
			} else {
				// Periodically report ActualState (installed modules + health)
				// to the control plane (#353, report-only v1). Headless `citadel
				// work` is the production node entrypoint, so it must report too
				// — not just the TUI. Same device-authed client and opt-out gate
				// as activity telemetry; node_id is the Headscale hostname.
				if emitter := nodestate.New(nodestate.Config{
					Poster:    apiSource.Client(),
					Inspector: nodestate.DockerInspector(),
					ConfigDir: platform.ConfigDir(),
					NodeID:    nodeName,
					Version:   Version,
				}); emitter != nil {
					go emitter.Run(ctx)
					fmt.Printf("   - Node-state reporting: every %s\n", nodestate.DefaultInterval)
				}

				// Desired-state PULL reconcile loop (aceteam#4273), ON by default.
				// The node periodically pulls its control-plane-assigned DesiredState
				// (protobuf) and converges via the SAME reconcile engine + live
				// ModuleOps adapter MODULE_SET uses. Wired in the worker path ONLY
				// (never the control center) so a node runs exactly one converge loop.
				// Disable fleet-wide with the CITADEL_RECONCILE_PULL kill switch.
				//
				// The loop is keyed by the Headscale numeric node ID, NOT the hostname
				// (aceteam#535). The desired-state serve endpoint matches rows by a raw
				// `.eq("node_id", <path param>)` against `fabric_node_status.node_id`,
				// which the backend upserts as the Headscale numeric ID (see
				// python-backend/worker/node_status/worker.py). Fetching by hostname
				// never matches any desired row, so the loop is non-functional. The
				// report path is symmetric: the node-state worker re-resolves the
				// reported id via `get_node_info`, which accepts the numeric ID, so
				// reporting the numeric ID keys `node_module_state` correctly too.
				if loop := newReconcileLoop(apiSource.Client(), headscaleNodeID); loop != nil {
					go func() {
						runErr := loop.Run(ctx, func(_ reconcile.Plan, _ reconcile.ApplyResult, passErr error) {
							if passErr != nil {
								fmt.Fprintf(os.Stderr, "   - ⚠️ reconcile pass error: %v\n", passErr)
							}
						})
						if runErr != nil && runErr != context.Canceled {
							fmt.Fprintf(os.Stderr, "   - ⚠️ reconcile loop error: %v\n", runErr)
						}
					}()
					fmt.Printf("   - Desired-state pull: ENABLED (reconcile every %s)\n", reconcile.DefaultInterval)
				} else if !reconcile.PullDisabled() && headscaleNodeID == "" {
					// Pull is on (not killed) but the Headscale ID never resolved: skip
					// rather than fetch/report under the wrong key. Log why — a silent
					// skip here is exactly the non-functional-loop failure aceteam#535
					// describes.
					fmt.Fprintln(os.Stderr, "   - ⚠️ Desired-state pull enabled but Headscale node ID is unresolved; skipping (fetching by the wrong key would never match desired rows)")
				}

				apiPublisher, err := heartbeat.NewAPIPublisher(heartbeat.APIPublisherConfig{
					Client:          apiSource.Client(),
					NodeID:          nodeName,
					HeadscaleNodeID: headscaleNodeID,
					OrgID:           orgID,
					DebugFunc:       Debug,
				}, collector)
				if err != nil {
					fmt.Fprintf(os.Stderr, "   - ⚠️ Failed to create API publisher: %v\n", err)
				} else {
					// Include current permissions in heartbeat
					apiPublisher.SetPermissions(permissionsToHeartbeat(config.LoadPermissions(platform.ConfigDir())))
					if autoStop != nil {
						apiPublisher.SetOnStatus(func(s *status.NodeStatus) { autoStop.Reconcile(s) })
					}
					go func() {
						fmt.Printf("   - API status: %s (every 30s)\n", apiPublisher.PubSubChannel())
						if err := apiPublisher.Start(ctx); err != nil && err != context.Canceled {
							fmt.Fprintf(os.Stderr, "   - ⚠️ API status publisher error: %v\n", err)
						}
					}()
				}
			}
		} else if workRedisURL != "" {
			// Legacy mode: direct Redis publisher
			// Get device code from flag or environment
			deviceCode := workDeviceCode
			if deviceCode == "" {
				deviceCode = os.Getenv("CITADEL_DEVICE_CODE")
			}

			redisPublisher, err := heartbeat.NewRedisPublisher(heartbeat.RedisPublisherConfig{
				RedisURL:        workRedisURL,
				RedisPassword:   workRedisPass,
				NodeID:          nodeName,
				HeadscaleNodeID: headscaleNodeID,
				DeviceCode:      deviceCode,
				ChannelOverride: workStatusChannel,
				DebugFunc:       Debug,
			}, collector)
			if err != nil {
				fmt.Fprintf(os.Stderr, "   - ⚠️ Failed to create Redis publisher: %v\n", err)
			} else {
				// Include current permissions in heartbeat
				redisPublisher.SetPermissions(permissionsToHeartbeat(config.LoadPermissions(platform.ConfigDir())))
				if autoStop != nil {
					redisPublisher.SetOnStatus(func(s *status.NodeStatus) { autoStop.Reconcile(s) })
				}
				go func() {
					fmt.Printf("   - Redis status: %s (every 30s)\n", redisPublisher.PubSubChannel())
					if deviceCode != "" {
						fmt.Printf("   - Device code: %s (for config lookup)\n", deviceCode[:8]+"...")
					}
					if err := redisPublisher.Start(ctx); err != nil && err != context.Canceled {
						fmt.Fprintf(os.Stderr, "   - ⚠️ Redis status publisher error: %v\n", err)
					}
				}()
			}

			// Start config queue consumer for device configuration jobs
			configConsumer, err := heartbeat.NewConfigConsumer(heartbeat.ConfigConsumerConfig{
				RedisURL:      workRedisURL,
				RedisPassword: workRedisPass,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "   - ⚠️ Failed to create config consumer: %v\n", err)
			} else {
				go func() {
					fmt.Printf("   - Config queue: %s (listening for device config)\n", configConsumer.QueueName())
					if err := configConsumer.Start(ctx); err != nil && err != context.Canceled {
						fmt.Fprintf(os.Stderr, "   - ⚠️ Config consumer error: %v\n", err)
					}
				}()
			}

			// Start config Pub/Sub subscriber for real-time config updates
			configSubscriber, err := heartbeat.NewConfigSubscriber(heartbeat.ConfigSubscriberConfig{
				RedisURL:      workRedisURL,
				RedisPassword: workRedisPass,
				NodeID:        nodeName,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "   - ⚠️ Failed to create config subscriber: %v\n", err)
			} else {
				go func() {
					defer configSubscriber.Close() // Ensure Redis connection is cleaned up
					fmt.Printf("   - Config subscriber: %s (real-time config updates)\n", configSubscriber.Channel())
					if err := configSubscriber.Start(ctx); err != nil && err != context.Canceled {
						fmt.Fprintf(os.Stderr, "   - ⚠️ Config subscriber error: %v\n", err)
					}
				}()
			}
		}
	}

	// Start terminal server if enabled
	if workTerminal {
		// Check platform support
		if runtime.GOOS == "windows" {
			fmt.Fprintln(os.Stderr, "   - ⚠️ Terminal server is not supported on Windows")
		} else {
			// Get org ID from device config (saved during init), fall back to manifest
			orgID := ""
			if deviceConfig != nil {
				orgID = deviceConfig.OrgID
			}
			if orgID == "" && workManifest != nil {
				orgID = workManifest.Node.OrgID
			}
			if orgID == "" {
				if manifest, _, err := findAndReadManifest(); err == nil {
					orgID = manifest.Node.OrgID
				}
			}

			if orgID == "" {
				fmt.Fprintln(os.Stderr, "   - ⚠️ Terminal server requires org-id (run 'citadel init' first)")
			} else {
				termConfig := terminal.DefaultConfig()
				termConfig.OrgID = orgID
				termConfig.AuthServiceURL = baseURL
				termConfig.Version = Version

				// Best-effort: provision a Citadel-managed tmux binary so persistent
				// terminal sessions "just work" on nodes without a system tmux. This
				// is non-fatal — if it fails or the platform is gated, the terminal
				// server falls back to a bare (non-persistent) shell. Skip when tmux
				// is already resolvable or when explicitly disabled.
				ensureManagedTmux()
				if workTerminalHost != "" {
					termConfig.Host = workTerminalHost
				}
				if workTerminalPort > 0 {
					termConfig.Port = workTerminalPort
				}
				if workTerminalDebug {
					termConfig.Debug = true
				}

				// Use CachingTokenValidator for production
				termAPIToken := ""
				if deviceConfig != nil {
					termAPIToken = deviceConfig.DeviceAPIToken
				}
				cachingAuth := terminal.NewCachingTokenValidator(
					baseURL,
					orgID,
					termAPIToken,
					termConfig.TokenRefreshInterval,
				)

				termServer := terminal.NewServer(termConfig, cachingAuth)

				// Add VPN listener so the terminal server is reachable over the tsnet VPN.
				// Bind to the explicit assigned VPN IP (not ":port") so inbound
				// connections from the platform relay (which dials ws://<vpn_ip>:7860)
				// are matched by tsnet. See network.ListenVPN and issue #286.
				if network.IsGlobalConnected() {
					vpnPort := fmt.Sprintf("%d", termConfig.Port)
					if vpnLn, vpnIP, err := network.ListenVPN("tcp", vpnPort); err != nil {
						// Loud (not Debug): if the terminal server cannot bind to the
						// VPN IP it is unreachable for the mobile/relay terminal even
						// though localhost works, which is hard to diagnose otherwise.
						Log("terminal server VPN listener failed (LAN-only): %v", err)
						fmt.Fprintf(os.Stderr, "   - ⚠️ Terminal server VPN listener failed (LAN-only): %v\n", err)
					} else {
						termServer.AddListener(vpnLn)
						Log("terminal server VPN listener on %s:%s", vpnIP, vpnPort)
						fmt.Printf("   - Terminal server VPN: ws://%s:%d/terminal\n", vpnIP, termConfig.Port)
					}
				}

				go func() {
					// Start the token cache
					if err := cachingAuth.Start(); err != nil {
						fmt.Fprintf(os.Stderr, "   - ⚠️ Token cache error: %v\n", err)
					}

					fmt.Printf("   - Terminal server: ws://%s:%d/terminal\n", termConfig.Host, termConfig.Port)
					if err := termServer.Start(); err != nil {
						fmt.Fprintf(os.Stderr, "   - ⚠️ Terminal server error: %v\n", err)
					}
				}()
			}
		}
	}

	// Start in-process HTTPS gateway if enabled
	if workGateway {
		// Validate that gateway port does not collide with upstream ports
		upstreamPorts := map[int]string{
			workStatusPort:       "status-port",
			workTerminalPort:     "terminal-port",
			workGatewayVNC:       "gateway-vnc-port",
			workGatewayEmbedding: "gateway-embedding-port",
		}
		if name, collision := upstreamPorts[workGatewayPort]; collision {
			fmt.Fprintf(os.Stderr, "Error: gateway port %d collides with --%s; choose a different --gateway-port\n", workGatewayPort, name)
			os.Exit(1)
		}

		// Fetch VPN IPs for TLS certificate SANs
		var vpnIPs []net.IP
		if network.IsGlobalConnected() {
			if netStatus, err := network.GetGlobalStatus(ctx); err == nil && netStatus.Connected {
				if netStatus.IPv4 != "" {
					if ip := net.ParseIP(netStatus.IPv4); ip != nil {
						vpnIPs = append(vpnIPs, ip)
					}
				}
				if netStatus.IPv6 != "" {
					if ip := net.ParseIP(netStatus.IPv6); ip != nil {
						vpnIPs = append(vpnIPs, ip)
					}
				}
			}
		}

		// Set up TLS (unless --gateway-no-tls)
		var gwTLSConfig *tls.Config
		if !workGatewayNoTLS {
			cert, err := tlscert.EnsureCert(tlscert.Config{
				Hostname:    nodeName,
				IPAddresses: vpnIPs,
				CertDir:     workGatewayCertDir,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: TLS certificate error: %v\n", err)
				os.Exit(1)
			}
			gwTLSConfig = &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
				NextProtos:   []string{"http/1.1"},
			}
			fmt.Printf("   - Gateway TLS: self-signed (cert: %s)\n", tlscert.CertPath(workGatewayCertDir))
		}

		// Build upstream addresses
		statusAddr := fmt.Sprintf("127.0.0.1:%d", workStatusPort)
		termAddr := fmt.Sprintf("127.0.0.1:%d", workTerminalPort)
		vncAddr := fmt.Sprintf("127.0.0.1:%d", workGatewayVNC)
		embeddingAddr := fmt.Sprintf("127.0.0.1:%d", workGatewayEmbedding)

		// Start the in-process websockify bridge when desktop permissions allow.
		// The gateway proxies "/vnc/..." to vncAddr (workGatewayVNC, e.g. 6080)
		// as a raw byte pump, but x11vnc only speaks raw RFB over TCP on 5900.
		// This bridge terminates the browser's WebSocket and forwards RFB bytes
		// to the TCP VNC port, which is the role classic websockify plays.
		//
		// The bridge is started unconditionally when desktop is permitted.
		// Per-connection VNC dial uses retry with backoff, so a VNC server
		// started after "citadel work" is picked up automatically.
		gatewayPerms := config.LoadPermissions(platform.ConfigDir())
		if gatewayPerms.Desktop {
			gatewayVNC := platform.GetVNCManager()
			vncPort := gatewayVNC.Port()
			if vncPort == 0 {
				vncPort = platform.DefaultVNCPort
			}
			bridge := websockify.NewBridge(websockify.Config{
				ListenPort: workGatewayVNC,
				VNCAddress: fmt.Sprintf("127.0.0.1:%d", vncPort),
				Logger:     Debug,
			})
			fmt.Printf("   - VNC bridge: ws://127.0.0.1:%d -> 127.0.0.1:%d (websockify, lazy connect)\n", workGatewayVNC, vncPort)
			go func() {
				if err := bridge.Start(ctx); err != nil && err != context.Canceled {
					fmt.Fprintf(os.Stderr, "   - Warning: VNC bridge error: %v\n", err)
				}
			}()
		}

		// Create gateway server
		gw := gateway.NewServer(gateway.Config{
			Port:          workGatewayPort,
			ListenAddress: fmt.Sprintf("%s:%d", workGatewayBind, workGatewayPort),
			TLSConfig:     gwTLSConfig,
			NodeName:      nodeName,
		})

		// Load and apply permissions
		perms := config.LoadPermissions(platform.ConfigDir())
		gw.SetPermissions(perms)

		// Register upstreams (same routes as cmd/serve.go)
		gw.AddUpstream("/health", &gateway.Upstream{Address: statusAddr})
		gw.AddUpstream("/status", &gateway.Upstream{Address: statusAddr})
		gw.AddUpstream("/ping", &gateway.Upstream{Address: statusAddr})
		gw.AddUpstream("/services", &gateway.Upstream{Address: statusAddr})
		gw.AddUpstream("/api/screenshot", &gateway.Upstream{Address: statusAddr})
		gw.AddUpstream("/api/actions", &gateway.Upstream{Address: statusAddr})
		gw.AddUpstream("/ssh/authorized-keys", &gateway.Upstream{Address: statusAddr})
		gw.AddUpstream("/provision", &gateway.Upstream{Address: statusAddr})
		gw.AddUpstream("/workflow", &gateway.Upstream{Address: statusAddr})

		// Embedding upstream (issue #351): route the already-metered
		// /v1/embeddings path to the local TEI service (default 127.0.0.1:8102).
		gw.AddUpstream("/v1/embeddings", &gateway.Upstream{Address: embeddingAddr})

		gw.AddUpstream("/vnc", &gateway.Upstream{
			Address:     vncAddr,
			StripPrefix: true,
			WebSocket:   true,
		})

		gw.AddUpstream("/terminal", &gateway.Upstream{
			Address:     termAddr,
			StripPrefix: false,
			WebSocket:   true,
		})

		// Expose provisioned MODULES on the mesh through the gateway
		// (aceteam-ai/citadel-cli#447), driven entirely by the provisioned-service
		// registry -- no per-module code here. Each module binds an auto-selected
		// free host port that nothing on the tsnet stack listens on, so it is
		// reached only via a /modules/<prefix> gateway route (StripPrefix so its own
		// paths map through). Wire the registry so module routes are gated by the
		// capability each module DECLARED, register every recorded module up front
		// (wired to its persisted port when deployed), and publish the gateway so the
		// in-process provision handler can wire a freshly-provisioned module live.
		gwCertPath := ""
		if !workGatewayNoTLS {
			gwCertPath = tlscert.CertPath(workGatewayCertDir)
		}
		provReg := provisionedRegistry()
		gw.SetProvisionedRegistry(provReg)
		provisionedEntries := registerProvisionedModuleRoutes(gw)
		setProvisionedServiceGateway(gw, workGatewayPort, !workGatewayNoTLS, gwCertPath, workStatusPort)
		// Persist the live gateway facts so an out-of-process CLI/TUI builds a
		// reachable mesh URL and verifies against the real cert (landmines a + b).
		// StatusPort is persisted too so a separate process can build the plaintext
		// cert_refresh_url (http://<ip>:<status-port>/gateway-cert.pem).
		if err := writeGatewayFacts(gatewayFacts{Port: workGatewayPort, UseTLS: !workGatewayNoTLS, CertPath: gwCertPath, StatusPort: workStatusPort}); err != nil {
			Log("could not persist gateway facts (out-of-process URL falls back to defaults): %v", err)
		}
		// Watch the registry so a module provisioned via the CLI while this gateway
		// is already running gets wired without a restart (landmine c).
		go watchProvisionedRegistry(ctx, gw)

		// Add VPN listener (TLS-wrapped) so the gateway is reachable over tsnet.
		// Bind to the explicit assigned VPN IP (not ":port"); see network.ListenVPN
		// and issue #286.
		if network.IsGlobalConnected() {
			vpnPort := fmt.Sprintf("%d", workGatewayPort)
			rawLn, vpnIP, err := network.ListenVPN("tcp", vpnPort)
			if err != nil {
				// A lost mesh bind takes /vnc, /terminal, and /modules/* off the
				// mesh while the node keeps heartbeating healthy — the silent
				// degradation that hid the control-listener port collision on
				// every node in the fleet (#504). Escalate to an unmissable
				// startup error; keep booting because the LAN listener still
				// works.
				bindErr := gatewayVPNBindErrorMessage(workGatewayPort, err)
				Log("%s", bindErr)
				fmt.Fprintln(os.Stderr, bindErr)
			} else {
				var vpnGwLn net.Listener
				if gwTLSConfig != nil {
					vpnGwLn = tls.NewListener(rawLn, gwTLSConfig)
				} else {
					vpnGwLn = rawLn
				}
				gw.AddListener(vpnGwLn)
				Log("gateway VPN listener on %s:%s", vpnIP, vpnPort)
			}
		}

		// Print route table
		scheme := "https"
		if workGatewayNoTLS {
			scheme = "http"
		}
		listenAddr := fmt.Sprintf("%s:%d", workGatewayBind, workGatewayPort)
		fmt.Printf("   - Gateway: %s://%s\n", scheme, listenAddr)
		fmt.Println("   - Routes:")
		fmt.Printf("     /health, /status, /ping  -> %s (status server)\n", statusAddr)
		fmt.Printf("     /api/screenshot, /api/actions -> %s\n", statusAddr)
		fmt.Printf("     /ssh/authorized-keys     -> %s (SSH key deploy)\n", statusAddr)
		fmt.Printf("     /workflow/...             -> %s (workflow API)\n", statusAddr)
		fmt.Printf("     /v1/embeddings           -> %s (TEI embeddings)\n", embeddingAddr)
		fmt.Printf("     /vnc/...                 -> %s (websockify)\n", vncAddr)
		fmt.Printf("     /terminal/...            -> %s (terminal)\n", termAddr)
		for _, e := range provisionedEntries {
			target := "dynamic port (not yet deployed)"
			if e.Port > 0 {
				target = fmt.Sprintf("127.0.0.1:%d", e.Port)
			}
			fmt.Printf("     /modules/%s/...  -> %s (provisioned module %q, cap %q)\n", e.Prefix, target, e.Name, e.Capability)
		}

		if len(vpnIPs) > 0 {
			fmt.Printf("   - Gateway VPN: %s://%s:%d\n", scheme, vpnIPs[0], workGatewayPort)
		}

		go func() {
			if err := gw.Start(ctx); err != nil && err != context.Canceled {
				fmt.Fprintf(os.Stderr, "   - Warning: Gateway error: %v\n", err)
			}
		}()
	}

	// Start usage syncer if store is available
	if usageStore != nil {
		publishFn := createUsagePublishFn(useAPIMode, apiSource, workRedisURL, workRedisPass)
		if publishFn != nil {
			syncer := usage.NewSyncer(usage.SyncerConfig{
				Store:     usageStore,
				PublishFn: publishFn,
			})
			go func() {
				Debug("usage syncer started (60s interval)")
				if err := syncer.Start(ctx); err != nil && err != context.Canceled {
					fmt.Fprintf(os.Stderr, "   - Warning: Usage syncer error: %v\n", err)
				}
			}()
		} else {
			Debug("usage syncer: no publish target available (local-only tracking)")
		}
	}

	// Create handlers with optional workspace for file-operation jobs. The node-job
	// handler set (legacy + workflow, plus the privileged AGENT_UPDATE /
	// WHATSAPP_PROVISION registered on the runner below) is built via the shared
	// helper so the control-center TUI registers the exact same set when it is the
	// only worker on the node.
	workPerms := config.LoadPermissions(platform.ConfigDir())
	nodeJobOpts := nodeJobHandlerOpts{
		WorkspaceDir:              wsDir,
		ConfigDir:                 workConfigDir,
		AllowReadOutsideWorkspace: resolveAllowReadOutsideWorkspace(),
		ShellDisabled:             !workPerms.Shell,
		WorkflowExec:              wfExec,
		HandlerLog:                func(format string, args ...any) { Log(format, args...) },
	}
	handlers := buildNodeJobHandlers(nodeJobOpts)

	// Build job record function for usage tracking
	var jobRecordFn func(record usage.UsageRecord)
	if usageStore != nil {
		jobRecordFn = func(record usage.UsageRecord) {
			record.NodeID = nodeName
			if err := usageStore.Insert(record); err != nil {
				Debug("usage insert error: %v", err)
			}
		}
	}

	// Resolve concurrency from flag or auto-detect from GPU count
	maxConcurrency := workMaxConcurrency
	var gpuTracker *worker.GPUTracker
	gpuCount := platform.GetGPUCountSimple()
	if gpuCount > 0 {
		gpuTracker = worker.NewGPUTracker(gpuCount)
		if maxConcurrency == 0 {
			maxConcurrency = gpuCount
		}
	}
	if maxConcurrency == 0 {
		maxConcurrency = 1 // Default: sequential
	}

	// Create runner.
	//
	// ActivityFn routes the runner's per-job and lifecycle log lines to the
	// citadel log file instead of stdout (which the log file does not capture).
	// Before this, those lines were lost during a live node-routing debug
	// (issue #3924/#236); #234 already routed the source's LogFn the same way.
	// State threads the shared introspection metrics so the /agent/* endpoints
	// can report consume/job activity.
	runner := worker.NewRunner(source, handlers, worker.RunnerConfig{
		WorkerID:       workerID,
		NodeID:         headscaleNodeID,
		AgentVersion:   Version,
		Verbose:        true,
		ActivityFn:     func(_ string, msg string) { Log("%s", msg) },
		JobRecordFn:    jobRecordFn,
		MaxConcurrency: maxConcurrency,
		GPUTracker:     gpuTracker,
		State:          workerState,
	})

	// Add stream writer factory if available
	if streamFactory != nil {
		runner.WithStreamWriterFactory(streamFactory)
	}

	// Register the node-targeted privileged job handlers (AGENT_UPDATE aceteam#4427,
	// WHATSAPP_PROVISION aceteam#4454) on the live runner. They are registered after
	// the runner exists so AGENT_UPDATE can borrow the runner's Drain/ActiveJobs for
	// the "publish result, THEN restart" ordering (mirroring the AutoUpdater below).
	// Shared with the control-center TUI via registerPrivilegedNodeJobHandlers so a
	// control-center-only node handles the same privileged set.
	registerPrivilegedNodeJobHandlers(runner, nodeJobOpts)

	// Start the periodic auto-updater. The goroutine always runs; whether it
	// actually checks/installs on a given tick is decided per-tick by
	// resolveAutoUpdateEnabled() so the switch can be flipped on a *running*
	// agent — `citadel update enable/disable` (or the web UI, which dispatches
	// those same commands) writes the persisted state and the next tick honors
	// it without a restart. When enabled and a newer version is found, it drains
	// in-flight jobs before atomically swapping the binary and restarting.
	if interval, err := update.ParseInterval(resolveAutoUpdateInterval()); err != nil {
		fmt.Fprintf(os.Stderr, "   - Warning: %v; auto-update disabled\n", err)
	} else {
		updater := update.NewAutoUpdater(update.AutoUpdaterConfig{
			Checker:    update.NewClientWithTimeout(Version, 30*time.Second),
			Interval:   interval,
			Enabled:    resolveAutoUpdateEnabled,
			ActiveJobs: runner.ActiveJobs,
			Drain:      func() { runner.Drain() },
			Log: func(format string, args ...any) {
				fmt.Printf("   - "+format+"\n", args...)
			},
		})
		go updater.Run(ctx)
	}

	// Start the self-heal liveness monitor (issue #548). It is the backstop for a
	// consumption-wedged worker that the per-job watchdog can't catch (a wedge
	// outside a handler, or a build with the watchdog disabled): it watches the
	// shared workerState and, on a clear loss of forward progress, exits non-zero
	// so systemd restarts the process clean. No-op if disabled (WORKER_SELF_HEAL).
	if monitor := worker.NewLivenessMonitor(workerState, runner.IsDraining, func(level, msg string) { Log("%s", msg) }); monitor != nil {
		go monitor.Run(ctx)
	}

	// Run the worker
	if err := runner.Run(ctx); err != nil {
		if err != context.Canceled {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}
}

// startedService records a managed service that this worker actually started
// (as opposed to one that was already running when the worker booted). It is
// used to tear down only what we launched on graceful shutdown, mirroring how
// the co-browse manager stops only the Chromium process it spawned.
type startedService struct {
	name string
	// composePath is set for docker/compose services; empty for native services.
	composePath string
	native      bool
}

// startManagedServices starts all services defined in the manifest and returns
// the subset it actually started (so callers can tear those down on shutdown).
//
// It honors ctx cancellation: if the worker is shutting down mid-startup (e.g.
// vLLM is still pulling an image), it stops launching the remaining services
// instead of orphaning a long-running goroutine. Per-service failures are
// logged as warnings and never abort the sequence — a heavy service that fails
// to come up must not block the rest, and the status collector reports actual
// container state regardless of start ordering.
//
// This function may block for a long time (image pulls, model loads). Callers
// that must not gate job-stream subscription on service readiness should invoke
// it from a background goroutine (see runWork). The old blocking behavior is
// still available via --wait-services.
func startManagedServices(ctx context.Context) []startedService {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		// No manifest found - this is fine, just skip service startup
		return nil
	}

	if len(manifest.Services) == 0 {
		return nil
	}

	fmt.Printf("--- Starting %d service(s) ---\n", len(manifest.Services))

	var started []startedService
	for _, service := range manifest.Services {
		// Stop launching further services once shutdown has been requested so a
		// slow startup goroutine unwinds promptly instead of racing teardown.
		if ctx.Err() != nil {
			fmt.Println("   - Service startup interrupted by shutdown")
			break
		}

		// Honor a durable "stopped" marker: a service a remote MODULE_SET set to
		// stopped stays installed but must NOT be composed up on boot, or the stop
		// would silently come back on the next `citadel work` restart (aceteam#5280).
		if serviceStartDisabled(service) {
			fmt.Printf("   - Skipping stopped service: %s (desired_status: stopped)\n", service.Name)
			continue
		}

		serviceType := determineServiceType(service)

		if serviceType == internalServices.ServiceTypeNative {
			fmt.Printf("   - Starting %s (native)...\n", service.Name)
			if err := startNativeService(service.Name, configDir); err != nil {
				fmt.Fprintf(os.Stderr, "     Warning: %s: %v\n", service.Name, err)
				continue
			}
			started = append(started, startedService{name: service.Name, native: true})
		} else {
			// Validate that compose file path stays within config directory (prevent path traversal)
			fullComposePath, err := platform.ValidatePathWithinDir(configDir, service.ComposeFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "     Warning: %s: invalid compose file path: %v\n", service.Name, err)
				continue
			}
			fmt.Printf("   - Starting %s...\n", service.Name)
			if err := startService(service.Name, fullComposePath); err != nil {
				fmt.Fprintf(os.Stderr, "     Warning: %s: %v\n", service.Name, err)
				continue
			}
			started = append(started, startedService{name: service.Name, composePath: fullComposePath})
		}
		fmt.Printf("   - %s is running\n", service.Name)
	}

	fmt.Println("--- Services started ---")
	return started
}

// startFootprintSampler launches the background footprint sampler goroutine
// (issue #422) unless it is disabled via --no-footprint or a non-positive
// CITADEL_FOOTPRINT_INTERVAL. The service list is taken from the manifest (the
// status collector is constructed with a nil service list, so it is not a usable
// source here), and the container engine is the selected runtime's engine binary
// so podman nodes sample podman, not a hardcoded docker.
func startFootprintSampler(ctx context.Context, nodeName string, manifest *CitadelManifest) {
	if workNoFootprint {
		Debug("footprint sampler disabled (--no-footprint)")
		return
	}
	interval, enabled := footprint.IntervalFromEnv()
	if !enabled {
		Debug("footprint sampler disabled (CITADEL_FOOTPRINT_INTERVAL<=0)")
		return
	}

	nodeDir, err := platform.DefaultNodeDir("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "   - Warning: footprint sampler disabled (no node dir): %v\n", err)
		return
	}

	var serviceNames []string
	if manifest != nil {
		for _, svc := range manifest.Services {
			serviceNames = append(serviceNames, svc.Name)
		}
	}

	cfg := footprint.Config{
		NodeID:        nodeName,
		Services:      serviceNames,
		EngineBin:     catalog.SelectContainerRuntime().EngineBin,
		Dir:           footprint.DefaultDir(nodeDir),
		Interval:      interval,
		RetentionDays: footprint.RetentionFromEnv(),
		Logf:          func(format string, args ...any) { Log(format, args...) },
	}
	go footprint.Run(ctx, cfg)
	fmt.Printf("   - Footprint sampler: every %s → %s\n", interval, cfg.Dir)
}

// stopManagedServices tears down services this worker started, mirroring the
// co-browse manager teardown: it stops the containers/processes we launched but
// preserves their data (docker compose down WITHOUT -v keeps model-cache
// volumes; native stops leave any on-disk state). Services that were already
// running when the worker booted are not in the list and are left untouched.
func stopManagedServices(started []startedService) {
	if len(started) == 0 {
		return
	}
	fmt.Printf("   - Stopping %d managed service(s)...\n", len(started))
	// Reverse order for graceful shutdown, matching `citadel stop`.
	for i := len(started) - 1; i >= 0; i-- {
		svc := started[i]
		if svc.native {
			if err := internalServices.StopNativeService(svc.name); err != nil {
				fmt.Fprintf(os.Stderr, "   - Warning: stop %s: %v\n", svc.name, err)
			}
			continue
		}
		if err := stopServiceByCompose(svc.composePath, false); err != nil {
			fmt.Fprintf(os.Stderr, "   - Warning: stop %s: %v\n", svc.name, err)
		}
	}
}

// shellQueueName returns the shared per-org shell command queue name.
// Untargeted jobs dispatched by platform MCP tools (terminal_exec, code_read,
// etc.) are enqueued to this queue using the pattern jobs:v1:shell:org_{org_id}.
// Every online worker in the org consumes it, so any of them may claim a job.
func shellQueueName(orgID string) string {
	return fmt.Sprintf("jobs:v1:shell:org_%s", orgID)
}

// meetingQueueName returns the org-scoped meeting-notetaker tag queue name.
// The backend dispatches MEETING_JOIN jobs to this per-org tag queue; the bare
// jobs:v1:tag:meeting queue is rejected server-side to prevent cross-org meeting
// dispatch (aceteam-ai/aceteam#5098). The string MUST stay byte-for-byte
// identical to the Python dispatch helper.
func meetingQueueName(orgID string) string {
	return fmt.Sprintf("jobs:v1:tag:meeting:org_%s", orgID)
}

// hasCapabilityTag reports whether the resolved capability tags contain the
// given tag. Used to gate optional queue subscriptions on the node actually
// advertising the matching capability.
func hasCapabilityTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// nodeQueueName returns the per-node shell stream name for node-targeted jobs.
//
// Node-targeted MCP jobs (terminal_exec, code_*, file reads, node attachments)
// must run on a specific node, identified by its Headscale numeric node ID.
// The platform dispatcher routes such jobs to this stream so only the intended
// worker can claim them, instead of any greedy peer on the shared shell stream
// (issue #3914). The name is namespaced under the org's shell stream and MUST
// stay byte-for-byte identical to the Python build_node_queue helper:
//
//	jobs:v1:shell:org_{org_id}:node:{node_id}
func nodeQueueName(orgID, nodeID string) string {
	return fmt.Sprintf("jobs:v1:shell:org_%s:node:%s", orgID, nodeID)
}

// getWorkHostname returns the hostname to use for VPN reconnection.
// Prefers the --node-name flag, then CITADEL_NODE_NAME env, then OS hostname.
func getWorkHostname() string {
	if workNodeName != "" {
		return workNodeName
	}
	if envName := os.Getenv("CITADEL_NODE_NAME"); envName != "" {
		return envName
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		return "citadel-node"
	}
	return hostname
}

// getRedisURLFromConfig reads Redis URL from global config file.
// Returns empty string if not configured.
func getRedisURLFromConfig() string {
	globalConfigFile := filepath.Join(platform.ConfigDir(), "config.yaml")
	data, err := os.ReadFile(globalConfigFile)
	if err != nil {
		return ""
	}

	var config struct {
		RedisURL string `yaml:"redis_url"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return ""
	}
	return config.RedisURL
}

// resolveWorkspaceDir returns the sandbox directory for file-operation handlers.
// Priority: --workspace flag > CITADEL_WORKSPACE env > ~/citadel-node/workspace default.
// Returns empty string if the directory cannot be resolved or created.
func resolveWorkspaceDir() string {
	dir := workWorkspaceDir
	if dir == "" {
		dir = os.Getenv("CITADEL_WORKSPACE")
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, "citadel-node", "workspace")
	}
	// Ensure the directory exists.
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "   - Warning: cannot create workspace dir %s: %v\n", dir, err)
		return ""
	}
	// Resolve to absolute path (handles relative flag values).
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// newAutoStopReconciler builds the config-gated auto-stop-when-idle reconciler
// (citadel #416), or returns nil when the operator has not opted in
// (SERVICE_AUTO_STOP_WHEN_IDLE unset/false) so the feature adds zero cost by
// default. When enabled, it wires a stop dispatcher that routes each idle
// candidate to the correct lifecycle: manifest/embedded services via the same
// compose "down" a SERVICE_STOP job takes, and catalog apps via the apps
// package's docker-stop. Covering both is deliberate: the idle GPU hog is often
// a catalog app (the #421 diffusers case), which a service-only path would
// silently fail to evict.
func newAutoStopReconciler() *status.AutoStopReconciler {
	if !status.AutoStopEnabled() {
		return nil
	}
	configDir := platform.ConfigDir()
	wsDir := resolveWorkspaceDir()
	svcHandler := jobs.NewServiceHandlerWithWorkspace(configDir, wsDir)

	stop := func(c status.IdleCandidate) error {
		switch c.Kind {
		case status.EntityApp:
			return apps.Stop(context.Background(), apps.ExecRunner{}, c.Name)
		default:
			return svcHandler.StopServiceByName(c.Name)
		}
	}
	logFn := func(level, format string, args ...any) {
		if level == "warning" {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		} else {
			fmt.Printf(format+"\n", args...)
		}
	}
	r := status.NewAutoStopReconciler(true, status.AutoStopThresholdSeconds(), stop, logFn)
	fmt.Printf("   - Auto-stop-when-idle: ENABLED (threshold %ds)\n", status.AutoStopThresholdSeconds())
	return r
}

// resolveAllowReadOutsideWorkspace returns whether read-only file handlers may
// access paths outside the workspace sandbox.
// Priority: --allow-read-outside-workspace flag > CITADEL_ALLOW_READ_OUTSIDE_WORKSPACE env.
func resolveAllowReadOutsideWorkspace() bool {
	if workAllowReadOutsideWorkspace {
		return true
	}
	v := os.Getenv("CITADEL_ALLOW_READ_OUTSIDE_WORKSPACE")
	return v == "true" || v == "1"
}

// resolveConsumerGroup returns the consumer group name to use.
// Priority: explicit flag > Headscale node ID > hostname > fallback "citadel-workers".
func resolveConsumerGroup(explicit, headscaleNodeID, hostname string) string {
	if explicit != "" {
		return explicit
	}
	if headscaleNodeID != "" {
		return fmt.Sprintf("citadel-node-%s", headscaleNodeID)
	}
	if hostname != "" {
		return fmt.Sprintf("citadel-%s", hostname)
	}
	return "citadel-workers"
}

// resolveAutoUpdateEnabled reports whether the periodic auto-updater should act
// on the current tick. Priority: --auto-update flag > CITADEL_AUTO_UPDATE env
// (explicit on/off) > the persisted `citadel update enable/disable` state.
// Disabled by default. Evaluated every tick so the web UI (which dispatches
// `citadel update enable/disable` to the node) can toggle a running agent.
func resolveAutoUpdateEnabled() bool {
	// The opt-out (--no-auto-update / CITADEL_NO_AUTO_UPDATE) and a dev build
	// both veto auto-INSTALL, ahead of any enable signal: a safety/"don't touch
	// my binary" signal must win over --auto-update / CITADEL_AUTO_UPDATE.
	// Explicit `citadel update` remains the escape hatch for a dev binary.
	if !autoUpdateAllowed() {
		return false
	}
	if workAutoUpdate {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CITADEL_AUTO_UPDATE"))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	state, err := update.LoadState()
	if err != nil || state == nil {
		return false
	}
	return state.AutoUpdate
}

// resolveAutoUpdateInterval returns the configured auto-update interval string.
// Priority: --auto-update-interval flag > CITADEL_AUTO_UPDATE_INTERVAL env.
// Empty means use the default (1h).
func resolveAutoUpdateInterval() string {
	if workAutoUpdateInterval != "" {
		return workAutoUpdateInterval
	}
	return os.Getenv("CITADEL_AUTO_UPDATE_INTERVAL")
}

// DeviceConfig holds device authentication configuration from the global config file.
type DeviceConfig struct {
	DeviceAPIToken string `yaml:"device_api_token"`
	APIBaseURL     string `yaml:"api_base_url"`
	OrgID          string `yaml:"org_id"`
	OrgName        string `yaml:"org_name"`
	RedisURL       string `yaml:"redis_url"`
	UserEmail      string `yaml:"user_email"`
	UserName       string `yaml:"user_name"`
	// AceteamAPIKey is an optional user act_ API key for surfaces the
	// device token cannot reach (Team Chat, MCP). Set manually by the user;
	// device auth never writes it. See aceteam-ai/citadel-cli#495.
	AceteamAPIKey string `yaml:"aceteam_api_key"`
}

// getDeviceConfigFromFile reads device authentication config from global config file.
func getDeviceConfigFromFile() *DeviceConfig {
	globalConfigFile := filepath.Join(platform.ConfigDir(), "config.yaml")
	data, err := os.ReadFile(globalConfigFile)
	if err != nil {
		return nil
	}

	var config DeviceConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil
	}

	// Return nil if no relevant config found
	if config.DeviceAPIToken == "" && config.RedisURL == "" {
		return nil
	}

	return &config
}

// createUsagePublishFn returns a function that publishes usage records to Redis.
// Returns nil if no publish target is available (records will be stored locally only).
func createUsagePublishFn(useAPI bool, apiSource *worker.APISource, redisURL, redisPass string) usage.PublishFunc {
	const streamName = "node:usage:stream"
	const maxLen int64 = 50000

	if useAPI && apiSource != nil {
		// API mode: use the secure API client
		return func(ctx context.Context, records []usage.UsageRecord) error {
			client := apiSource.Client()
			for _, r := range records {
				payload, err := json.Marshal(usageStreamEntry{
					Version: "1.0",
					NodeID:  r.NodeID,
					Record:  usageRecordPayload(r),
				})
				if err != nil {
					return fmt.Errorf("marshal usage record: %w", err)
				}
				if err := client.StreamAdd(ctx, streamName, map[string]string{
					"data": string(payload),
				}, maxLen); err != nil {
					return fmt.Errorf("stream add: %w", err)
				}
			}
			return nil
		}
	}

	if redisURL != "" {
		// Direct Redis mode: create client once, reuse across sync cycles
		opts, err := goredis.ParseURL(redisURL)
		if err != nil {
			return nil
		}
		if redisPass != "" {
			opts.Password = redisPass
		}
		rdb := goredis.NewClient(opts)

		return func(ctx context.Context, records []usage.UsageRecord) error {
			for _, r := range records {
				payload, err := json.Marshal(usageStreamEntry{
					Version: "1.0",
					NodeID:  r.NodeID,
					Record:  usageRecordPayload(r),
				})
				if err != nil {
					return fmt.Errorf("marshal usage record: %w", err)
				}
				if err := rdb.XAdd(ctx, &goredis.XAddArgs{
					Stream: streamName,
					MaxLen: maxLen,
					Approx: true,
					Values: map[string]string{
						"data": string(payload),
					},
				}).Err(); err != nil {
					return fmt.Errorf("xadd: %w", err)
				}
			}
			return nil
		}
	}

	return nil
}

type usageStreamEntry struct {
	Version string          `json:"version"`
	NodeID  string          `json:"nodeId"`
	Record  usageRecordJSON `json:"record"`
}

// usageRecordJSON is the JSON representation published to Redis.
// ErrorMessage is intentionally excluded to avoid leaking internal error details.
type usageRecordJSON struct {
	JobID            string `json:"jobId"`
	JobType          string `json:"jobType"`
	Backend          string `json:"backend,omitempty"`
	Model            string `json:"model,omitempty"`
	Status           string `json:"status"`
	StartedAt        string `json:"startedAt"`
	CompletedAt      string `json:"completedAt"`
	DurationMs       int64  `json:"durationMs"`
	PromptTokens     int64  `json:"promptTokens,omitempty"`
	CompletionTokens int64  `json:"completionTokens,omitempty"`
	TotalTokens      int64  `json:"totalTokens,omitempty"`
	RequestBytes     int64  `json:"requestBytes,omitempty"`
	ResponseBytes    int64  `json:"responseBytes,omitempty"`
}

func usageRecordPayload(r usage.UsageRecord) usageRecordJSON {
	return usageRecordJSON{
		JobID:            r.JobID,
		JobType:          r.JobType,
		Backend:          r.Backend,
		Model:            r.Model,
		Status:           r.Status,
		StartedAt:        r.StartedAt.UTC().Format(time.RFC3339),
		CompletedAt:      r.CompletedAt.UTC().Format(time.RFC3339),
		DurationMs:       r.DurationMs,
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		TotalTokens:      r.TotalTokens,
		RequestBytes:     r.RequestBytes,
		ResponseBytes:    r.ResponseBytes,
	}
}

// permissionsToHeartbeat converts config.Permissions to the heartbeat wire format.
func permissionsToHeartbeat(p *config.Permissions) *heartbeat.PermissionState {
	if p == nil {
		return nil
	}
	return &heartbeat.PermissionState{
		Console:  p.Console,
		Desktop:  p.Desktop,
		Files:    p.Files,
		Services: p.Services,
		SSH:      p.SSH,
		Shell:    p.Shell,
	}
}

// ensureManagedTmux best-effort provisions a Citadel-managed tmux binary so the
// terminal server can back sessions with persistent tmux on nodes that lack a
// system tmux. It is intentionally non-fatal and quiet:
//   - if tmux is already resolvable (CITADEL_TMUX_BIN / PATH / managed), do
//     nothing;
//   - if the platform is gated/unsupported or the download/verify fails, log at
//     debug level and return — the terminal server already falls back to a bare,
//     non-persistent shell.
//
// Set CITADEL_TMUX_NO_INSTALL=1 (or true) to skip provisioning entirely.
func ensureManagedTmux() {
	if v := os.Getenv("CITADEL_TMUX_NO_INSTALL"); v == "1" || strings.EqualFold(v, "true") {
		Debug("managed tmux install disabled via CITADEL_TMUX_NO_INSTALL")
		return
	}
	if tmux.IsAvailable() {
		return
	}
	if !tmuxinstall.Available() {
		Debug("no managed tmux artifact for this platform; terminals use a bare shell")
		return
	}
	inst := tmuxinstall.New()
	if _, err := inst.Ensure(); err != nil {
		Debug("managed tmux install failed (falling back to bare shell): %v", err)
		return
	}
	Log("Installed Citadel-managed tmux at %s", inst.DestPath())
}

func init() {
	rootCmd.AddCommand(workCmd)

	// Queue flags
	workCmd.Flags().StringVar(&workQueue, "queue", "", "Queue/stream name to consume from (default: jobs:v1:cpu-general)")
	workCmd.Flags().StringVar(&workGroup, "group", "", "Consumer group name (default: citadel-node-<id> or citadel-<hostname>)")
	workCmd.Flags().IntVar(&workPollMs, "poll-ms", 5000, "Block timeout in milliseconds")
	workCmd.Flags().IntVar(&workMaxRetries, "max-retries", 3, "Maximum retry attempts before DLQ")

	// Debug flags (hidden) - direct Redis for development/debugging only
	workCmd.Flags().StringVar(&workRedisURL, "debug-redis-url", "", "Direct Redis URL for debugging (bypasses API mode)")
	workCmd.Flags().StringVar(&workRedisPass, "debug-redis-password", "", "Redis password for debugging")
	workCmd.Flags().MarkHidden("debug-redis-url")
	workCmd.Flags().MarkHidden("debug-redis-password")

	// Status flags
	workCmd.Flags().IntVar(&workStatusPort, "status-port", 0, "Enable status HTTP server on port (0 = disabled)")
	workCmd.Flags().StringVar(&workBaseURL, "base-url", "", "AceTeam API base URL (default: https://aceteam.ai, or set ACETEAM_URL env)")
	workCmd.Flags().StringVar(&workAPIKey, "api-key", "", "API key for authentication (or set CITADEL_API_KEY env)")
	workCmd.Flags().StringVar(&workNodeName, "node-name", "", "Node name for status reporting (default: hostname)")

	// SSH key sync flags
	workCmd.Flags().BoolVar(&workSSHSync, "ssh-sync", false, "Enable SSH authorized_keys synchronization from AceTeam")
	workCmd.Flags().IntVar(&workSSHSyncMins, "ssh-sync-interval", 5, "SSH sync interval in minutes")

	// Redis status publishing flags
	workCmd.Flags().BoolVar(&workRedisStatus, "redis-status", true, "Enable Redis status publishing for real-time updates")
	workCmd.Flags().StringVar(&workDeviceCode, "device-code", "", "Device authorization code for config lookup (or set CITADEL_DEVICE_CODE env)")
	workCmd.Flags().StringVar(&workStatusChannel, "status-channel", "", "Override Redis pub/sub channel for status (default: node:status:{node-name})")

	// Terminal server flags
	workCmd.Flags().BoolVar(&workTerminal, "terminal", true, "Enable terminal WebSocket server (enabled by default; use --no-terminal to disable)")
	workCmd.Flags().BoolVar(&workNoTerminal, "no-terminal", false, "Disable the terminal WebSocket server")
	workCmd.Flags().StringVar(&workTerminalHost, "terminal-host", "", "Terminal server bind address (default: 0.0.0.0)")
	workCmd.Flags().IntVar(&workTerminalPort, "terminal-port", 7860, "Terminal server port (default: 7860)")
	workCmd.Flags().BoolVar(&workTerminalDebug, "terminal-debug", false, "Enable verbose debug logging for terminal server")

	// Service auto-start flags
	workCmd.Flags().BoolVar(&workNoServices, "no-services", false, "Skip auto-starting services from manifest")
	workCmd.Flags().BoolVar(&workWaitServices, "wait-services", false, "Block until all manifest services are up before subscribing to job streams (default: start services asynchronously)")

	// Update check flags (deprecated - update check now runs on all commands via root.go)
	workCmd.Flags().BoolVar(&workNoUpdate, "no-update", false, "(Deprecated) No longer has any effect - use 'citadel update disable' instead")
	workCmd.Flags().BoolVar(&workNoFootprint, "no-footprint", false, "Disable the background resource-footprint sampler")
	workCmd.Flags().BoolVar(&workNoSingleInstance, "no-single-instance", false, "Allow a second worker to run for this node (skips the single-instance lock)")
	workCmd.Flags().BoolVar(&workAttach, "attach", false, "When a worker is already running, print its status banner instead of refusing (default: only on an interactive terminal)")
	workCmd.Flags().BoolVar(&workNoAttach, "no-attach", false, "When a worker is already running, refuse with exit 1 instead of printing the status banner")
	workCmd.Flags().BoolVar(&workNoKeyRenew, "no-key-renew", false, "Disable the background Headscale node-key renewer (renews the key before expiry while online)")
	workCmd.Flags().MarkDeprecated("no-update", "use 'citadel update disable' to disable auto-update checks")

	// Auto-update (periodic self-update) flags
	workCmd.Flags().BoolVar(&workAutoUpdate, "auto-update", false, "Periodically check for and install newer releases (or set CITADEL_AUTO_UPDATE=true)")
	workCmd.Flags().StringVar(&workAutoUpdateInterval, "auto-update-interval", "", "Interval between auto-update checks, e.g. 1h, 30m (default 1h; or set CITADEL_AUTO_UPDATE_INTERVAL)")

	// Capability detection flags
	workCmd.Flags().StringVar(&workCapabilities, "capabilities", "", "Additional comma-separated capability tags (e.g., gpu:rtx4090,llm:llama3)")
	workCmd.Flags().BoolVar(&workAutoDetect, "auto-detect", false, "(Deprecated) Capabilities are now always auto-detected unless declared in citadel.yaml")

	// Concurrency flags
	workCmd.Flags().IntVar(&workMaxConcurrency, "max-concurrency", 0, "Maximum concurrent jobs (0 = auto-detect from GPU count)")

	// Workspace flags
	workCmd.Flags().StringVar(&workWorkspaceDir, "workspace", "", "Workspace directory for file-operation jobs (or set CITADEL_WORKSPACE env)")
	workCmd.Flags().BoolVar(&workAllowReadOutsideWorkspace, "allow-read-outside-workspace", false, "Allow read-only file ops to access paths outside the workspace sandbox (or set CITADEL_ALLOW_READ_OUTSIDE_WORKSPACE=true)")

	// Gateway flags
	workCmd.Flags().BoolVar(&workGateway, "gateway", true, "Start the HTTPS gateway in-process (enabled by default; use --no-gateway to disable)")
	workCmd.Flags().BoolVar(&workNoGateway, "no-gateway", false, "Disable the HTTPS gateway")
	workCmd.Flags().IntVar(&workGatewayPort, "gateway-port", 8443, "HTTPS gateway port (default: 8443)")
	workCmd.Flags().StringVar(&workGatewayBind, "gateway-bind", "0.0.0.0", "Gateway bind address")
	workCmd.Flags().IntVar(&workGatewayVNC, "gateway-vnc-port", 6080, "VNC websockify port for gateway proxy")
	workCmd.Flags().IntVar(&workGatewayEmbedding, "gateway-embedding-port", 8102, "Local TEI embedding service port (/v1/embeddings upstream)")
	workCmd.Flags().BoolVar(&workGatewayNoTLS, "gateway-no-tls", false, "Disable TLS on the gateway (for testing only)")
	workCmd.Flags().StringVar(&workGatewayCertDir, "gateway-cert-dir", "", "Custom directory for gateway TLS certificates")
	workCmd.Flags().MarkHidden("gateway-no-tls")
}

// initProvisionHandler creates a provision.Handler backed by the Docker
// backend with state persisted in the Citadel config directory.
// Returns nil if initialization fails (provisioning API will be unavailable).
func initProvisionHandler() *provision.Handler {
	configDir := platform.ConfigDir()
	store, err := provision.NewStore(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "   - provision store init failed: %v\n", err)
		return nil
	}
	docker, err := provision.NewDockerBackend()
	if err != nil {
		fmt.Fprintf(os.Stderr, "   - provision docker backend init failed: %v\n", err)
		return nil
	}
	backends := map[provision.ResourceType]provision.Backend{
		provision.ResourceTypeDocker: docker,
	}
	mgr := provision.NewManager(store, backends)
	// Reconcile persisted resources against actual Docker state on startup.
	// This detects containers that crashed or were removed while the daemon
	// was down and updates their status accordingly.
	mgr.ReconcileAll(context.Background())
	return provision.NewHandler(mgr)
}

// splitAndTrim splits a comma-separated env value into non-empty, trimmed items.
// Used for CITADEL_COORDINATOR_SANS (the allowlist of coordinator SAN URIs that
// may invoke mutating control endpoints -- issue #5028).
func splitAndTrim(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
