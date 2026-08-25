// cmd/status.go
package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/capabilities"
	"github.com/aceteam-ai/citadel-cli/internal/heartbeat"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/resmon"
	statuspkg "github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/tui"
	"github.com/aceteam-ai/citadel-cli/internal/tui/dashboard"
	"github.com/aceteam-ai/citadel-cli/internal/worklock"
	svcports "github.com/aceteam-ai/citadel-cli/services"
	"github.com/fatih/color"
	"github.com/redis/go-redis/v9"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/spf13/cobra"
)

var (
	headerColor   = color.New(color.FgCyan, color.Bold)
	goodColor     = color.New(color.FgGreen)
	warnColor     = color.New(color.FgYellow)
	badColor      = color.New(color.FgRed)
	faintColor    = color.New(color.Faint)
	labelColor    = color.New(color.Bold)
	interactiveUI bool // Flag to enable interactive TUI dashboard
	statusJSON    bool // Flag to output JSON format
)

var statusCmd = &cobra.Command{
	Use:     "status",
	Aliases: []string{"st", "info"},
	Short:   "Shows a comprehensive status of the Citadel node",
	Long: `Provides a full health check and resource overview of the Citadel node.
It checks network connectivity, system vitals (CPU, RAM, Disk), GPU status,
and the state of all managed services.`,
	Example: `  # View full node status with colors
  citadel status

  # View status without colors (for scripts/logging)
  citadel status --no-color

  # Interactive dashboard with live updates
  citadel status -i`,
	Run: func(cmd *cobra.Command, args []string) {
		// JSON output mode
		if statusJSON {
			runJSONStatus()
			return
		}

		// Check if interactive mode requested and available
		if interactiveUI && tui.ShouldUseInteractive(true, color.NoColor) {
			runInteractiveDashboard()
			return
		}

		// Fall back to standard tabwriter output
		runStandardStatus()
	},
}

// runJSONStatus outputs status data as JSON
func runJSONStatus() {
	data, err := gatherStatusData()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to gather status: %v\n", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to encode JSON: %v\n", err)
		os.Exit(1)
	}
}

// runStandardStatus displays status using the traditional tabwriter format
func runStandardStatus() {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()

	// Load manifest once for use in multiple sections
	manifest, _, _ := findAndReadManifest()

	headerColor.Fprintf(w, "--- 📊 Citadel Node Status (%s) ---\n", Version)

	headerColor.Fprintln(w, "\n🛠️ WORKER")
	printWorkerInfo(w)

	headerColor.Fprintln(w, "\n💻 SYSTEM VITALS")
	printMemInfo(w)
	printCPUInfo(w)
	printDiskInfo(w)

	headerColor.Fprintln(w, "\n🗂️ CACHE USAGE (~/citadel-cache)")
	printCacheInfo(w)

	headerColor.Fprintln(w, "\n💎 GPU STATUS")
	printGPUInfo(w)

	headerColor.Fprintln(w, "\n🧮 RESOURCE CONSUMERS")
	printResourcesInfo(w)

	headerColor.Fprintln(w, "\n🔧 CAPABILITIES")
	printCapabilities(w, manifest)

	headerColor.Fprintln(w, "\n🌐 NETWORK STATUS")
	printNetworkInfo(w, manifest)
	printPeerInfo(w)

	// Only show Job Queue section if configured
	if os.Getenv("REDIS_URL") != "" {
		headerColor.Fprintln(w, "\n📋 JOB QUEUE")
		printJobQueueInfo(w)
	}

	headerColor.Fprintln(w, "\n🚀 MANAGED SERVICES")
	printServiceInfo(w)
}

// printWorkerInfo reports whether a `citadel work` worker currently holds the
// single-instance lock for this node (issue #524). It mirrors the control-center
// precedent (cmd/controlcenter.go) which uses worklock.IsHeld to detect a live
// worker, and enriches the line with the recorded PID / version. Read-only: it
// never touches the lock (IsHeld probes on a separate fd and unlocks immediately).
func printWorkerInfo(w io.Writer) {
	stateDir := network.GetStateDir()
	held, pid := worklock.IsHeld(stateDir)
	if !held {
		fmt.Fprintf(w, "  %s\t%s\n", labelColor.Sprint("Status:"), warnColor.Sprint("not running"))
		// Still check the marker even with no worker detected: a stale or
		// missing marker is exactly the "no signal must not read as healthy"
		// case #726 asks for, and a marker left over from a crashed worker is
		// informative on its own.
		printHeartbeatFreshness(w)
		return
	}
	detail := fmt.Sprintf("PID %d", pid)
	if holder, ok := worklock.ReadHolder(stateDir); ok {
		if holder.Version != "" {
			detail += ", v" + holder.Version
		}
		if !holder.StartTime.IsZero() {
			detail += ", uptime " + humanizeUptime(time.Since(holder.StartTime))
		}
	}
	fmt.Fprintf(w, "  %s\t%s (%s)\n", labelColor.Sprint("Status:"), goodColor.Sprint("running"), detail)
	printPubSubInfo(w)
	printHeartbeatFreshness(w)
}

// printHeartbeatFreshness reports how long ago this node's DURABLE heartbeat
// write (the `node:status:stream` XADD / StreamAdd) last succeeded
// (citadel-cli#726). It is the node-local complement to printPubSubInfo: that
// function reports the live worker's best-effort pub/sub transport, while
// this one reports the reliable stream write the platform actually uses to
// decide whether the node is online -- the fact a healthy-looking node in
// citadel-cli#722 had no way to check about itself for 12 hours.
//
// Reads a marker file the publisher writes on every publish attempt
// (internal/heartbeat.RecordSuccess/RecordFailure) rather than talking to the
// worker process, since the worker (`citadel work`) and `citadel status` are
// separate processes -- the marker is the only cross-process channel. An
// absent marker (fresh install, worker never published, or a config dir the
// live worker does not share) is reported as "unknown", never as healthy.
func printHeartbeatFreshness(w io.Writer) {
	m := heartbeat.LoadMarker(network.GetNodeConfigDir())
	label := labelColor.Sprint("Heartbeat:")
	state, age := heartbeat.Freshness(m, time.Now(), heartbeat.DefaultStaleAfter)

	switch state {
	case heartbeat.FreshnessUnknown:
		fmt.Fprintf(w, "  %s\t%s\n", label,
			faintColor.Sprint("unknown (no successful durable heartbeat write recorded for this node yet)"))
	case heartbeat.FreshnessStale:
		detail := fmt.Sprintf("last success %s ago", age.Round(time.Second))
		// LastAttemptAt distinguishes "the worker is alive and every write is
		// failing" (fresh attempt, stale success) from "the worker stopped
		// publishing entirely" (attempt also stale) -- the two scenarios call
		// for different operator action, so surface both timestamps rather
		// than collapsing them into one "stale" verdict.
		if !m.LastAttemptAt.IsZero() && !m.LastAttemptAt.Equal(m.LastSuccessAt) {
			detail += fmt.Sprintf(", last attempt %s ago", time.Since(m.LastAttemptAt).Round(time.Second))
		}
		if m.ConsecutiveFailures > 0 {
			detail += fmt.Sprintf(", %d consecutive failure(s)", m.ConsecutiveFailures)
		}
		if m.LastError != "" {
			detail += fmt.Sprintf(": %s", m.LastError)
		}
		fmt.Fprintf(w, "  %s\t%s\n", label,
			warnColor.Sprint("STALE -- "+detail+" (the node's heartbeat may not be landing upstream)"))
	default: // FreshnessOK
		fmt.Fprintf(w, "  %s\t%s\n", label, goodColor.Sprintf("last success %s ago", age.Round(time.Second)))
	}
}

// printPubSubInfo reports which transport the running worker's pub/sub
// publishes are using (issue #723).
//
// This is the line that would have ended a twelve-hour outage in seconds. A
// worker whose startup WebSocket connect failed silently degraded to the HTTP
// publish fallback -- which does not work (citadel-cli#721) -- and nothing
// anywhere said so: the failure was logged at Debug, and every other status
// signal read healthy.
//
// Read over loopback from the worker's own status server, so it reflects the
// live process rather than a file written at startup. Best-effort: the status
// server is opt-in (--status-port / --gateway), so an unreachable server is
// reported as "unknown" rather than guessed at -- claiming "http" here when we
// simply could not ask would be a worse lie than saying nothing.
func printPubSubInfo(w io.Writer) {
	transport, state := probeWorkerPubSubTransport()
	label := labelColor.Sprint("Pub/Sub:")
	switch {
	case state == pubSubProbeUnreachable:
		fmt.Fprintf(w, "  %s\t%s\n", label,
			faintColor.Sprint("unknown (nothing is listening on the worker status server's loopback port; "+
				"it is opt-in, so enable it with --status-port or --gateway)"))
	case state == pubSubProbeTimedOut:
		fmt.Fprintf(w, "  %s\t%s\n", label,
			faintColor.Sprint("unknown (the worker status server IS enabled but did not answer in time: "+
				"the node is busy or the server is wedged, so re-running --status-port/--gateway will not help)"))
	case state == pubSubProbeMalformed:
		fmt.Fprintf(w, "  %s\t%s\n", label,
			faintColor.Sprint("unknown (the worker status server answered with a payload this build could not parse)"))
	case state == pubSubProbeBadStatus:
		fmt.Fprintf(w, "  %s\t%s\n", label,
			faintColor.Sprint("unknown (the worker status server answered with an error status: it IS running, "+
				"so check its logs rather than the --status-port/--gateway setting)"))
	case state == pubSubProbeNotReported:
		fmt.Fprintf(w, "  %s\t%s\n", label,
			faintColor.Sprint("unknown (worker reports no pub/sub transport: not API mode, or an older build)"))
	case transport == redisapi.PubSubTransportWebSocket:
		fmt.Fprintf(w, "  %s\t%s\n", label, goodColor.Sprint("websocket (real-time)"))
	case transport == redisapi.PubSubTransportHTTP:
		fmt.Fprintf(w, "  %s\t%s\n", label,
			warnColor.Sprint("HTTP fallback: real-time pub/sub is DOWN (heartbeat may not reach the platform)"))
	default:
		fmt.Fprintf(w, "  %s\t%s\n", label, transport)
	}
}

// pubSubProbeState distinguishes the two ways the transport can be unknown, so
// the status line says WHY rather than implying a verdict it did not obtain.
type pubSubProbeState int

const (
	pubSubProbeOK pubSubProbeState = iota
	// pubSubProbeUnreachable: nothing is listening (connection refused, no
	// route to the port). The worker's status server is opt-in (--status-port /
	// --gateway), so this is the common case, and the operator action is to
	// enable it.
	pubSubProbeUnreachable
	// pubSubProbeTimedOut: something IS listening but did not answer inside the
	// probe's bound. The operator action is the OPPOSITE of the above: the
	// server is already enabled, so telling them to enable it sends them to fix
	// a setting that is not the problem (citadel-cli#735).
	pubSubProbeTimedOut
	// pubSubProbeNotReported: the server answered but carried no transport --
	// direct-Redis mode (no WebSocket/HTTP split) or a pre-#723 build.
	pubSubProbeNotReported
	// pubSubProbeMalformed: the server answered with a body we could not decode.
	// Distinct from Unreachable: a reply we cannot parse is not an absent server,
	// and reporting it as one would send the operator to enable something that is
	// already running.
	pubSubProbeMalformed
	// pubSubProbeBadStatus: the server answered with a non-2xx we cannot use (and
	// that is not the 404 meaning "no such route"). Same reason as the two above
	// for not folding it into Unreachable: an HTTP status is proof something
	// answered, so "enable --status-port/--gateway" would be advice to change a
	// setting that is not the problem.
	pubSubProbeBadStatus
)

// Probe bounds. Package vars so tests can shrink them rather than sleep.
var (
	// pubSubProbeCheapTimeout bounds the /worker probe. /worker reads a cached
	// field and shells out to nothing, so anything slower than this is a wedged
	// server, not a busy one.
	pubSubProbeCheapTimeout = 2 * time.Second
	// pubSubProbeFullTimeout bounds the /status fallback used against a worker
	// too old to serve /worker. /status runs a full collection (docker stats per
	// running service plus nvidia-smi), measured at 1.98-2.67s on a gateway node,
	// so this must sit well above a realistic sweep. It is a compatibility path:
	// on a worker that serves /worker it is never reached.
	pubSubProbeFullTimeout = 10 * time.Second
)

// probeWorkerPubSubTransport reads worker.pubsub_transport off the running
// worker over loopback.
//
// It asks /worker first. The transport is a single cached field, and coupling it
// to /status's full collection made the answer a coin flip on exactly the busy
// gateway nodes the line was written for (citadel-cli#735). /status remains the
// fallback for a worker predating /worker. Version skew is normal here, since
// the probing binary is whatever `citadel status` the operator just ran while
// the serving binary is a long-lived `citadel work` nobody has restarted.
func probeWorkerPubSubTransport() (string, pubSubProbeState) {
	port := resolveStatusPort()
	if port <= 0 {
		return "", pubSubProbeUnreachable
	}

	body, err := httpGetBodyErr(&http.Client{Timeout: pubSubProbeCheapTimeout},
		fmt.Sprintf("http://127.0.0.1:%d/worker", port))
	if err == nil {
		return parsePubSubTransport(body)
	}
	// Only a 404 earns the fallback. 404 is the one status that means "this build
	// has no /worker route": internal/status.Server.buildMux registers no "/"
	// catch-all, so an unregistered path is answered by net/http itself. Every
	// other outcome is either a transport failure that will repeat on /status, or
	// a server that answered and simply could not serve this. Paying for a full
	// collection on either buys nothing but the sweep's latency, which is the
	// cost this whole change exists to avoid.
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusNotFound {
		return "", classifyProbeError(err)
	}

	// The server answered but has no /worker route: an older worker. Pay for the
	// full collection, with a bound above a realistic sweep this time.
	body, err = httpGetBodyErr(&http.Client{Timeout: pubSubProbeFullTimeout},
		fmt.Sprintf("http://127.0.0.1:%d/status", port))
	if err != nil {
		return "", classifyProbeError(err)
	}
	return parsePubSubTransport(body)
}

// classifyProbeError maps a probe failure onto the state whose message names the
// right operator action. Only a failure to reach anything at all may return
// Unreachable: that is the one state whose message says "enable the status
// server", and saying it to someone whose server just answered is the exact
// conflation citadel-cli#735 is about.
func classifyProbeError(err error) pubSubProbeState {
	if isTimeoutError(err) {
		return pubSubProbeTimedOut
	}
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) {
		return pubSubProbeBadStatus
	}
	return pubSubProbeUnreachable
}

// isTimeoutError reports whether err is a deadline/timeout rather than a
// connection failure. http.Client.Timeout surfaces as a *url.Error whose
// Timeout() is true; the context check is a belt for the deadline propagated
// into the request.
func isTimeoutError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// parsePubSubTransport extracts worker.pubsub_transport from a /status payload.
// Split out from the fetch so the "older worker / no worker / malformed" cases
// are unit-testable without a live status server.
func parsePubSubTransport(body []byte) (string, pubSubProbeState) {
	var payload struct {
		Worker *struct {
			PubSubTransport string `json:"pubsub_transport"`
		} `json:"worker"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", pubSubProbeMalformed
	}
	if payload.Worker == nil || payload.Worker.PubSubTransport == "" {
		return "", pubSubProbeNotReported
	}
	return payload.Worker.PubSubTransport, pubSubProbeOK
}

// runInteractiveDashboard runs the interactive TUI dashboard
func runInteractiveDashboard() {
	// Gather initial data
	data, _ := gatherStatusData()

	// Run the tview dashboard with refresh callback
	if err := dashboard.RunTviewDashboard(data, gatherStatusData); err != nil {
		fmt.Fprintf(os.Stderr, "Dashboard error: %v\n", err)
		os.Exit(1)
	}
}

// gatherStatusData collects all status data for the dashboard
func gatherStatusData() (dashboard.StatusData, error) {
	data := dashboard.StatusData{
		Version: Version,
	}

	// Load manifest
	manifest, _, _ := findAndReadManifest()
	if manifest != nil {
		data.NodeName = manifest.Node.Name
		data.OrgID = manifest.Node.OrgID
		data.Tags = manifest.Node.Tags
	}

	// Get hostname if not in manifest
	if data.NodeName == "" {
		data.NodeName, _ = os.Hostname()
	}

	// System vitals - Memory
	if v, err := mem.VirtualMemory(); err == nil {
		data.MemoryPercent = v.UsedPercent
		data.MemoryUsed = formatBytes(v.Used)
		data.MemoryTotal = formatBytes(v.Total)
	}

	// System vitals - CPU
	if percentages, err := cpu.Percent(500*time.Millisecond, false); err == nil && len(percentages) > 0 {
		data.CPUPercent = percentages[0]
	}

	// System vitals - Disk
	if d, err := disk.Usage("/"); err == nil {
		data.DiskPercent = d.UsedPercent
		data.DiskUsed = formatBytes(d.Used)
		data.DiskTotal = formatBytes(d.Total)
	}

	// GPU info
	if detector, err := platform.GetGPUDetector(); err == nil && detector.HasGPU() {
		if gpus, err := detector.GetGPUInfo(); err == nil {
			for _, gpu := range gpus {
				gpuInfo := dashboard.GPUInfo{
					Name:        gpu.Name,
					Memory:      gpu.Memory,
					Temperature: gpu.Temperature,
					Driver:      gpu.Driver,
				}
				if gpu.Utilization != "" {
					utilStr := strings.TrimSuffix(gpu.Utilization, "%")
					if util, err := strconv.ParseFloat(utilStr, 64); err == nil {
						gpuInfo.Utilization = util
					}
				}
				data.GPUs = append(data.GPUs, gpuInfo)
			}
		} else {
			// nvidia-smi failed but hardware is present — show lspci info
			hwName := platform.DetectNvidiaHardware()
			if hwName != "" {
				errMsg := platform.NvidiaSMIErrorMessage(err)
				data.GPUs = append(data.GPUs, dashboard.GPUInfo{
					Name:   hwName + " (drivers not loaded)",
					Driver: errMsg,
				})
			}
		}
	}

	// Network status
	if network.HasState() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		connected, _ := network.VerifyOrReconnect(ctx)
		data.Connected = connected

		if status, err := network.GetGlobalStatus(ctx); err == nil {
			data.NodeIP = status.IPv4
			if data.NodeName == "" {
				data.NodeName = status.Hostname
			}
		}

		// Get peers
		myIP, _ := network.GetGlobalIPv4()
		if peers, err := network.GetGlobalPeers(ctx); err == nil {
			for _, peer := range peers {
				if peer.IP != myIP {
					peerInfo := dashboard.PeerInfo{
						Hostname: peer.Hostname,
						IP:       peer.IP,
						Online:   peer.Online,
					}

					// Ping online peers
					if peer.Online {
						pingCtx, pingCancel := context.WithTimeout(context.Background(), 1*time.Second)
						if latency, connType, _, err := network.PingPeer(pingCtx, peer.IP); err == nil {
							peerInfo.Latency = fmt.Sprintf("%.0fms", latency)
							peerInfo.ConnType = connType
						}
						pingCancel()
					}

					data.Peers = append(data.Peers, peerInfo)
				}
			}
		}
	}

	// Services
	if manifest != nil {
		configDir := ""
		if m, cd, err := findAndReadManifest(); err == nil && m != nil {
			configDir = cd
		}

		for _, service := range manifest.Services {
			svcStatus := dashboard.ServiceStatus{
				Name:   service.Name,
				Status: "stopped",
			}

			if configDir != "" {
				fullComposePath := filepath.Join(configDir, service.ComposeFile)
				if _, err := os.Stat(fullComposePath); err == nil {
					if state, err := composeServiceState(fullComposePath, service.Name); err == nil {
						svcStatus.Status = state.State
					}
				}
			}

			data.Services = append(data.Services, svcStatus)
		}
	}

	// Heartbeat freshness (citadel-cli#726). Age is derivable from
	// HeartbeatLastSuccessAt by callers, so it is not duplicated on the wire.
	if m := heartbeat.LoadMarker(network.GetNodeConfigDir()); m != nil {
		state, _ := heartbeat.Freshness(m, time.Now(), heartbeat.DefaultStaleAfter)
		data.HeartbeatKnown = state != heartbeat.FreshnessUnknown
		if data.HeartbeatKnown {
			data.HeartbeatLastSuccessAt = m.LastSuccessAt
			data.HeartbeatStale = state == heartbeat.FreshnessStale
		}
		data.HeartbeatLastAttemptAt = m.LastAttemptAt
		data.HeartbeatConsecutiveFailures = m.ConsecutiveFailures
		data.HeartbeatLastError = m.LastError
	}

	return data, nil
}

func printCacheInfo(w *tabwriter.Writer) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(w, "  %s\n", badColor.Sprintf("Could not determine home directory: %v", err))
		return
	}
	cacheDir := filepath.Join(homeDir, "citadel-cache")

	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		fmt.Fprintf(w, "  (Cache directory not found)\t\n")
		return
	}

	totalCmd := exec.Command("du", "-sh", cacheDir)
	totalOutput, err := totalCmd.Output()
	if err == nil {
		parts := strings.Fields(string(totalOutput))
		if len(parts) > 0 {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Total Size"), parts[0])
		}
	}

	fmt.Fprintf(w, "  %s:\n", labelColor.Sprint("Breakdown"))
	// Use Go's Glob to find all subdirectories/files
	entries, err := filepath.Glob(filepath.Join(cacheDir, "*"))
	if err != nil || len(entries) == 0 {
		fmt.Fprintf(w, "    (Empty)\n")
		return
	}

	// Iterate over the found entries and run `du` on each one
	for _, entry := range entries {
		cmd := exec.Command("du", "-sh", entry)
		output, err := cmd.Output()
		if err != nil {
			continue // Skip if we can't get the size of an entry
		}
		parts := strings.Fields(string(output))
		if len(parts) >= 2 {
			size := parts[0]
			// Use filepath.Base to get just the directory name
			name := filepath.Base(parts[1])
			fmt.Fprintf(w, "    - %s:\t%s\n", name, size)
		}
	}
}

func printMemInfo(w *tabwriter.Writer) {
	v, err := mem.VirtualMemory()
	if err != nil {
		fmt.Fprintf(w, "  🧠 Memory:\t%s\n", badColor.Sprintf("Error getting memory info: %v", err))
		return
	}
	percentStr := colorizePercent(v.UsedPercent)
	fmt.Fprintf(w, "  🧠 %s:\t%s (%s / %s)\n", labelColor.Sprint("Memory"), percentStr, formatBytes(v.Used), formatBytes(v.Total))
}

func printCPUInfo(w *tabwriter.Writer) {
	percentages, err := cpu.Percent(time.Second, false)
	if err != nil || len(percentages) == 0 {
		fmt.Fprintf(w, "  ⚡️ CPU Usage:\t%s\n", badColor.Sprintf("Error getting CPU info: %v", err))
		return
	}
	percentStr := colorizePercent(percentages[0])
	fmt.Fprintf(w, "  ⚡️ %s:\t%s\n", labelColor.Sprint("CPU Usage"), percentStr)
}

func printDiskInfo(w *tabwriter.Writer) {
	d, err := disk.Usage("/")
	if err != nil {
		fmt.Fprintf(w, "  💾 Disk (/):\t%s\n", badColor.Sprintf("Error getting disk info: %v", err))
		return
	}
	percentStr := colorizePercent(d.UsedPercent)
	fmt.Fprintf(w, "  💾 %s:\t%s (%s / %s)\n", labelColor.Sprint("Disk (/)"), percentStr, formatBytes(d.Used), formatBytes(d.Total))
}

func printGPUInfo(w *tabwriter.Writer) {
	detector, err := platform.GetGPUDetector()
	if err != nil {
		fmt.Fprintf(w, "  GPU:\t%s\n", badColor.Sprintf("Error: %v", err))
		return
	}

	if !detector.HasGPU() {
		fmt.Fprintln(w, "  GPU:\tNo GPU detected on this system.")
		return
	}

	gpus, err := detector.GetGPUInfo()
	if err != nil {
		// Hardware detected via lspci but nvidia-smi failed — show actionable message
		hwName := platform.DetectNvidiaHardware()
		if hwName != "" {
			fmt.Fprintf(w, "  GPU:\t%s\n", warnColor.Sprintf("NVIDIA hardware detected (drivers not working)"))
			fmt.Fprintf(w, "    %s:\t%s\n", labelColor.Sprint("Hardware"), hwName)
		} else {
			fmt.Fprintf(w, "  GPU:\t%s\n", warnColor.Sprint("Hardware detected, but could not get details"))
		}
		errMsg := platform.NvidiaSMIErrorMessage(err)
		fmt.Fprintf(w, "    %s:\t%s\n", labelColor.Sprint("Issue"), warnColor.Sprint(errMsg))
		return
	}

	for i, gpu := range gpus {
		fmt.Fprintf(w, "  %s %d:\t%s\n", labelColor.Sprint("GPU"), i, gpu.Name)

		if gpu.Memory != "" {
			fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Memory"), gpu.Memory)
		}

		if gpu.Temperature != "" {
			// Parse temperature to colorize it
			tempStr := strings.TrimSuffix(gpu.Temperature, "°C")
			fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Temp"), colorizeTemp(tempStr))
		}

		if gpu.Utilization != "" {
			// Parse utilization to colorize it
			utilStr := strings.TrimSuffix(gpu.Utilization, "%")
			if utilFloat, err := strconv.ParseFloat(utilStr, 64); err == nil {
				fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Util"), colorizePercent(utilFloat))
			} else {
				fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Util"), gpu.Utilization)
			}
		}

		if gpu.Driver != "" {
			fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Driver"), gpu.Driver)
		}
	}
}

// printResourcesInfo lists every GPU compute consumer on the node — managed and
// unmanaged alike — with its owner and a reclaimable flag (issue #427). This is
// the operator-facing view of the same snapshot the fabric pulls via /resources
// and the RESOURCE_SNAPSHOT job: leftover test/dev services that silently pin
// the GPU (the tei-gte leftover found on node 1054) show up here labeled "not
// managed by citadel", so an operator can free them by hand.
func printResourcesInfo(w *tabwriter.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	// Pass manifest-declared service names so a managed service running under a
	// bare (non-"citadel-"-prefixed) name is classified as managed, not flagged
	// reclaimable. The CLI has the manifest in hand; the server/job paths don't.
	var managedNames []string
	if manifest, _, err := findAndReadManifest(); err == nil && manifest != nil {
		for _, svc := range manifest.Services {
			managedNames = append(managedNames, svc.Name)
		}
	}
	snap := resmon.CollectWithManaged(ctx, managedNames)

	// Host RAM/disk headroom (citadel #833) is independent of GPU presence —
	// print it before the GPU-gated section below so a no-GPU/CPU-only node
	// still shows it. Blank when the underlying probe couldn't read it.
	if snap.Host.MemoryTotalBytes > 0 {
		fmt.Fprintf(w, "  %s:\t%s available / %s total\n", labelColor.Sprint("RAM"),
			formatBytes(snap.Host.MemoryAvailableBytes), formatBytes(snap.Host.MemoryTotalBytes))
	}
	if snap.Host.DiskTotalBytes > 0 {
		pathSuffix := ""
		if snap.Host.DiskPath != "" {
			pathSuffix = " (" + snap.Host.DiskPath + ")"
		}
		fmt.Fprintf(w, "  %s:\t%s available / %s total%s\n", labelColor.Sprint("Disk"),
			formatBytes(snap.Host.DiskAvailableBytes), formatBytes(snap.Host.DiskTotalBytes), pathSuffix)
	}

	if !snap.HasGPU {
		fmt.Fprintln(w, "  GPU:\tNo GPU / nvidia-smi unavailable — no GPU consumers to report.")
		return
	}

	fmt.Fprintf(w, "  %s:\t%s / %s used (%s free)\n", labelColor.Sprint("GPU Memory"),
		formatBytes(snap.GPU.UsedBytes), formatBytes(snap.GPU.TotalBytes), formatBytes(snap.GPU.FreeBytes))

	if len(snap.Consumers) == 0 {
		fmt.Fprintf(w, "  %s\n", goodColor.Sprint("No GPU compute processes — GPU is free."))
		return
	}

	for _, c := range snap.Consumers {
		ownerStr := c.Owner
		switch c.Kind {
		case resmon.OwnerCitadelManaged:
			ownerStr = goodColor.Sprint(c.Owner)
		case resmon.OwnerContainer, resmon.OwnerHost:
			ownerStr = warnColor.Sprint(c.Owner)
		}
		line := fmt.Sprintf("  - PID %d (%s):\tVRAM %s  RAM %s", c.PID, ownerStr,
			formatBytes(c.VRAMBytes), formatBytes(c.RSSBytes))
		if c.Reclaimable {
			line += "  " + badColor.Sprintf("⚠️ reclaimable — %s", c.Reason)
		}
		fmt.Fprintln(w, line)
	}
}

func printCapabilities(w *tabwriter.Writer, manifest *CitadelManifest) {
	// If manifest declares capabilities, show those; otherwise auto-detect
	nodeCaps := resolveCapabilities(manifest)

	if nodeCaps.GPU != nil && len(nodeCaps.GPU.Devices) > 0 {
		if nodeCaps.GPU.DriverStatus == "not_loaded" || nodeCaps.GPU.DriverStatus == "error" {
			// Hardware present but drivers not working — show status without routing tags
			fmt.Fprintf(w, "  %s:\t%d\n", labelColor.Sprint("GPU Count"), nodeCaps.GPU.Count)
			for i, dev := range nodeCaps.GPU.Devices {
				fmt.Fprintf(w, "  %s %d:\t%s %s\n", labelColor.Sprint("GPU"), i, dev.Name,
					warnColor.Sprint("(drivers not loaded)"))
			}
			if nodeCaps.GPU.DriverError != "" {
				fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Issue"), warnColor.Sprint(nodeCaps.GPU.DriverError))
			}
		} else {
			fmt.Fprintf(w, "  %s:\t%d\n", labelColor.Sprint("GPU Count"), nodeCaps.GPU.Count)
			for i, dev := range nodeCaps.GPU.Devices {
				vramStr := ""
				if dev.VRAMTag != "" {
					vramStr = fmt.Sprintf(" (%s)", strings.ToUpper(dev.VRAMTag))
				}
				fmt.Fprintf(w, "  %s %d:\t%s%s\n", labelColor.Sprint("GPU"), i, dev.Name, vramStr)
			}
		}
	} else {
		fmt.Fprintln(w, "  GPU:\tNone detected")
	}

	if len(nodeCaps.Engines) > 0 {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Engines"), strings.Join(nodeCaps.Engines, ", "))
	}

	if len(nodeCaps.Tags) > 0 {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Tags"), strings.Join(nodeCaps.Tags, ", "))
	}
}

// resolveCapabilities returns node capabilities from manifest if declared, or auto-detects.
func resolveCapabilities(manifest *CitadelManifest) *capabilities.NodeCapabilities {
	if manifest != nil && manifest.Capabilities != nil {
		return manifestToNodeCapabilities(manifest.Capabilities)
	}
	return capabilities.DetectNodeCapabilities()
}

// manifestToNodeCapabilities converts declared manifest capabilities to the common struct.
func manifestToNodeCapabilities(mc *ManifestCapabilities) *capabilities.NodeCapabilities {
	caps := &capabilities.NodeCapabilities{
		Engines: mc.Engines,
	}

	if len(mc.GPUs) > 0 {
		gpuCaps := &capabilities.GPUCapabilities{}
		totalCount := 0
		for _, mg := range mc.GPUs {
			count := mg.Count
			if count <= 0 {
				count = 1
			}
			totalCount += count
			tag := capabilities.NormalizeGPUName(mg.Name)
			vramTag := ""
			if mg.VRAMMb > 0 {
				vramTag = capabilities.NormalizeVRAM(fmt.Sprintf("%d", mg.VRAMMb))
				if vramTag != "" {
					vramTag += "gb"
				}
			}
			for i := 0; i < count; i++ {
				gpuCaps.Devices = append(gpuCaps.Devices, capabilities.GPUDevice{
					Name:    mg.Name,
					VRAMMb:  mg.VRAMMb,
					Tag:     tag,
					VRAMTag: vramTag,
				})
			}
		}
		gpuCaps.Count = totalCount
		caps.GPU = gpuCaps

		// Build tags from GPU info
		seen := make(map[string]bool)
		for i, dev := range gpuCaps.Devices {
			if dev.Tag != "" {
				tag := "gpu:" + dev.Tag
				if !seen[tag] {
					seen[tag] = true
					caps.Tags = append(caps.Tags, tag)
				}
			}
			if dev.VRAMTag != "" {
				tag := "vram:" + dev.VRAMTag
				if !seen[tag] {
					seen[tag] = true
					caps.Tags = append(caps.Tags, tag)
				}
			}
			if dev.Tag != "" && dev.VRAMTag != "" {
				indexedTag := fmt.Sprintf("gpu:%d:%s:%s", i, dev.Tag, dev.VRAMTag)
				if capabilities.ValidateTag(indexedTag) {
					caps.Tags = append(caps.Tags, indexedTag)
				}
			}
		}
	}

	// Add engine tags
	for _, engine := range mc.Engines {
		tag := "engine:" + engine
		if capabilities.ValidateTag(tag) {
			caps.Tags = append(caps.Tags, tag)
		}
	}

	caps.Tags = append(caps.Tags, "cpu:general")
	return caps
}

func printNetworkInfo(w *tabwriter.Writer, manifest *CitadelManifest) {
	// Check if we have network state
	if !network.HasState() {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), badColor.Sprint("🔴 OFFLINE (Not logged in)"))
		fmt.Fprintf(w, "  %s\n", "   Run 'citadel login' to connect to the AceTeam Network")
		return
	}

	// Try to reconnect if state exists but not connected
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	connected, reconnectErr := network.VerifyOrReconnect(ctx)
	if !connected {
		if reconnectErr != nil {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), warnColor.Sprintf("🟡 DISCONNECTED (%v)", reconnectErr))
		} else {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), badColor.Sprint("🔴 OFFLINE (Could not reconnect)"))
		}
		fmt.Fprintf(w, "  %s\n", "   Run 'citadel login' to re-authenticate")
		return
	}

	// Get detailed status
	status, err := network.GetGlobalStatus(ctx)
	if err != nil {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), warnColor.Sprint("⚠️  WARNING (Could not get network status)"))
		return
	}

	if status.Connected {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), goodColor.Sprint("🟢 ONLINE to AceTeam Network"))
		if status.Hostname != "" {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Hostname"), status.Hostname)
		}
		if status.IPv4 != "" {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("IP Address"), status.IPv4)
		}
		// Display organization info from manifest
		if manifest != nil && manifest.Node.OrgID != "" {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Organization"), manifest.Node.OrgID)
		}
		// Display node tags from manifest
		if manifest != nil && len(manifest.Node.Tags) > 0 {
			tagsStr := strings.Join(manifest.Node.Tags, ", ")
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Tags"), tagsStr)
		}
	} else {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), badColor.Sprint("🔴 OFFLINE (Not connected to AceTeam Network)"))
	}
}

func printPeerInfo(w *tabwriter.Writer) {
	// Only show peers if we're connected
	if !network.HasState() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Get our own IP to filter ourselves out of peer list
	myIP, _ := network.GetGlobalIPv4()

	peers, err := network.GetGlobalPeers(ctx)
	if err != nil {
		// Silently skip if we can't get peers (e.g., not connected)
		return
	}

	// Filter out ourselves from the peer list
	var otherPeers []network.PeerInfo
	for _, peer := range peers {
		if peer.IP != myIP {
			otherPeers = append(otherPeers, peer)
		}
	}

	if len(otherPeers) == 0 {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Peers"), "(no other nodes on network)")
		return
	}

	fmt.Fprintf(w, "  %s:\n", labelColor.Sprint("Peers"))
	for _, peer := range otherPeers {
		statusStr := badColor.Sprint("⚫")
		extraInfo := ""

		if peer.Online {
			statusStr = goodColor.Sprint("🟢")

			// Ping online peers (with short timeout)
			pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
			latency, connType, relay, err := network.PingPeer(pingCtx, peer.IP)
			pingCancel()

			if err == nil {
				extraInfo = fmt.Sprintf(" %.0fms", latency)
				if connType == "relay" && relay != "" {
					extraInfo += fmt.Sprintf(" [relay:%s]", relay)
				} else if connType == "direct" {
					extraInfo += " [direct]"
				}
			}

			// Add OS if available
			if peer.OS != "" {
				extraInfo += fmt.Sprintf(" (%s)", peer.OS)
			}
		}

		// Show hostname and IP
		peerDisplay := peer.Hostname
		if peer.IP != "" {
			peerDisplay = fmt.Sprintf("%s %s", peer.Hostname, peer.IP)
		}

		fmt.Fprintf(w, "    %s %s%s\n", statusStr, peerDisplay, extraInfo)
	}
}

func printJobQueueInfo(w *tabwriter.Writer) {
	// Get Redis URL from environment (same as work command)
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		return
	}

	// Get queue name from environment or use default
	queueName := os.Getenv("WORKER_QUEUE")
	if queueName == "" {
		queueName = "jobs:v1:gpu-general"
	}

	consumerGroup := os.Getenv("CONSUMER_GROUP")
	if consumerGroup == "" {
		consumerGroup = "citadel-workers"
	}

	// Extract just the channel/tag part for display (e.g., "gpu-general" from "jobs:v1:gpu-general")
	channel := extractChannelName(queueName)

	// Connect to Redis
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Status"), warnColor.Sprintf("Invalid URL: %v", err))
		return
	}

	if password := os.Getenv("REDIS_PASSWORD"); password != "" {
		opts.Password = password
	}

	client := redis.NewClient(opts)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Check connection
	if err := client.Ping(ctx).Err(); err != nil {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Status"), badColor.Sprintf("❌ Connection failed: %v", err))
		return
	}

	fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Status"), goodColor.Sprint("🟢 Listening"))
	fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Channel"), channel)

	// Get queue length (XLEN) - displayed as "Pending Jobs"
	queueLen, err := client.XLen(ctx, queueName).Result()
	if err == nil {
		fmt.Fprintf(w, "  %s:\t%d\n", labelColor.Sprint("Pending Jobs"), queueLen)
	}

	// Get in-progress count (XPENDING)
	pending, err := client.XPending(ctx, queueName, consumerGroup).Result()
	if err == nil && pending != nil {
		fmt.Fprintf(w, "  %s:\t%d\n", labelColor.Sprint("In Progress"), pending.Count)
	}

	// Get DLQ count - displayed as "Failed Jobs"
	dlqName := getDLQName(queueName)
	dlqLen, err := client.XLen(ctx, dlqName).Result()
	if err == nil {
		colorFn := goodColor
		if dlqLen > 0 {
			colorFn = warnColor
		}
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Failed Jobs"), colorFn.Sprintf("%d", dlqLen))
	}
}

func extractChannelName(queueName string) string {
	// "jobs:v1:gpu-general" → "gpu-general"
	parts := strings.Split(queueName, ":")
	if len(parts) >= 3 {
		return parts[len(parts)-1]
	}
	return queueName
}

func getDLQName(queueName string) string {
	parts := strings.Split(queueName, ":")
	suffix := parts[len(parts)-1]
	return fmt.Sprintf("dlq:v1:%s", suffix)
}

func printServiceInfo(w *tabwriter.Writer) {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		// The error from findAndReadManifest is already user-friendly
		fmt.Fprintf(w, "  %s\n", badColor.Sprint(err.Error()))
		return
	}

	if len(manifest.Services) == 0 {
		fmt.Fprintln(w, "  No managed services are configured.")
		return
	}

	// If services are listed in the manifest, the 'services' directory must exist.
	servicesDir := filepath.Join(configDir, "services")
	if _, statErr := os.Stat(servicesDir); os.IsNotExist(statErr) {
		fmt.Fprintf(w, "  %s\n", warnColor.Sprint("⚠️  Configuration Error"))
		fmt.Fprintf(w, "    The configuration file lists services, but the 'services' directory is missing.\n")
		fmt.Fprintf(w, "    Expected at: %s\n", servicesDir)
		return
	}

	for _, service := range manifest.Services {
		fullComposePath := filepath.Join(configDir, service.ComposeFile)

		// Proactively check if the compose file exists to provide a better error message.
		if _, statErr := os.Stat(fullComposePath); os.IsNotExist(statErr) {
			fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, warnColor.Sprint("⚠️  Configuration Error"))
			fmt.Fprintf(w, "    Compose file not found: %s\n", service.ComposeFile)
			continue
		}

		psArgs := append(composeFileArgs(fullComposePath, fullComposePath), "ps", "--format", "json")
		psCmd := composeCommand(psArgs...)
		output, err := psCmd.CombinedOutput() // Use CombinedOutput to get stderr
		if err != nil {
			errMsg := string(output)
			if strings.Contains(errMsg, "permission denied") && strings.Contains(errMsg, "docker.sock") {
				fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, badColor.Sprint("❌ PERMISSION DENIED"))
				fmt.Fprintf(w, "    %s\n", "Could not connect to the Docker daemon.")
				fmt.Fprintf(w, "    %s\n", "Hint: Add your user to the 'docker' group (`sudo usermod -aG docker $USER`)")
				fmt.Fprintf(w, "    %s\n", "      then log out and log back in for the change to take effect.")
			} else {
				fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, warnColor.Sprint("⚠️  Could not get status"))
				fmt.Fprintf(w, "    %s\n", strings.TrimSpace(errMsg))
			}
			continue
		}

		svcState := composeServiceStateFrom(output, fullComposePath, service.Name)
		switch {
		case svcState.Running:
			statusStr := goodColor.Sprint("🟢 RUNNING")
			if svcState.Native {
				statusStr += labelColor.Sprint(" (native)")
			}
			// For running serving engines, show the model(s) actually loaded,
			// e.g. "🟢 RUNNING (Qwen/Qwen3-8B)" (#529). Non-engine services and
			// engines that don't answer within the short probe window print
			// unchanged.
			if models := discoverServiceModels(service.Name); len(models) > 0 {
				statusStr += fmt.Sprintf(" (%s)", strings.Join(models, ", "))
			}
			fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, statusStr)
		case svcState.Container != nil:
			// A container exists for this service but is not up. Report its real
			// state rather than a bare STOPPED so a crash loop is visible.
			raw := strings.ToUpper(svcState.Container.State)
			if strings.Contains(raw, "EXITED") || strings.Contains(raw, "DEAD") {
				fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, badColor.Sprintf("🔴 %s", raw))
			} else {
				fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, warnColor.Sprintf("🟡 %s", raw))
			}
		default:
			fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, labelColor.Sprint("⚫ STOPPED"))
		}
	}
}

// discoverServiceModels returns the LOADED model(s) for a running serving
// engine service (vllm/ollama/llamacpp), or nil for non-engine services and
// for engines that fail to answer quickly (#529). The port comes from the
// citadel-owned host-port registry (services.ManagedServiceHostPort), falling
// back to the engine's well-known native port (e.g. ollama 11434). Failures
// are silent: status rendering must never stall or error on a slow engine, so
// the probe is bounded by a short timeout.
func discoverServiceModels(serviceName string) []string {
	engine := statuspkg.EngineTypeFromName(serviceName)
	if engine == "" {
		return nil
	}
	port, ok := svcports.ManagedServiceHostPort(engine)
	if !ok {
		port = statuspkg.InferServicePort(engine)
	}
	if port <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), statuspkg.ModelDiscoveryTimeout)
	defer cancel()
	models, err := statuspkg.NewModelDiscovery().DiscoverModels(ctx, engine, port)
	if err != nil {
		return nil
	}
	return models
}

func colorizePercent(p float64) string {
	s := fmt.Sprintf("%.1f%%", p)
	if p > 90.0 {
		return badColor.Sprint(s)
	}
	if p > 75.0 {
		return warnColor.Sprint(s)
	}
	return goodColor.Sprint(s)
}

func colorizeTemp(t string) string {
	temp, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return t
	}
	s := fmt.Sprintf("%s°C", t)
	if temp > 85.0 {
		return badColor.Sprint(s)
	}
	if temp > 70.0 {
		return warnColor.Sprint(s)
	}
	return goodColor.Sprint(s)
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func init() {
	rootCmd.AddCommand(statusCmd)
	statusCmd.Flags().BoolVarP(&interactiveUI, "interactive", "i", false, "Launch interactive dashboard with live updates")
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output in JSON format")
}
