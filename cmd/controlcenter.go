// cmd/controlcenter.go
package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/capabilities"
	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/clilog"
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/demo"
	"github.com/aceteam-ai/citadel-cli/internal/deskstream"
	"github.com/aceteam-ai/citadel-cli/internal/desktop"
	"github.com/aceteam-ai/citadel-cli/internal/heartbeat"
	"github.com/aceteam-ai/citadel-cli/internal/instance"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/nodestate"
	"github.com/aceteam-ai/citadel-cli/internal/pairingdisplay"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	pmx "github.com/aceteam-ai/citadel-cli/internal/proxmox"
	"github.com/aceteam-ai/citadel-cli/internal/pulse"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/teamchat"
	"github.com/aceteam-ai/citadel-cli/internal/telemetry"
	"github.com/aceteam-ai/citadel-cli/internal/terminal"
	"github.com/aceteam-ai/citadel-cli/internal/tui"
	"github.com/aceteam-ai/citadel-cli/internal/tui/controlcenter"
	"github.com/aceteam-ai/citadel-cli/internal/tui/whimsy"
	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/aceteam-ai/citadel-cli/internal/usage"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/aceteam-ai/citadel-cli/internal/workflow"
	"github.com/aceteam-ai/citadel-cli/internal/worklock"
	"github.com/aceteam-ai/citadel-cli/services"
	"github.com/google/uuid"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/spf13/cobra"
)

// Worker state for TUI management
var (
	ccWorkerMu      sync.Mutex
	ccWorkerRunning bool
	// ccWorkerConsuming is true only while runTUIWorker is actually THIS
	// process's job consumer for the node -- i.e. it passed the workerHeld
	// check and reached its own consume loop, rather than falling into
	// monitor-only mode beside a dedicated `citadel work`. It backs
	// controlcenter.WorkerCallbacks.OwnsConsumption, the signal the
	// detach-on-quit modal (issue #658) uses to decide whether quitting this
	// TUI would actually stop job processing for the node.
	ccWorkerConsuming bool
	ccWorkerCancel    context.CancelFunc
	ccWorkerQueue     string
	ccActivityFn      func(level, msg string)
	ccHeartbeatFn     func(active bool) // Callback when heartbeat publishes
)

// Demo server state
var (
	ccDemoServer *demo.Server
	ccDemoCancel context.CancelFunc
	ccDemoPort   = 7777
)

// Terminal server state.
//
// ccTerminalServer and ccTerminalRunning are accessed concurrently by the VPN
// listener supervisor (ccSuperviseVPNListeners and the attach* helpers it
// calls) and the start/stop paths, so they are atomics. They must NOT be
// guarded by ccVPNMu: startTerminalServer sets the running flag and then calls
// attachTerminalVPNListener, which locks ccVPNMu — a plain mutex around the
// flag would re-enter and deadlock (issue #319). ccTerminalOrgID/APIToken are
// only touched on the start/stop paths (never by the supervisor) so they stay
// plain.
var (
	ccTerminalServer   atomic.Pointer[terminal.Server]
	ccTerminalAuth     *terminal.CachingTokenValidator
	ccTerminalPort     = 7860
	ccTerminalRunning  atomic.Bool
	ccTerminalOrgID    string // orgID the terminal server was started with
	ccTerminalAPIToken string // device API token the terminal server was started with
)

// VNC server state. ccVNCServer/ccVNCRunning are atomics for the same reason as
// the terminal globals above (concurrent supervisor access; ccVPNMu must not
// guard them or attachVNCVPNListener would re-enter it). See issue #319.
var (
	ccVNCServer  atomic.Pointer[desktop.VNCServer]
	ccVNCPort    = 5900
	ccVNCRunning atomic.Bool
)

// H.264 desktop stream server state (citadel-cli#338). Mirrors the VNC state
// above: atomics for concurrent supervisor access, exposed over the tsnet mesh
// via attachH264VPNListener. Gated on the same desktop permission as VNC.
var (
	ccH264Server  atomic.Pointer[deskstream.Server]
	ccH264Port    = deskstream.DefaultPort
	ccH264Running atomic.Bool
)

// vpnListener wraps a tsnet VPN listener with a liveness flag. tsnet does not
// expose an explicit "listener torn down on reconnect" event, so we detect
// death the only reliable way available: when the server's accept loop calls
// Accept() and it returns a non-recoverable error (the listener was closed by
// the tsnet teardown). The flag is set from inside Accept, so it works
// regardless of whether the tailnet IP changed — covering the issue #317
// same-IP blip where the node returns to the *same* address with a dead
// listener (machine key preserved → same IP). See issue #317.
type vpnListener struct {
	net.Listener
	dead   atomic.Bool
	closed atomic.Bool // set when we deliberately Close() it (re-attach/shutdown)
}

func (l *vpnListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil && !l.closed.Load() && errors.Is(err, net.ErrClosed) {
		// Only a closed/torn-down listener is terminal — mark it dead so the
		// supervisor re-attaches it. This mirrors the server accept loops, which
		// treat only net.ErrClosed as terminal and continue on transient errors
		// (issue #319): a transient Accept error must NOT trigger a needless
		// tear-down/rebind. (When we close it on purpose, closed is already set.)
		l.dead.Store(true)
	}
	return conn, err
}

func (l *vpnListener) Close() error {
	l.closed.Store(true)
	return l.Listener.Close()
}

func (l *vpnListener) isDead() bool { return l.dead.Load() }

// VPN-listener attachment state (issue #317).
//
// The VPN listener lifecycle is deliberately tracked separately from the
// server "running" flags above. A tsnet drop+recover tears down the listener
// bound to the tailnet IP while the localhost server keeps running, so the
// supervisor must be able to (re)attach a VPN listener even when the server
// object is already running. ccVPNMu guards all of the fields below and
// serializes attach attempts so the supervisor and the startup/login paths
// never race.
var (
	ccVPNMu sync.Mutex

	ccVNCVPNListener *vpnListener // nil when no VNC VPN listener is attached
	ccVNCVPNIP       string       // tailnet IP the current VNC VPN listener is bound to

	ccH264VPNListener *vpnListener // nil when no H.264 VPN listener is attached
	ccH264VPNIP       string       // tailnet IP the current H.264 VPN listener is bound to

	ccTerminalVPNListener *vpnListener // nil when no terminal VPN listener is attached
	ccTerminalVPNIP       string       // tailnet IP the current terminal VPN listener is bound to
)

// listenVPNFn / isConnectedFn / currentVPNIPFn are indirection points for the
// network layer so the VPN-listener supervisor can be unit-tested without a
// live tailnet. Production wiring uses the real network package functions.
var (
	listenVPNFn    = network.ListenVPN
	isConnectedFn  = network.IsGlobalConnected
	currentVPNIPFn = network.GetGlobalIPv4
)

// runControlCenter launches the unified control center TUI
func runControlCenter() {
	if !tui.IsTTY() {
		fmt.Fprintln(os.Stderr, "Control center requires a terminal. Run 'citadel work' for headless/background operation.")
		os.Exit(1)
	}

	// Single-instance detection: a control-center TUI is already running (it holds
	// citadel.sock) in another terminal. Two terminals cannot share one bubbletea
	// TUI, so name the running instance and exit cleanly rather than silently
	// dropping the user into a surprise shell on the node (the reported confusion:
	// "this should've reattached to the TUI but went to a console"). An explicit
	// shell into the node remains available via `citadel attach --shell`.
	configDir := platform.ConfigDir()
	if instance.IsRunning(configDir) {
		if pid := instance.PID(configDir); pid > 0 {
			fmt.Fprintf(os.Stderr, "Citadel control center is already running (PID %d) in another terminal.\n", pid)
		} else {
			fmt.Fprintln(os.Stderr, "Citadel control center is already running in another terminal.")
		}
		fmt.Fprintln(os.Stderr, "Switch to that terminal to use it, or run `citadel attach --shell` for a shell on this node.")
		return
	}

	// Auto-update on startup
	if updated := ccAutoUpdate(); updated {
		// Binary was updated, restart
		return
	}

	// Suppress tsnet/tailscale logs to prevent TUI corruption
	network.SuppressLogs()

	// Start the instance server for attach support
	instanceServer, _ := instance.Listen(configDir)
	if instanceServer != nil {
		_ = instance.WritePID(configDir)
	}

	// Start the demo server in the background
	startDemoServer()

	cfg := controlcenter.Config{
		Version:            Version,
		AuthServiceURL:     authServiceURL,
		NexusURL:           nexusURL,
		RefreshFn:          gatherControlCenterData,
		StartServiceFn:     ccStartService,
		StopServiceFn:      ccStopService,
		RestartServiceFn:   ccRestartService,
		AddServiceFn:       ccAddService,
		GetServicesFn:      services.GetAvailableServices,
		GetConfiguredFn:    ccGetConfiguredServices,
		GetServiceDetailFn: ccGetServiceDetail,
		GetServiceLogsFn:   ccGetServiceLogs,
		DeviceAuth: controlcenter.DeviceAuthCallbacks{
			StartFlow:  ccStartDeviceAuthFlow,
			PollToken:  ccPollDeviceAuthToken,
			Connect:    ccConnectWithAuthkey,
			Disconnect: ccDisconnect,
		},
		Worker: controlcenter.WorkerCallbacks{
			Start:           ccStartWorker,
			Stop:            ccStopWorker,
			IsRunning:       ccWorkerIsRunning,
			OwnsConsumption: ccWorkerOwnsConsumption,
		},
		Permissions: controlcenter.PermissionsCallbacks{
			Load: func() *config.Permissions {
				return config.LoadPermissions(platform.ConfigDir())
			},
			Save: func(p *config.Permissions) error {
				return config.SavePermissions(platform.ConfigDir(), p)
			},
		},
		OnConnect:       ccOnNetworkConnect,
		ConnectToPeerFn: ccConnectToPeer,
		// Chat is seeded with the snapshot resolvable at startup, but a node that
		// completes device authorization in-TUI persists its credentials only
		// afterward. ChatConfigProvider lets the ChatPage re-resolve them lazily
		// at connect() time — the same way the terminal/desktop/worker paths
		// resolve token+org at network-connect time — so a freshly-authed node no
		// longer shows a permanent "device authorization required" status.
		Chat:               buildChatConfig(),
		ChatConfigProvider: buildChatConfig,

		TeamChat:               buildTeamChatConfig(),
		TeamChatConfigProvider: buildTeamChatConfig,
		Proxmox:                buildProxmoxConfig(),
		Settings: controlcenter.SettingsCallbacks{
			LoadTelemetry: func() *config.Telemetry {
				return config.LoadTelemetry(platform.ConfigDir())
			},
			SaveTelemetry: func(t *config.Telemetry) error {
				return config.SaveTelemetry(platform.ConfigDir(), t)
			},
			LoadKeepAwake: func() *config.KeepAwake {
				return config.LoadKeepAwake(platform.ConfigDir())
			},
			SaveKeepAwake: func(k *config.KeepAwake) error {
				return config.SaveKeepAwake(platform.ConfigDir(), k)
			},
			LoadMouse: func() *config.Mouse {
				return config.LoadMouse(platform.ConfigDir())
			},
			SaveMouse: func(m *config.Mouse) error {
				return config.SaveMouse(platform.ConfigDir(), m)
			},
			// SetMouseEnabled is injected by the control center itself in Run(),
			// where the running tview app exists to receive EnableMouse.
			LoadRendering: func() *config.Rendering {
				return config.LoadRendering(platform.ConfigDir())
			},
			SaveRendering: func(r *config.Rendering) error {
				return config.SaveRendering(platform.ConfigDir(), r)
			},
			LoadMeeting: func() *config.Meeting {
				return config.LoadMeeting(platform.ConfigDir())
			},
			SaveMeeting: func(m *config.Meeting) error {
				return config.SaveMeeting(platform.ConfigDir(), m)
			},
			// SetFullscreenEnabled is intentionally left nil: tview cannot swap the
			// terminal's alternate-screen mode on a running app. Today the toggle only
			// persists the preference; wiring a launch-time consumer that reads it at
			// screen creation lives in the control center's Run() path and is a
			// follow-up.
		},
		// Resolve the initial mouse state: persisted preference with the
		// --no-mouse flag applied as a session override.
		MouseEnabled: controlcenter.ResolveMouseEnabled(noMouse, config.LoadMouse(platform.ConfigDir()).Enabled),
		// Resolve the initial fullscreen-rendering state from the persisted
		// preference (defaults on). Consumed once at launch in Run(); a
		// mid-session toggle only applies on the next start.
		FullscreenEnabled: config.LoadRendering(platform.ConfigDir()).Fullscreen,
		WhatsApp:          buildWhatsAppCallbacks(),
		ModuleInstall:     buildModuleInstallCallbacks(),
	}

	cc := controlcenter.New(cfg)

	// Show deferred update notification in TUI activity log (if any)
	if deferredUpdateNotification != "" {
		cc.AddActivity("info", fmt.Sprintf("Update available: %s -> %s (run 'citadel update install')", Version, deferredUpdateNotification))
	}

	// Set heartbeat callback so worker can update TUI
	ccHeartbeatFn = cc.UpdateHeartbeat

	// Supervise the VNC + terminal VPN listeners for the lifetime of the TUI so
	// they self-heal across tsnet reconnects without relaunching citadel (issue
	// #317). Runs independently of the one-shot auto-connect goroutine below,
	// which only fires at startup/login.
	supervisorCtx, supervisorCancel := context.WithCancel(context.Background())
	defer supervisorCancel()
	go ccSuperviseVPNListeners(supervisorCtx, cc.AddActivity)

	// Handle OS signals (SIGINT/SIGTERM) so the TUI exits cleanly even when
	// Ctrl+C is delivered as a signal rather than a tcell key event, or when
	// the process is sent SIGTERM by a service manager. cc.Stop() is guarded by
	// sync.Once and stops the tview event loop first, so calling it from this
	// goroutine unblocks cc.Run() and triggers the teardown path below. Without
	// this, a real SIGINT had no handler at all and the process hung forever
	// (issue #312).
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		cc.Stop()
	}()

	// Auto-connect to network and start worker
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		networkConnected := false

		// Try to reconnect if we have saved state
		if network.HasState() {
			cc.AddActivity("info", "Reconnecting to network...")
			connected, err := network.VerifyOrReconnect(ctx)
			if err != nil && errors.Is(err, network.ErrStaleState) {
				// Stale VPN keys — try auto-recovery with device API token
				deviceConfig := getDeviceConfigFromFile()
				if deviceConfig != nil && deviceConfig.DeviceAPIToken != "" {
					cc.AddActivity("info", "VPN keys expired, attempting auto-recovery...")
					apiBaseURL := deviceConfig.APIBaseURL
					if apiBaseURL == "" {
						apiBaseURL = authServiceURL
					}
					if freshKey, fetchErr := network.FetchFreshAuthkey(ctx, apiBaseURL, deviceConfig.DeviceAPIToken); fetchErr != nil {
						cc.AddActivity("warning", fmt.Sprintf("Auto-recovery failed: %v", fetchErr))
					} else {
						// Try reconnect with existing state (preserves IP)
						if ok, reconnErr := network.ReconnectWithAuthKey(ctx, freshKey); reconnErr == nil && ok {
							cc.AddActivity("success", "VPN reconnected (IP preserved)")
							networkConnected = true
						} else {
							// Clear state and connect fresh (new IP) — IDENTITY-CHURN
							// path. Warn loudly (same reasoning as recoverStaleVPN): the
							// persisted identity could not be re-authorized, so the node
							// re-registers with a new id/IP/device key. Root cause is
							// usually ephemeral registration; durable fix is #4584/#4583.
							hostname, _ := os.Hostname()
							warnIdentityChurn(hostname)
							cc.AddActivity("warning", "Node identity reset (new id/IP); re-run 'citadel init' to stop churn")
							_ = network.ClearState()
							freshCtx, freshCancel := context.WithTimeout(ctx, 15*time.Second)
							config := network.ServerConfig{
								Hostname:   hostname,
								ControlURL: nexusURL,
								AuthKey:    freshKey,
							}
							if _, connectErr := network.Connect(freshCtx, config); connectErr == nil {
								cc.AddActivity("success", "VPN reconnected (fresh state)")
								networkConnected = true
							} else {
								cc.AddActivity("warning", fmt.Sprintf("VPN recovery failed: %v", connectErr))
							}
							freshCancel()
						}
					}
				} else {
					cc.AddActivity("warning", "VPN keys expired, login required")
				}
			} else if err != nil {
				cc.AddActivity("warning", fmt.Sprintf("Network reconnect failed: %v", err))
			} else if connected {
				cc.AddActivity("success", "Connected to AceTeam Network")
				networkConnected = true
			}
		}

		// Gather initial data after network check
		if data, err := gatherControlCenterData(); err == nil {
			cc.UpdateData(data)
			networkConnected = data.Connected
		}

		// Set device URL for "V" key shortcut if connected
		if networkConnected {
			deviceURL := authServiceURL + "/fabric"
			if d, err := gatherControlCenterData(); err == nil && d.HeadscaleNodeID != "" {
				deviceURL = authServiceURL + "/fabric/machines/" + d.HeadscaleNodeID
			}
			cc.SetDeviceURL(deviceURL)
		}

		// Auto-start servers and worker after network is connected
		if networkConnected {
			ccOnNetworkConnect(cc.AddActivity)
			cc.ShowChat()
		} else {
			// Not connected - show login prompt after a short delay
			// to allow the TUI to fully initialize
			time.Sleep(1 * time.Second)
			cc.QueueShowLoginPrompt()
		}
	}()

	runErr := cc.Run()

	// Detach (issue #658): the user chose to leave the node's worker (and any
	// terminal/VNC/etc. servers this session started) running rather than
	// stopping them on quit. Only the TUI-owned subsystems are torn down here
	// (through the same bounded watchdog every other teardown step uses);
	// handleDetach keeps this process alive until it's told to stop, at which
	// point it runs the same node-service teardown the plain-quit path below
	// runs immediately.
	nodeSteps := nodeServiceShutdownSteps(instanceServer, configDir)

	if cc.DetachRequested() {
		gracefulShutdown([]shutdownStep{{name: "tui-cleanup", fn: cc.Cleanup}})
		handleDetach(nodeSteps)
		return
	}

	// Tear down all long-lived subsystems once the TUI event loop has exited.
	// Each step is bounded internally, but we run them through gracefulShutdown
	// so that a regression in any one teardown can never hang the process: after
	// a short grace period the watchdog force-exits, logging what was still
	// pending (issue #312). The background worker started by ccOnNetworkConnect
	// uses its own context.Background()-derived context and is NOT tied to the
	// TUI, so it must be cancelled explicitly here or its goroutines (Redis API
	// poll, heartbeat) would leak past exit.
	gracefulShutdown(append(
		[]shutdownStep{{name: "tui-cleanup", fn: cc.Cleanup}},
		nodeSteps...,
	))

	if runErr != nil {
		fmt.Fprintf(os.Stderr, "Control center error: %v\n", runErr)
		os.Exit(1)
	}
}

// nodeServiceShutdownSteps returns the teardown steps for every long-lived
// node-facing subsystem the control center may have started: the worker,
// the terminal/VNC/H.264/demo servers, and the instance server. It excludes
// the TUI-only "tui-cleanup" step (console/chat page teardown) deliberately
// -- callers add that separately, since the detach path (handleDetach, issue
// #658) runs it immediately on quit but defers everything in this slice
// until the detached process is actually told to stop.
func nodeServiceShutdownSteps(instanceServer *instance.Server, configDir string) []shutdownStep {
	return []shutdownStep{
		{name: "worker", fn: func() { _ = ccStopWorker() }},
		{name: "terminal-server", fn: stopTerminalServer},
		{name: "vnc-server", fn: stopVNCServer},
		{name: "h264-server", fn: stopH264Server},
		{name: "demo-server", fn: stopDemoServer},
		{name: "instance-server", fn: func() {
			if instanceServer != nil {
				instanceServer.Close()
				instance.RemovePID(configDir)
			}
		}},
	}
}

// handleDetach implements the bounded, honest form of "detach" chosen for
// issue #658: this OS process is kept alive instead of tearing down the
// worker/terminal/VNC/etc. subsystems it started, so the node keeps working
// exactly as it was while the Control Center was open. This is deliberately
// NOT full daemonization -- it does not fork/setsid/re-exec to free the
// controlling terminal the way `tmux detach` or a real background service
// would (that is real, non-trivial additional plumbing; see the PR for
// issue #658 for the tradeoff). The process remains attached to the
// terminal/session it was launched from, so closing that session can still
// end it (e.g. via SIGHUP) unless the operator backgrounds it themselves.
func handleDetach(nodeSteps []shutdownStep) {
	fmt.Println()
	fmt.Println("Detached: the Control Center display has closed. This node's worker")
	fmt.Println("(and any terminal/VNC servers this session started) keep running in")
	fmt.Printf("this process (PID %d).\n", os.Getpid())
	fmt.Println()
	fmt.Println("This process is still attached to the current terminal/session --")
	fmt.Println("closing it may stop the node unless you background this process")
	fmt.Println("yourself (Ctrl+Z then 'bg'/'disown', 'nohup citadel &', tmux/screen)")
	fmt.Println("or install Citadel as a system service ('citadel service install').")
	fmt.Println()
	fmt.Println("Press Ctrl+C, or send SIGTERM, to stop the node.")

	detachSigs := make(chan os.Signal, 1)
	signal.Notify(detachSigs, syscall.SIGINT, syscall.SIGTERM)
	<-detachSigs

	gracefulShutdown(nodeSteps)
	os.Exit(0)
}

// startDemoServer starts the demo HTTP server in the background
func startDemoServer() {
	// Create info callback for demo server
	getInfo := func() demo.NodeInfo {
		hostname, _ := os.Hostname()
		info := demo.NodeInfo{
			Hostname: hostname,
			Version:  Version,
		}

		// Check network connection
		if network.IsGlobalConnected() {
			info.Connected = true
			if ip, err := network.GetGlobalIPv4(); err == nil {
				info.NetworkIP = ip
			}
		}

		// Get services from manifest
		if manifest, _, err := findAndReadManifest(); err == nil {
			for _, svc := range manifest.Services {
				info.Services = append(info.Services, svc.Name)
			}
		}

		return info
	}

	ccDemoServer = demo.NewServer(ccDemoPort, Version, getInfo)

	// Start in background with cancellable context
	var ctx context.Context
	ctx, ccDemoCancel = context.WithCancel(context.Background())
	go func() {
		// Ignore error - server runs until cancelled
		_ = ccDemoServer.Start(ctx)
	}()
}

// stopDemoServer stops the demo HTTP server
func stopDemoServer() {
	if ccDemoCancel != nil {
		ccDemoCancel()
	}
	if ccDemoServer != nil {
		_ = ccDemoServer.Stop()
	}
}

// ccConnectToPeer opens an interactive shell on the given peer IP, wired into
// the Control Center's Network Peers modal (issue #761, Enter=connect) via
// controlcenter.Config.ConnectToPeerFn. Reuses the exact same bare-target
// routing `citadel connect <peer>` uses (#754/PR #756): the ts-net terminal
// endpoint first (with an interactive node-passcode prompt on the gate), and
// the legacy raw-sshd path only when the terminal endpoint itself is
// unreachable. A throwaway *cobra.Command carries no --user/--port/--raw/
// --mesh flags, so connectToNode always takes the default (ts-net-first,
// fallback-on-unreachable) route here, the same default a bare `citadel
// connect <peer>` gets from the CLI.
func ccConnectToPeer(ip string) error {
	return connectToNode(&cobra.Command{}, ip)
}

// ccOnNetworkConnect starts terminal server, VNC server, and worker after VPN connects.
// Called both on auto-reconnect at startup and after interactive login.
func ccOnNetworkConnect(activityFn func(level, msg string)) {
	if orgID := getOrgIDFromConfig(); orgID != "" {
		// Detect if credentials changed (e.g., after re-pairing). If the terminal
		// server is already running with stale credentials, restart it so the
		// CachingTokenValidator fetches fresh token hashes from the platform API.
		currentAPIToken := ""
		if cfg := getDeviceConfigFromFile(); cfg != nil {
			currentAPIToken = cfg.DeviceAPIToken
		}
		if ccTerminalRunning.Load() && (orgID != ccTerminalOrgID || currentAPIToken != ccTerminalAPIToken) {
			activityFn("info", "Credentials changed, restarting terminal server...")
			stopTerminalServer()
		}

		// Console permission gate (aceteam#6524): the control-center node stands up
		// its own terminal server with a VPN listener, so — exactly like `citadel
		// work` — starting it must be gated on the operator opting `console` in, not
		// just on being connected. A fresh node has `console` default-DENY, so no
		// terminal listener goes on the org mesh. When enabled, the passcode
		// verifier wired in startTerminalServer gates every connection.
		if config.LoadPermissions(platform.ConfigDir()).Console {
			if err := startTerminalServer(orgID, activityFn); err != nil {
				activityFn("warning", fmt.Sprintf("Terminal server failed: %v", err))
			} else {
				activityFn("info", fmt.Sprintf("Terminal server listening on port %d", ccTerminalPort))
			}
		} else {
			activityFn("info", "Console (terminal) disabled — enable the 'console' permission + set a node passcode to allow remote terminal access")
		}
	}

	perms := config.LoadPermissions(platform.ConfigDir())
	if perms.Desktop {
		if err := startVNCServer(); err != nil {
			activityFn("warning", fmt.Sprintf("VNC server failed: %v", err))
		} else {
			activityFn("info", fmt.Sprintf("VNC server listening on port %d", ccVNCPort))
		}

		// Start the H.264 desktop stream server alongside VNC when the node can
		// encode (ffmpeg + X). Clients that support H.264 use it; others fall
		// back to noVNC. A node without ffmpeg/X simply logs and stays on VNC
		// only (citadel-cli#338).
		if err := startH264Server(); err != nil {
			activityFn("info", fmt.Sprintf("H.264 stream unavailable (using VNC only): %v", err))
		} else {
			activityFn("info", fmt.Sprintf("H.264 stream server listening on port %d", ccH264Port))
		}
	}

	deviceConfig := getDeviceConfigFromFile()
	if deviceConfig != nil && deviceConfig.DeviceAPIToken != "" {
		time.Sleep(500 * time.Millisecond)
		activityFn("info", "Starting worker...")
		if err := ccStartWorker(activityFn); err != nil {
			activityFn("warning", fmt.Sprintf("Worker auto-start failed: %v", err))
		}
	}
}

// startTerminalServer starts the terminal server for remote SSH access.
// activityFn routes log messages through the TUI activity panel. It must
// not be nil — callers always pass one from ccOnNetworkConnect.
func startTerminalServer(orgID string, activityFn func(level, msg string)) error {
	if ccTerminalRunning.Load() {
		return nil // Already running
	}

	// Build configuration
	termCfg := terminal.DefaultConfig()
	termCfg.OrgID = orgID
	termCfg.AuthServiceURL = authServiceURL
	termCfg.Port = ccTerminalPort

	// Per-node passcode gate (aceteam#6524), same as the `citadel work` terminal
	// server: even after a valid token or same-owner mesh identity, a connection
	// must present the node passcode. Loaded fresh per connection so a rotated
	// passcode is honored without restarting the control center; fails closed.
	// (config is the internal/config package; the local terminal config is
	// termCfg to avoid shadowing it here.)
	termCfg.PasscodeVerifier = func(pin string) bool {
		return config.LoadPermissions(platform.ConfigDir()).VerifyPasscode(pin)
	}
	// Lets the reject response distinguish passcode_not_set from
	// passcode_invalid (citadel#753); see terminal.Config.PasscodeHasPasscode.
	termCfg.PasscodeHasPasscode = func() bool {
		return config.LoadPermissions(platform.ConfigDir()).HasPasscode()
	}

	// Create the caching token validator
	apiToken := ""
	if cfg := getDeviceConfigFromFile(); cfg != nil {
		apiToken = cfg.DeviceAPIToken
	}
	ccTerminalAuth = terminal.NewCachingTokenValidator(
		termCfg.AuthServiceURL,
		termCfg.OrgID,
		apiToken,
		termCfg.TokenRefreshInterval,
	)

	// Route validator log messages through the TUI activity log so token
	// fetch failures are visible (previously swallowed silently, making
	// 401 errors after re-pairing impossible to diagnose).
	ccTerminalAuth.SetLogFn(func(level, msg string) {
		activityFn(level, fmt.Sprintf("[terminal auth] %s", msg))
	})

	// Start the validator's background refresh
	if err := ccTerminalAuth.Start(); err != nil {
		return fmt.Errorf("failed to start token cache: %w", err)
	}

	// Create and start the server. Operate on a local until fully started, then
	// publish the pointer atomically so concurrent readers (the supervisor)
	// never observe a half-initialized server.
	srv := terminal.NewServer(termCfg, ccTerminalAuth)

	// Trust verified mesh peers on the VPN listener so `citadel connect <name>`
	// works with no platform-minted token (citadel #585). Additive: token path
	// preserved and takes precedence; localhost/LAN still requires a token.
	srv.SetMeshResolver(meshIdentityResolver{})

	// Suppress terminal server logging in TUI mode to prevent display corruption
	srv.SetSilent()

	if err := srv.Start(); err != nil {
		ccTerminalAuth.Stop()
		return fmt.Errorf("failed to start terminal server: %w", err)
	}
	ccTerminalServer.Store(srv)

	// Record which credentials the server was started with, so we can
	// detect when they change (e.g., after re-pairing) and restart.
	ccTerminalOrgID = orgID
	ccTerminalAPIToken = apiToken
	ccTerminalRunning.Store(true)

	// Attach the VPN listener so the terminal server is reachable over the
	// tsnet VPN. This is decoupled from the server lifecycle (issue #317): the
	// supervisor re-attaches it idempotently across tsnet reconnects.
	attachTerminalVPNListener()
	return nil
}

// stopTerminalServer stops the terminal server
func stopTerminalServer() {
	if !ccTerminalRunning.Load() {
		return
	}

	if ccTerminalAuth != nil {
		ccTerminalAuth.Stop()
	}

	if srv := ccTerminalServer.Load(); srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	}

	ccTerminalRunning.Store(false)
	ccTerminalOrgID = ""
	ccTerminalAPIToken = ""

	// Closing the server closes its listeners; reset the VPN-attach tracking
	// so a subsequent start re-attaches cleanly (issue #317).
	ccVPNMu.Lock()
	if ccTerminalVPNListener != nil {
		_ = ccTerminalVPNListener.Close()
	}
	ccTerminalVPNListener = nil
	ccTerminalVPNIP = ""
	ccVPNMu.Unlock()
}

// startVNCServer starts the embedded VNC server for remote desktop access.
// It retries transient failures (e.g. screen capture init racing with display
// server startup) up to 3 times with exponential backoff.
func startVNCServer() error {
	if ccVNCRunning.Load() {
		return nil
	}

	// Operate on a local until the server is fully started, then publish the
	// pointer atomically so the supervisor never observes a half-built server.
	srv := desktop.NewVNCServer(desktop.VNCServerConfig{
		Host: "127.0.0.1",
		Port: ccVNCPort,
		FPS:  10,
	})

	srv.SetSilent()

	// Retry with backoff for transient failures (display not ready yet).
	// Permanent failures (unsupported platform) bail immediately.
	const maxRetries = 3
	interval := 500 * time.Millisecond
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := srv.Start(); err != nil {
			lastErr = err
			// Permanent failure: platform doesn't support screen capture
			if strings.Contains(err.Error(), "not supported") {
				return fmt.Errorf("failed to start VNC server: %w", err)
			}
			if attempt < maxRetries {
				Log("VNC server start attempt %d/%d failed (retrying in %s): %v", attempt+1, maxRetries+1, interval, err)
				time.Sleep(interval)
				interval *= 2
				// Re-create server for retry since Start() sets running=false on failure
				srv = desktop.NewVNCServer(desktop.VNCServerConfig{
					Host: "127.0.0.1",
					Port: ccVNCPort,
					FPS:  10,
				})
				srv.SetSilent()
				continue
			}
			return fmt.Errorf("failed to start VNC server after %d attempts: %w", maxRetries+1, lastErr)
		}
		break
	}

	ccVNCServer.Store(srv)
	ccVNCRunning.Store(true)

	// Attach the VPN listener so the VNC server is reachable over the tsnet
	// VPN. The heartbeat's vnc_port is set from inside the attach helper so it
	// reflects actual VPN-listener reachability, not just localhost server
	// state (issue #317).
	attachVNCVPNListener()
	return nil
}

// stopVNCServer stops the embedded VNC server
func stopVNCServer() {
	if !ccVNCRunning.Load() {
		return
	}
	if srv := ccVNCServer.Load(); srv != nil {
		srv.Stop()
	}
	ccVNCRunning.Store(false)
	platform.ClearEmbeddedVNCPort()

	// Closing the server closes its listeners; reset the VPN-attach tracking
	// so a subsequent start re-attaches cleanly (issue #317).
	ccVPNMu.Lock()
	if ccVNCVPNListener != nil {
		_ = ccVNCVPNListener.Close()
	}
	ccVNCVPNListener = nil
	ccVNCVPNIP = ""
	ccVPNMu.Unlock()
}

// startH264Server starts the embedded H.264 desktop stream server for remote
// desktop video over the mesh (citadel-cli#338). It returns an error (and does
// not mark itself running) when the node cannot encode H.264 (no ffmpeg or X),
// so the caller leaves the node on VNC only and clients fall back to noVNC.
func startH264Server() error {
	if ccH264Running.Load() {
		return nil
	}

	srv := deskstream.NewServer(deskstream.Config{
		Host: "127.0.0.1",
		Port: ccH264Port,
		FPS:  15,
	})
	srv.SetSilent()

	if err := srv.Start(); err != nil {
		return fmt.Errorf("failed to start H.264 server: %w", err)
	}

	ccH264Server.Store(srv)
	ccH264Running.Store(true)

	// Attach the VPN listener so the stream is reachable over the tsnet mesh,
	// mirroring the VNC exposure (issue #317 self-healing semantics).
	attachH264VPNListener()
	return nil
}

// stopH264Server stops the embedded H.264 stream server.
func stopH264Server() {
	if !ccH264Running.Load() {
		return
	}
	if srv := ccH264Server.Load(); srv != nil {
		srv.Stop()
	}
	ccH264Running.Store(false)

	ccVPNMu.Lock()
	if ccH264VPNListener != nil {
		_ = ccH264VPNListener.Close()
	}
	ccH264VPNListener = nil
	ccH264VPNIP = ""
	ccVPNMu.Unlock()
}

// h264VPNHealthyLocked reports whether the H.264 VPN listener is currently
// attached and live. Caller must hold ccVPNMu.
func h264VPNHealthyLocked() bool {
	return ccH264VPNListener != nil && !ccH264VPNListener.isDead()
}

// attachH264VPNListener (re)binds the H.264 server's tsnet VPN listener
// idempotently, mirroring attachVNCVPNListener (issue #317). Safe to call
// repeatedly: it no-ops when a live listener is attached and replaces a dead
// one.
func attachH264VPNListener() {
	ccVPNMu.Lock()
	defer ccVPNMu.Unlock()

	srv := ccH264Server.Load()
	if !ccH264Running.Load() || srv == nil {
		return
	}

	if h264VPNHealthyLocked() {
		return
	}

	if !isConnectedFn() {
		return
	}

	if ccH264VPNListener != nil {
		_ = ccH264VPNListener.Close()
		srv.RemoveListener(ccH264VPNListener)
		ccH264VPNListener = nil
		ccH264VPNIP = ""
	}

	vpnPort := fmt.Sprintf("%d", ccH264Port)
	rawLn, vpnIP, err := listenVPNFn("tcp", vpnPort)
	if err != nil {
		Log("H.264 server VPN listener failed (localhost-only): %v", err)
		return
	}

	vpnLn := &vpnListener{Listener: rawLn}
	srv.AddListener(vpnLn)
	ccH264VPNListener = vpnLn
	ccH264VPNIP = vpnIP
	Log("H.264 server VPN listener on %s:%s", vpnIP, vpnPort)
}

// vncVPNHealthyLocked reports whether the VNC VPN listener is currently
// attached and live. Caller must hold ccVPNMu.
func vncVPNHealthyLocked() bool {
	return ccVNCVPNListener != nil && !ccVNCVPNListener.isDead()
}

// terminalVPNHealthyLocked reports whether the terminal VPN listener is
// currently attached and live. Caller must hold ccVPNMu.
func terminalVPNHealthyLocked() bool {
	return ccTerminalVPNListener != nil && !ccTerminalVPNListener.isDead()
}

// attachVNCVPNListener (re)binds the VNC server's tsnet VPN listener,
// idempotently. It is safe to call repeatedly: it no-ops when a live listener
// is already attached, and it tears down and replaces a dead one (e.g. a
// listener whose tsnet binding was torn down by a reconnect — including the
// same-IP blip where the node returns to the same tailnet address). See issue
// #317.
//
// The VNC server is only managed when desktop permission is enabled, so callers
// gate on perms.Desktop before starting the server; this helper assumes the
// server object exists (ccVNCServer.Load() != nil && ccVNCRunning.Load()).
func attachVNCVPNListener() {
	ccVPNMu.Lock()
	defer ccVPNMu.Unlock()

	// Load the server pointer once and use the local for every method call: a
	// concurrent stopVNCServer could otherwise swap it between the nil-check and
	// AddListener (issue #319 data race).
	srv := ccVNCServer.Load()
	if !ccVNCRunning.Load() || srv == nil {
		return
	}

	// A live listener is already attached — make sure the heartbeat reflects
	// reachability and return.
	if vncVPNHealthyLocked() {
		platform.SetEmbeddedVNCPort(ccVNCPort)
		return
	}

	// No live VPN listener: the desktop is unreachable over the tailnet, so the
	// heartbeat's vnc_port must report 0 (issue #317 requirement). Clear it now
	// even if we cannot rebind yet (e.g. still disconnected); a successful
	// re-attach below sets it back.
	platform.ClearEmbeddedVNCPort()

	if !isConnectedFn() {
		return
	}

	// Replace any dead listener before rebinding (don't leak it).
	if ccVNCVPNListener != nil {
		_ = ccVNCVPNListener.Close()
		srv.RemoveListener(ccVNCVPNListener)
		ccVNCVPNListener = nil
		ccVNCVPNIP = ""
	}

	vpnPort := fmt.Sprintf("%d", ccVNCPort)
	rawLn, vpnIP, err := listenVPNFn("tcp", vpnPort)
	if err != nil {
		Log("VNC server VPN listener failed (localhost-only): %v", err)
		platform.ClearEmbeddedVNCPort()
		return
	}

	vpnLn := &vpnListener{Listener: rawLn}
	srv.AddListener(vpnLn)
	ccVNCVPNListener = vpnLn
	ccVNCVPNIP = vpnIP
	platform.SetEmbeddedVNCPort(ccVNCPort)
	Log("VNC server VPN listener on %s:%s", vpnIP, vpnPort)
}

// attachTerminalVPNListener (re)binds the terminal server's tsnet VPN listener,
// idempotently. See attachVNCVPNListener for the contract. Issue #317.
func attachTerminalVPNListener() {
	ccVPNMu.Lock()
	defer ccVPNMu.Unlock()

	// Load the server pointer once; see attachVNCVPNListener for why (issue #319).
	srv := ccTerminalServer.Load()
	if !ccTerminalRunning.Load() || srv == nil {
		return
	}

	if terminalVPNHealthyLocked() {
		return
	}

	if !isConnectedFn() {
		return
	}

	if ccTerminalVPNListener != nil {
		_ = ccTerminalVPNListener.Close()
		srv.RemoveListener(ccTerminalVPNListener)
		ccTerminalVPNListener = nil
		ccTerminalVPNIP = ""
	}

	vpnPort := fmt.Sprintf("%d", ccTerminalPort)
	rawLn, vpnIP, err := listenVPNFn("tcp", vpnPort)
	if err != nil {
		Log("terminal server VPN listener failed (localhost-only): %v", err)
		return
	}

	vpnLn := &vpnListener{Listener: rawLn}
	srv.AddListener(vpnLn)
	ccTerminalVPNListener = vpnLn
	ccTerminalVPNIP = vpnIP
	Log("terminal server VPN listener on %s:%s", vpnIP, vpnPort)
}

// vpnListenersHealthy reports whether the VPN listeners that should be attached
// (given the running servers) are currently attached and live. The supervisor
// uses it to decide whether a re-attach is needed. A listener is unhealthy if
// it is missing or its accept loop saw the underlying tsnet listener get torn
// down (issue #317). VNC is only considered when its server is running (which
// itself is gated on desktop permission at start time).
func vpnListenersHealthy() bool {
	ccVPNMu.Lock()
	defer ccVPNMu.Unlock()

	if ccTerminalRunning.Load() && !terminalVPNHealthyLocked() {
		return false
	}
	if ccVNCRunning.Load() && !vncVPNHealthyLocked() {
		return false
	}
	if ccH264Running.Load() && !h264VPNHealthyLocked() {
		return false
	}
	return true
}

// ccSuperviseVPNListeners runs for the lifetime of the control center and
// self-heals the VNC + terminal VPN listeners across tsnet reconnects (issue
// #317).
//
// tsnet does not expose an explicit reconnect event: network.IsGlobalConnected
// polls the backend state, and on the "context deadline exceeded" blip the
// listener bound to the old tailnet IP is silently torn down while the server
// keeps running. So instead of hooking an event, this supervisor polls and
// reacts to the transition to connected, plus a steady-state health check that
// also covers the issue's "display==available && vnc_port==0" self-heal
// trigger (a VNC server that is running but has no reachable VPN listener).
//
// Re-attach is idempotent (see attach* helpers): a healthy listener is left
// alone, a dead one is replaced.
func ccSuperviseVPNListeners(ctx context.Context, activityFn func(level, msg string)) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	wasConnected := isConnectedFn()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		connected := isConnectedFn()

		// Nothing to manage while disconnected. Listeners bound to the old IP
		// are already dead; we re-attach on the transition back to connected.
		if !connected {
			wasConnected = false
			continue
		}

		// React to a transition into connected (startup reconnect or a
		// mid-session tsnet recovery), or to a steady-state where a running
		// server's VPN listener has gone unreachable (display available but
		// vnc_port effectively 0).
		if !wasConnected || !vpnListenersHealthy() {
			if ccTerminalRunning.Load() {
				attachTerminalVPNListener()
			}
			if ccVNCRunning.Load() {
				before := platform.EmbeddedVNCPort()
				attachVNCVPNListener()
				if activityFn != nil && before == 0 && platform.EmbeddedVNCPort() != 0 {
					activityFn("info", "Desktop VPN listener re-established after network reconnect")
				}
			}
			if ccH264Running.Load() {
				attachH264VPNListener()
			}
		}

		wasConnected = true
	}
}

// buildChatConfig constructs ChatPageConfig from device config and manifest.
// Returns a zero-value config if credentials are not yet available — the
// ChatPage handles this gracefully by showing an error on activation.
func buildChatConfig() controlcenter.ChatPageConfig {
	deviceConfig := getDeviceConfigFromFile()
	if deviceConfig == nil || deviceConfig.DeviceAPIToken == "" {
		return controlcenter.ChatPageConfig{}
	}

	apiBaseURL := deviceConfig.APIBaseURL
	if apiBaseURL == "" {
		apiBaseURL = authServiceURL
	}

	orgID := deviceConfig.OrgID
	if orgID == "" {
		if manifest, _, err := findAndReadManifest(); err == nil {
			orgID = manifest.Node.OrgID
		}
	}

	nodeName, _ := os.Hostname()
	nodeID := nodeName
	if netStatus, err := network.GetGlobalStatus(context.Background()); err == nil && netStatus.Connected {
		if netStatus.Hostname != "" {
			nodeName = netStatus.Hostname
		}
		if netStatus.NodeID != "" {
			nodeID = netStatus.NodeID
		}
	}

	return controlcenter.ChatPageConfig{
		APIBaseURL: apiBaseURL,
		APIToken:   deviceConfig.DeviceAPIToken,
		OrgID:      orgID,
		NodeID:     nodeID,
		NodeName:   nodeName,
	}
}

// buildTeamChatConfig constructs TeamChatPageConfig for the Team Chat page.
// The credential chain mirrors `citadel mcp`: ACETEAM_API_KEY env var, then
// the aceteam_api_key config.yaml field, then the device token. The device
// token is currently scope-denied on Team Chat routes (the page renders
// setup guidance in that case) — see aceteam-ai/citadel-cli#495.
func buildTeamChatConfig() controlcenter.TeamChatPageConfig {
	apiBaseURL := authServiceURL
	var configKey, deviceToken string
	if deviceConfig := getDeviceConfigFromFile(); deviceConfig != nil {
		if deviceConfig.APIBaseURL != "" {
			apiBaseURL = deviceConfig.APIBaseURL
		}
		configKey = deviceConfig.AceteamAPIKey
		deviceToken = deviceConfig.DeviceAPIToken
	}

	token, source := teamchat.ResolveToken(os.Getenv("ACETEAM_API_KEY"), configKey, deviceToken)
	return controlcenter.TeamChatPageConfig{
		APIBaseURL:  apiBaseURL,
		Token:       token,
		TokenSource: source,
	}
}

// getOrgIDFromConfig gets the organization ID from manifest or device config
func getOrgIDFromConfig() string {
	// Try manifest first
	if manifest, _, err := findAndReadManifest(); err == nil && manifest.Node.OrgID != "" {
		return manifest.Node.OrgID
	}
	// Try device config
	if deviceConfig := getDeviceConfigFromFile(); deviceConfig != nil && deviceConfig.OrgID != "" {
		return deviceConfig.OrgID
	}
	return ""
}

// gatherControlCenterData collects all data for the control center
func gatherControlCenterData() (controlcenter.StatusData, error) {
	data := controlcenter.StatusData{
		Version: Version,
	}

	// Load manifest
	manifest, configDir, _ := findAndReadManifest()
	if manifest != nil {
		data.NodeName = manifest.Node.Name
		data.OrgID = manifest.Node.OrgID
	}

	// Load user info from device config
	if deviceConfig := getDeviceConfigFromFile(); deviceConfig != nil {
		data.UserEmail = deviceConfig.UserEmail
		data.UserName = deviceConfig.UserName
		if data.OrgID == "" && deviceConfig.OrgID != "" {
			data.OrgID = deviceConfig.OrgID
		}
		if data.OrgName == "" && deviceConfig.OrgName != "" {
			data.OrgName = deviceConfig.OrgName
		}
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
	if percentages, err := cpu.Percent(200*time.Millisecond, false); err == nil && len(percentages) > 0 {
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
		if gpus, err := detector.GetGPUInfo(); err == nil && len(gpus) > 0 {
			gpu := gpus[0]
			data.GPUName = gpu.Name
			data.GPUMemory = gpu.Memory
			data.GPUTemp = gpu.Temperature
			if gpu.Utilization != "" {
				utilStr := strings.TrimSuffix(gpu.Utilization, "%")
				if util, err := strconv.ParseFloat(utilStr, 64); err == nil {
					data.GPUUtilization = util
				}
			}
		}
	}

	// Network status - check connection state without blocking
	if network.HasState() {
		// Check if already connected (don't try to reconnect, that blocks)
		if network.IsGlobalConnected() {
			data.Connected = true

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if status, err := network.GetGlobalStatus(ctx); err == nil {
				data.NodeIP = status.IPv4
				data.HeadscaleNodeID = status.NodeID
				if data.NodeName == "" {
					data.NodeName = status.Hostname
				}
			}
			cancel()

			// Get peers (with short timeout)
			ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
			myIP, _ := network.GetGlobalIPv4()
			if peers, err := network.GetGlobalPeers(ctx2); err == nil {
				for _, peer := range peers {
					if peer.IP != myIP {
						peerInfo := controlcenter.PeerInfo{
							Hostname: peer.Hostname,
							IP:       peer.IP,
							Online:   peer.Online,
						}

						// Skip ping for now to avoid blocking
						data.Peers = append(data.Peers, peerInfo)
					}
				}
			}
			cancel2()
		}
	}

	// Services
	if manifest != nil && configDir != "" {
		for _, service := range manifest.Services {
			svcInfo := controlcenter.ServiceInfo{
				Name:    service.Name,
				Status:  "stopped",
				Managed: true,
			}

			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			if _, err := os.Stat(fullComposePath); err == nil {
				if state, err := composeServiceState(fullComposePath, service.Name); err == nil {
					svcInfo.Status = state.State
					if state.Running && state.Container != nil && state.Container.Status != "" {
						svcInfo.Uptime = extractUptime(state.Container.Status)
					}
				}
			}

			data.Services = append(data.Services, svcInfo)
		}
	}

	// Surface inference engines running on this node that citadel did NOT
	// start (citadel #657) — e.g. ollama installed via its own `curl | sh`
	// script. Without this, an engine like that is invisible in the console
	// (which only walked citadel.yaml `services:` above) while the platform's
	// inference_nodes still lists the node as serving it, so the two screens
	// disagreed and the operator had no way to tell which was lying. Reuses
	// status.DiscoverLocalEngines — the SAME probe the heartbeat, `citadel
	// chat`, and the gateway chat router already use to decide what a node is
	// serving — so this pane agrees with the rest of the fleet instead of
	// running a second, possibly-divergent detection sweep.
	// 5s mirrors enrichServiceFootprints' budget below: DiscoverLocalEngines
	// probes each running managed engine sequentially at up to
	// status.ModelDiscoveryTimeout (2s) apiece, so this comfortably covers the
	// realistic case (1-2 engines running) without letting a wedged engine
	// stall this 15s-cadence refresh for the worst-case count.
	discCtx, discCancel := context.WithTimeout(context.Background(), 5*time.Second)
	discovered := status.DiscoverLocalEngines(discCtx)
	discCancel()
	data.Services = mergeDetectedEngines(data.Services, discovered)

	// Enrich running services with their live resource footprint (CPU/RAM/VRAM/
	// GPU) and a footprint-derived idle label, then roll up a one-line managed
	// summary (citadel #421). This is what makes the services panel resource-
	// aware: a heavy-and-idle container (the diffusers eviction candidate) is now
	// visible and highlighted instead of a bare "running".
	enrichServiceFootprints(&data)

	// Detect system tailscale (dual connection)
	running, ip, name, sameNetwork := controlcenter.DetectSystemTailscale(nexusURL)
	data.SystemTailscaleRunning = running
	data.SystemTailscaleIP = ip
	data.SystemTailscaleName = name
	data.DualConnection = running && sameNetwork && data.Connected

	// Worker status
	ccWorkerMu.Lock()
	data.WorkerRunning = ccWorkerRunning
	data.WorkerQueue = ccWorkerQueue
	ccWorkerMu.Unlock()

	// Demo server URL (always running when control center is active)
	data.DemoServerURL = fmt.Sprintf("http://localhost:%d", ccDemoPort)

	// Terminal server URL (only shown when running and connected)
	if ccTerminalRunning.Load() {
		data.TerminalServerURL = fmt.Sprintf("ws://localhost:%d/terminal", ccTerminalPort)
	}

	return data, nil
}

// mergeDetectedEngines combines the manifest-derived managed service rows
// with engines status.DiscoverLocalEngines found running on this node
// (citadel #657). A discovered engine already represented by a managed row —
// matched by name, case-insensitively — is never double-listed;
// managedProbeEngines' names (vllm/ollama/llamacpp/bonsai/unlimited-ocr) are
// exactly the citadel.yaml service names for the embedded engines, so name is
// a reliable de-dupe key. Every remaining discovered engine is appended as an
// unmanaged row (Managed=false, Status="running") carrying the model(s) it is
// currently serving, so the console shows it distinctly instead of it being
// invisible.
//
// Name-matched engines split on whether the MANAGED row is actually running
// (citadel #804): DiscoverLocalEngines' liveness check
// (status.enginePortIfRunning) falls back to nativesvc.IsNativeServiceServing
// — a raw "does something answer this engine's port" probe — independent of
// composeServiceState (docker compose ps), which is the ONLY thing that sets a
// managed row's Status above. So a managed row can read "stopped" while a live
// signal for the same name exists (e.g. citadel.yaml declares "ollama" but its
// compose stack isn't running, while a separately-started native ollama
// answers on the same port). Dropping that live signal because the name
// already "exists" silently hid a serving engine. When the managed row is NOT
// running, the discovered signal UPGRADES it in place instead: Status flips to
// "running", Models is attached, and Managed flips to false so the console
// renders it as a detected/native instance rather than implying citadel's own
// compose stack is up. When the managed row IS running, it wins unchanged —
// composeServiceState is already authoritative for that case.
//
// No I/O, so the merge/de-dupe/upgrade decision is unit-testable without
// docker, a live engine, or a manifest file. NOT allocation-pure, though: an
// upgrade mutates the matched element of `managed` in place (out shares
// managed's backing array until an append grows it), so the caller's slice
// header can be reassigned from the return value but its already-existing
// elements may already reflect the upgrade too. The single call site
// (`data.Services = mergeDetectedEngines(data.Services, ...)`) relies on
// exactly this, not a defensive copy.
func mergeDetectedEngines(managed []controlcenter.ServiceInfo, discovered []status.LocalEngine) []controlcenter.ServiceInfo {
	indexByName := make(map[string]int, len(managed))
	for i, svc := range managed {
		indexByName[strings.ToLower(svc.Name)] = i
	}

	out := managed
	for _, eng := range discovered {
		key := strings.ToLower(eng.Name)
		if idx, ok := indexByName[key]; ok {
			if out[idx].Status == "running" {
				// Managed compose is already running — it wins, no change.
				continue
			}
			// Live signal for a declared-but-not-compose-running engine:
			// upgrade the row instead of silently dropping it.
			out[idx].Status = "running"
			out[idx].Managed = false
			out[idx].Models = eng.Models
			continue
		}
		out = append(out, controlcenter.ServiceInfo{
			Name:    eng.Name,
			Status:  "running",
			Managed: false,
			Models:  eng.Models,
		})
		indexByName[key] = len(out) - 1
	}
	return out
}

// ccFootprintIdleTracker is the long-lived footprint-derived idle tracker for
// the Control Center refresh path (citadel #421). It MUST be a package-level
// singleton: idle duration accumulates across refreshes from a container's
// first-inactive timestamp, so a fresh tracker per refresh would never cross the
// idle threshold and the heavy-and-idle warning would never fire.
var ccFootprintIdleTracker = status.NewFootprintIdleTracker()

// enrichServiceFootprints attaches a live resource footprint + idle label to
// each running service in data, and computes the one-line managed summary. It
// makes a single batched footprint collection for all running services (one
// stats call + one nvidia-smi pair), then formats per-service.
func enrichServiceFootprints(data *controlcenter.StatusData) {
	// Collect candidate container names for running services only. Note this
	// is NOT filtered to Managed rows — a detected/unmanaged row (citadel
	// #657) still gets a per-row footprint; applyServiceFootprints is what
	// keeps it out of the managed TOTAL (citadel #805).
	var names []string
	nameToIdx := map[string]int{}
	for i := range data.Services {
		if data.Services[i].Status != "running" {
			continue
		}
		for _, cn := range []string{"citadel-" + data.Services[i].Name, data.Services[i].Name} {
			if _, dup := nameToIdx[cn]; dup {
				continue
			}
			nameToIdx[cn] = i
			names = append(names, cn)
		}
	}
	if len(names) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	footprints := status.CollectFootprints(ctx, names)

	applyServiceFootprints(data, nameToIdx, footprints, ccFootprintIdleTracker)
}

// applyServiceFootprints attaches a footprint/idle label to every running row
// in data.Services that a pre-collected footprints map resolved (keyed by
// candidate container name, via nameToIdx), and rolls up the one-line
// "managed: RAM .../VRAM ..." summary.
//
// Unmanaged rows (Managed=false — a detected engine citadel did not start,
// citadel #657) still get their own per-row Footprint/IdleLabel/HeavyAndIdle,
// but their RAM/VRAM is deliberately excluded from the summed total (citadel
// #805): that summary is read as "what citadel itself is using", and an
// engine running under its own bare container name (e.g. an official Ollama
// install answering on the port citadel.yaml also declares) is not citadel's
// usage to report.
//
// Split out from enrichServiceFootprints so the roll-up/exclusion logic is
// unit-testable against a fixed footprints map, without docker or nvidia-smi.
func applyServiceFootprints(
	data *controlcenter.StatusData,
	nameToIdx map[string]int,
	footprints map[string]status.ServiceFootprint,
	idleTracker *status.FootprintIdleTracker,
) {
	var totalRAM, totalVRAM uint64
	var anyGPU bool
	for cn, fp := range footprints {
		idx, ok := nameToIdx[cn]
		if !ok {
			continue
		}
		// Skip a candidate name that resolved to no container (bare name when
		// the "citadel-" variant is the real one).
		if fp.CPUPercent < 0 && fp.RAMBytes == 0 && fp.VRAMBytes == 0 {
			continue
		}
		fpCopy := fp
		idle := idleTracker.Observe(cn, &fpCopy)
		data.Services[idx].Footprint = status.FormatFootprint(&fpCopy)
		data.Services[idx].IdleLabel = status.FormatIdleLabel(&idle)
		data.Services[idx].HeavyAndIdle = status.IsHeavyAndIdle(&fpCopy, &idle)

		if !data.Services[idx].Managed {
			continue
		}
		totalRAM += fpCopy.RAMBytes
		totalVRAM += fpCopy.VRAMBytes
		if fpCopy.HasGPU {
			anyGPU = true
		}
	}

	// One-line node roll-up: "managed: RAM 13G/62G · VRAM 21G/24G".
	summary := "managed: RAM " + status.FormatBytesGB(totalRAM)
	if data.MemoryTotal != "" {
		summary += "/" + data.MemoryTotal
	}
	if anyGPU {
		summary += " · VRAM " + status.FormatBytesGB(totalVRAM)
		if data.GPUMemory != "" {
			summary += "/" + data.GPUMemory
		}
	}
	data.ManagedSummary = summary
}

// extractUptime tries to extract uptime from docker status string like "Up 2 hours"
func extractUptime(status string) string {
	status = strings.ToLower(status)
	if uptime, found := strings.CutPrefix(status, "up "); found {
		return uptime
	}
	return ""
}

// ccStartService starts a service by name
func ccStartService(name string) error {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		return err
	}

	for _, service := range manifest.Services {
		if service.Name == name {
			// Preflight (citadel #767): this is the literal repro from the
			// issue (TUI "Starting ollama..." -> "Failed to start ollama:
			// exec: \"docker\": executable file not found in $PATH") --
			// composeCommand below execs the resolved engine binary directly
			// with no prior check. Refuse ONLY when the CLI is missing (that
			// exec would fail immediately anyway); a daemon that failed to
			// answer the preflight's probe is logged as a warning and the
			// start proceeds, since it may simply be slow and the compose-up
			// call below already surfaces docker's own error if it truly is
			// unreachable -- see platform.PreflightDockerStart. ccStopService
			// deliberately carries no equivalent check: a stop that can't
			// reach docker is already a no-op (nothing to stop), not the
			// silent "start reported nothing wrong" failure mode this issue
			// is about. Resolved once and reused below (composeCommandFor)
			// so this doesn't re-probe the runtime a second time.
			rt := catalog.SelectContainerRuntime()
			if refuseErr, warning := platform.PreflightDockerStart(rt.EngineBin); refuseErr != nil {
				return fmt.Errorf("cannot start %s: %s", name, refuseErr)
			} else if warning != "" {
				clilog.Writef("warning", "docker preflight for %s: %s", name, warning)
			}
			// Clear the durable stopped marker FIRST (mirrors liveModuleOps.Start):
			// an explicit start restores start-on-boot even if the compose up below
			// fails transiently. Best-effort: the service is in the manifest (we
			// just found it), so a failure here is unexpected but must not block
			// the start.
			_ = setServiceDesiredStatus(configDir, name, "")
			// Transitional (#528): drop any container still under the legacy
			// "citadel-<name>" project so the no-`-p` up below does not conflict
			// on the pinned container_name.
			removeLegacyCitadelProject(name)
			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			// Include the least-privilege sandbox override when present so a
			// TUI-installed untrusted module also starts hardened here (the
			// override would otherwise be bypassed by this start site).
			// No -p: the default compose project (dir basename, "services") is the
			// standardized convention shared with the boot/run/stop/job paths and
			// the no-`-p` status reads (#528).
			composeArgs := composeFileArgs(fullComposePath, fullComposePath)
			composeArgs = append(composeArgs, "up", "-d")
			// composeCommand injects the citadel-owned host ports so the
			// ${CITADEL_*_HOST_PORT:?...} guard resolves; without this the TUI
			// start button dies for llamacpp/vllm/extraction/diffusers on
			// v2.57.0 (#426).
			return composeCommandFor(rt, composeArgs...).Run()
		}
	}

	return fmt.Errorf("service not found: %s", name)
}

// ccStopService stops a service by name
func ccStopService(name string) error {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		return err
	}

	for _, service := range manifest.Services {
		if service.Name == name {
			// Mark durably stopped FIRST (mirrors liveModuleOps.Stop): even if the
			// compose down below is interrupted, a worker restart / reboot will not
			// resurrect the service (cmd/work.go and cmd/run.go skip services with
			// desired_status: stopped). This is also what makes the TUI stop
			// survive `citadel work` restarting managed services (#528).
			if err := setServiceDesiredStatus(configDir, name, "stopped"); err != nil {
				return err
			}
			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			// No -p: production containers run under the default compose project
			// (dir basename, "services"). The old `-p citadel-<name>` down matched
			// zero containers and exited 0 -- the silent no-op of #528.
			downArgs := append(composeFileArgs(fullComposePath, fullComposePath), "down")
			cmd := composeCommand(downArgs...)
			runErr := cmd.Run()
			// Transitional (#528): also remove containers a pre-fix start left
			// under the legacy "citadel-<name>" project, which the no-`-p` down
			// above cannot see.
			removeLegacyCitadelProject(name)
			return runErr
		}
	}

	return fmt.Errorf("service not found: %s", name)
}

// ccRestartService restarts a service by name
func ccRestartService(name string) error {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		return err
	}

	for _, service := range manifest.Services {
		if service.Name == name {
			// Preflight (citadel #767): same warn-and-proceed rationale as
			// ccStartService above.
			rt := catalog.SelectContainerRuntime()
			if refuseErr, warning := platform.PreflightDockerStart(rt.EngineBin); refuseErr != nil {
				return fmt.Errorf("cannot restart %s: %s", name, refuseErr)
			} else if warning != "" {
				clilog.Writef("warning", "docker preflight for %s: %s", name, warning)
			}
			// A restart expresses "keep this running": clear any durable stopped
			// marker so the service also starts on the next boot. Best-effort.
			_ = setServiceDesiredStatus(configDir, name, "")
			// Transitional (#528): a container still under the legacy
			// "citadel-<name>" project is invisible to a no-`-p` `compose restart`
			// (silent no-op) AND conflicts with a no-`-p` up on the pinned
			// container_name. Remove it, then converge with `up -d
			// --force-recreate`, which restarts a default-project container and
			// (re)creates one when the legacy container was just removed --
			// `restart` alone would no-op in that case.
			removeLegacyCitadelProject(name)
			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			restartArgs := append(composeFileArgs(fullComposePath, fullComposePath), "up", "-d", "--force-recreate")
			cmd := composeCommandFor(rt, restartArgs...)
			return cmd.Run()
		}
	}

	return fmt.Errorf("service not found: %s", name)
}

// ccGetServiceDetail returns detailed information about a service
func ccGetServiceDetail(name string) *controlcenter.ServiceDetailInfo {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		return nil
	}

	for _, service := range manifest.Services {
		if service.Name == name {
			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			detail := &controlcenter.ServiceDetailInfo{
				ComposePath: service.ComposeFile,
			}

			// Get container info via docker compose ps (no -p: default project,
			// matching where production containers actually run, #528). The output
			// is project-wide, so it must be narrowed to this compose file's own
			// services before reading a container off it. Decoding the first
			// record showed another service's container here (#692).
			if state, err := composeServiceState(fullComposePath, service.Name); err == nil && state.Container != nil {
				container := state.Container
				if len(container.ID) > 12 {
					detail.ContainerID = container.ID[:12]
				} else {
					detail.ContainerID = container.ID
				}
				detail.Image = container.Image
				if container.Ports != "" {
					// Parse ports string like "0.0.0.0:8000->8000/tcp"
					detail.Ports = strings.Split(container.Ports, ", ")
				}
			}

			return detail
		}
	}

	return nil
}

// ccGetServiceLogs returns recent log lines for a service
func ccGetServiceLogs(name string) ([]string, error) {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		return nil, err
	}

	for _, service := range manifest.Services {
		if service.Name == name {
			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			// No -p: read logs from the default compose project, where production
			// containers actually run (#528).
			logArgs := append(composeFileArgs(fullComposePath, fullComposePath), "logs", "--tail", "50", "--no-color")
			cmd := composeCommand(logArgs...)
			output, err := cmd.Output()
			if err != nil {
				return nil, err
			}
			lines := strings.Split(string(output), "\n")
			// Filter out empty lines
			var result []string
			for _, line := range lines {
				if strings.TrimSpace(line) != "" {
					result = append(result, line)
				}
			}
			return result, nil
		}
	}

	return nil, fmt.Errorf("service not found: %s", name)
}

// ccAddService adds a new service to the manifest and extracts its compose file
func ccAddService(name string) error {
	// Find or create config directory
	_, configDir, err := findOrCreateManifest()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}

	// Ensure compose file exists
	if err := ensureComposeFile(configDir, name); err != nil {
		return err
	}

	// Add to manifest
	if err := addServiceToManifest(configDir, name); err != nil {
		return err
	}

	return nil
}

// ccGetConfiguredServices returns the list of services already configured in the manifest
func ccGetConfiguredServices() []string {
	manifest, _, err := findAndReadManifest()
	if err != nil {
		return nil
	}

	var configured []string
	for _, svc := range manifest.Services {
		configured = append(configured, svc.Name)
	}
	return configured
}

// ccStartDeviceAuthFlow starts the device authorization flow and returns the config
func ccStartDeviceAuthFlow() (*controlcenter.DeviceAuthConfig, error) {
	client := nexus.NewDeviceAuthClient(authServiceURL)

	resp, err := client.StartFlow(nil)
	if err != nil {
		return nil, err
	}

	return &controlcenter.DeviceAuthConfig{
		UserCode:        resp.UserCode,
		VerificationURI: resp.VerificationURI,
		DeviceCode:      resp.DeviceCode,
		ExpiresIn:       resp.ExpiresIn,
		Interval:        resp.Interval,
	}, nil
}

// ccPollDeviceAuthToken polls for device authorization token (single check)
func ccPollDeviceAuthToken(deviceCode string, interval int) (string, error) {
	client := nexus.NewDeviceAuthClient(authServiceURL)

	// Use checkToken for single poll (not blocking PollForToken)
	token, err := client.CheckToken(deviceCode)
	if err != nil {
		return "", err
	}
	if token == nil || token.Authkey == "" {
		return "", fmt.Errorf("authorization_pending")
	}

	// Save the device config including user info when auth succeeds
	if token.DeviceAPIToken != "" || token.UserEmail != "" {
		if err := saveDeviceConfigToFile(token); err != nil {
			// Log but don't fail - we have the authkey
			Debug("failed to save device config: %v", err)
		}
	}

	return token.Authkey, nil
}

// ccConnectWithAuthkey connects to the network using an authkey
func ccConnectWithAuthkey(authkey string) error {
	// Get node name from manifest or hostname
	nodeName := ""
	if manifest, _, err := findAndReadManifest(); err == nil && manifest != nil {
		nodeName = manifest.Node.Name
	}
	if nodeName == "" {
		nodeName, _ = os.Hostname()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	config := network.ServerConfig{
		Hostname:   nodeName,
		ControlURL: nexusURL,
		AuthKey:    authkey,
	}

	_, err := network.Connect(ctx, config)
	return err
}

// ccDisconnect disconnects from the network
func ccDisconnect() error {
	return network.Logout()
}

// ccWorkerIsRunning returns true if the worker is running
func ccWorkerIsRunning() bool {
	ccWorkerMu.Lock()
	defer ccWorkerMu.Unlock()
	return ccWorkerRunning
}

// ccWorkerOwnsConsumption backs controlcenter.WorkerCallbacks.OwnsConsumption
// (issue #658): true only while this process's own runTUIWorker call is the
// node's job consumer, false in monitor-only mode (a dedicated `citadel work`
// holds the worklock) or before/after the worker's consume loop runs.
func ccWorkerOwnsConsumption() bool {
	ccWorkerMu.Lock()
	defer ccWorkerMu.Unlock()
	return ccWorkerConsuming
}

// ccStartWorker starts the worker in the background
func ccStartWorker(activityFn func(level, msg string)) error {
	ccWorkerMu.Lock()
	if ccWorkerRunning {
		ccWorkerMu.Unlock()
		return fmt.Errorf("worker is already running")
	}
	ccActivityFn = activityFn
	ccWorkerMu.Unlock()

	// Create a cancellable context for the worker
	ctx, cancel := context.WithCancel(context.Background())

	ccWorkerMu.Lock()
	ccWorkerCancel = cancel
	ccWorkerMu.Unlock()

	// Start worker in goroutine
	go func() {
		defer func() {
			ccWorkerMu.Lock()
			ccWorkerRunning = false
			ccWorkerConsuming = false
			ccWorkerQueue = ""
			ccWorkerMu.Unlock()
		}()

		if err := runTUIWorker(ctx, activityFn); err != nil {
			if err != context.Canceled && activityFn != nil {
				activityFn("error", fmt.Sprintf("Worker error: %v", err))
			}
		}
	}()

	ccWorkerMu.Lock()
	ccWorkerRunning = true
	ccWorkerMu.Unlock()

	return nil
}

// ccStopWorker stops the running worker
func ccStopWorker() error {
	ccWorkerMu.Lock()
	defer ccWorkerMu.Unlock()

	if !ccWorkerRunning {
		return fmt.Errorf("worker is not running")
	}

	if ccWorkerCancel != nil {
		ccWorkerCancel()
		ccWorkerCancel = nil
	}

	return nil
}

// runTUIWorker runs the worker for the TUI (simplified version of runWork)
func runTUIWorker(ctx context.Context, activityFn func(level, msg string)) error {
	activity := func(level, msg string) {
		if activityFn != nil {
			activityFn(level, msg)
		}
	}

	// citadel#853/#856: mirrors runWork's boot-time --node-dir refusal
	// (cmd/work.go). This is the control center's OTHER path into
	// buildNodeJobHandlers (below), which registers the SAME
	// jobs.NewConfigHandler("") (internal/worker/handler_adapter.go) --
	// hardcoded to "", NOT threaded from nodeJobHandlerOpts.ConfigDir, so it
	// is NOT --node-dir-aware regardless of which entry point constructed it.
	// Without this check here too, a bare `citadel` (TUI, the default when no
	// dedicated worker holds the lock) run under an active
	// --node-dir/CITADEL_NODE_DIR would silently apply an APPLY_DEVICE_CONFIG
	// job against this machine's REAL ~/citadel-node instead of the
	// override -- exactly the split runWork's refusal exists to prevent, via
	// a different entry point that reaches the same unguarded handler.
	if override := resolveNodeDirOverride(); override != "" {
		return fmt.Errorf(
			"--node-dir/CITADEL_NODE_DIR (%q) is not supported by the control center's worker mode: like "+
				"'citadel work', it wires jobs.NewConfigHandler(\"\") (internal/worker/handler_adapter.go), "+
				"which is not --node-dir-aware and would resolve APPLY_DEVICE_CONFIG jobs against this "+
				"machine's real config directory regardless of the override. Unset the override to use the "+
				"control center, or use 'citadel module stop|start|restart' for targeted node operations.",
			override)
	}

	// Load device config from file
	deviceConfig := getDeviceConfigFromFile()

	// Detect a dedicated `citadel work` worker already serving this node. If one
	// holds the single-instance lock (issues #443/#435/#455), the control center
	// MUST NOT compete for this node's jobs: two consumers in the same consumer
	// group split the per-node stream non-deterministically, so node-targeted
	// privileged jobs (WHATSAPP_PROVISION, AGENT_UPDATE) were randomly grabbed by
	// the control-center worker and failed with "no handler" even though the real
	// worker's binary handles them (the competing-consumer incident). When a worker
	// is present the control center stays a read-only monitor (heartbeat/telemetry
	// only) and lets the real worker own all job consumption.
	//
	// Detection is a one-shot at TUI-worker startup: the systemd worker is normally
	// already running before the TUI opens. If a worker starts or stops later the
	// mode is not re-evaluated until the TUI worker restarts, but that residual is
	// benign — the "no handler" hazard is removed unconditionally by the shared
	// handler set below (both modes register WHATSAPP_PROVISION / AGENT_UPDATE), so a
	// transient double-consumer only reproduces the pre-existing split, never a job
	// failure. A worker that later dies is systemd-restarted (re-taking the lock).
	workerHeld, workerPID := worklock.IsHeld(network.GetStateDir())
	if workerHeld {
		activity("info", fmt.Sprintf("Dedicated worker detected (PID %d); control center runs in monitor-only mode (no job consumption)", workerPID))
	}
	// Feeds ccWorkerOwnsConsumption / controlcenter.WorkerCallbacks.OwnsConsumption
	// (issue #658): this process only ever reaches its own consume loop below
	// when !workerHeld, so that's also the right moment to record it as the
	// node's job-consuming worker for the detach-on-quit decision.
	ccWorkerMu.Lock()
	ccWorkerConsuming = !workerHeld
	ccWorkerMu.Unlock()

	// Determine job source mode
	var source worker.JobSource
	var streamFactory func(job *worker.Job) worker.StreamWriter

	// inferenceQueueReconciler mirrors cmd/work.go's #612/#824 wiring
	// (citadel-cli #823): watches for a serving-engine transition and
	// self-subscribes the inference queue the moment one appears, so a node
	// run via the Control Center does not need a restart to serve inference
	// started after boot. Registered as an OnStatus observer once the
	// heartbeat publisher exists, below. Stays nil unless the boot-time queue
	// set actually has a gap for it to fill (see missingQueues below) or a
	// dedicated `citadel work` holds this node's lock (workerHeld) -- in that
	// case this TUI instance never runs a consume loop at all (see the
	// workerHeld early-return further down), so subscribing here would be
	// pure waste, not a fix.
	var inferenceQueueReconciler *worker.InferenceQueueReconciler

	if deviceConfig != nil && deviceConfig.DeviceAPIToken != "" {
		// API mode
		apiBaseURL := deviceConfig.APIBaseURL
		if apiBaseURL == "" {
			apiBaseURL = authServiceURL
		}

		// Boot-time queue resolution (cpu-general base + FetchWorkerConfig
		// workQueue + org shellQueue + inference queues), shared with runWork
		// (cmd/work.go) via resolveWorkerQueues (citadel-cli#839) so the two
		// entry points cannot drift apart on which queues an equivalent node
		// ends up consuming. Before this fix the TUI worker path built its own
		// inline set (resolveControlCenterInferenceQueues) that covered the
		// cpu-general base and inference queues (#612/#823/#837) but never
		// fetched the platform-assigned workQueue or joined the org shell
		// queue, so a TUI-run node silently missed jobs dispatched to either.
		//
		// ccNodeCaps/ccServing are only computed when !workerHeld (skip the
		// manifest read / docker ps when the resolver will Skip anyway --
		// resolveWorkerQueues never touches them in that case, see its doc
		// comment for why: a dedicated `citadel work` already owns this
		// node's consumption and the Control Center must not compete for it).
		var ccNodeCaps *capabilities.NodeCapabilities
		ccServing := false
		if !workerHeld {
			if manifest, _, mErr := findAndReadManifest(); mErr == nil && manifest != nil && manifest.Capabilities != nil {
				ccNodeCaps = manifestToNodeCapabilities(manifest.Capabilities)
			} else {
				ccNodeCaps = capabilities.DetectNodeCapabilities()
			}
			ccServing = nodeIsServingModels(ctx)
		}
		// Deliberately does NOT write the resolved OrgID back onto
		// deviceConfig.OrgID: unlike runWork, the pre-#839 Control Center
		// path never called FetchWorkerConfig at all, so downstream org
		// resolution here (perNodeOrgID below, the heartbeat orgID further
		// down) still falls back to manifest.Node.OrgID exactly as before.
		// Propagating the fetched org would let it silently outrank that
		// existing fallback -- a behavior change outside #839's scope
		// (queue-set parity only). See resolveWorkerQueues' doc comment.
		ccQueueResult := resolveWorkerQueues(ctx, workerQueueParams{
			APIBaseURL: apiBaseURL,
			Token:      deviceConfig.DeviceAPIToken,
			OrgID:      deviceConfig.OrgID,
			NodeCaps:   ccNodeCaps,
			Serving:    ccServing,
			WorkerHeld: workerHeld,
			DebugFn:    func(format string, args ...any) { activity("info", fmt.Sprintf(format, args...)) },
		})
		ccQueueNames, ccMissingQueues := ccQueueResult.Queues, ccQueueResult.Missing

		apiSource := worker.NewAPISource(worker.APISourceConfig{
			BaseURL:       apiBaseURL,
			Token:         deviceConfig.DeviceAPIToken,
			QueueNames:    ccQueueNames,
			ConsumerGroup: "",
			BlockMs:       5000,
			MaxAttempts:   3,
			LogFn:         activity, // Route logs through TUI
		})

		if err := apiSource.Connect(ctx); err != nil {
			return fmt.Errorf("failed to connect to Redis API: %w", err)
		}

		if len(ccMissingQueues) > 0 {
			inferenceQueueReconciler = worker.NewInferenceQueueReconciler(
				ccMissingQueues,
				nodeIsServingModels,
				func(_ context.Context, queue string) error {
					apiSource.AddQueue(queue) // APISource creates its consumer group lazily; no error to surface
					return nil
				},
			)
		}

		// Enable WebSocket for real-time pub/sub. Same defect and same fix as
		// the worker path (issue #723): one failed connect used to disable
		// real-time pub/sub for the life of the process. Retries in the
		// background with backoff/jitter, honoring any 429 retry_after.
		enableWebSocketWithRetry(ctx, wsConnector{client: apiSource.Client()},
			func(level, format string, args ...any) {
				activity(level, fmt.Sprintf(format, args...))
			})

		source = apiSource
		streamFactory = worker.CreateAPIStreamWriterFactory(ctx, apiSource)

		activity("info", "Worker mode: API (secure)")

		ccWorkerMu.Lock()
		ccWorkerQueue = "api"
		ccWorkerMu.Unlock()
	} else {
		// No device_api_token configured - worker can't start
		// Note: Direct Redis mode is only available via 'citadel work' command
		return fmt.Errorf("no API token configured (complete device authorization first)")
	}

	// Get node name and Headscale node ID
	nodeName := ""
	headscaleNodeID := ""
	if netStatus, err := network.GetGlobalStatus(ctx); err == nil && netStatus.Connected && netStatus.Hostname != "" {
		nodeName = netStatus.Hostname
		headscaleNodeID = netStatus.NodeID
	} else {
		nodeName, _ = os.Hostname()
	}

	// Subscribe to this node's per-node shell stream so node-targeted MCP jobs
	// (terminal_exec, code_*, file reads, node attachments) reach ONLY this node
	// instead of being claimed by a greedy peer on the shared shell stream
	// (issue #3914). The canonical `citadel` (TUI) path must do this too, not
	// just `citadel work` -- otherwise node-targeted jobs are never consumed in
	// the normal run mode. Mirrors the subscription in runWork(); purely additive
	// (the shared shell stream is still consumed for untargeted work).
	if headscaleNodeID == "" {
		if id := network.GetGlobalNodeID(ctx); id != "" {
			headscaleNodeID = id
		}
	}
	// Only subscribe to the per-node stream when NO dedicated worker holds the lock.
	// If a worker is present it owns the per-node stream; a second subscriber here
	// would re-introduce the competing-consumer split.
	if workerHeld {
		activity("info", "Per-node stream owned by the dedicated worker; control center does not subscribe")
	} else if headscaleNodeID != "" {
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
			if apiSrc, ok := source.(*worker.APISource); ok {
				apiSrc.AddQueue(perNodeQueue)
			}
			activity("info", fmt.Sprintf("Per-node shell stream: %s", perNodeQueue))
		} else {
			activity("warning", "Per-node shell stream skipped (org id unknown); node-targeted jobs fall back to the shared stream")
		}
	} else {
		activity("warning", "Per-node shell stream skipped (node ID unavailable); node-targeted jobs fall back to the shared stream")
	}

	// Create status collector for heartbeat
	collector := status.NewCollector(status.CollectorConfig{
		NodeName:  nodeName,
		ConfigDir: "",
		Services:  nil,
	})

	// Start heartbeat publisher if we have API mode
	if deviceConfig != nil && deviceConfig.DeviceAPIToken != "" {
		orgID := ""
		if deviceConfig.OrgID != "" {
			orgID = deviceConfig.OrgID
		} else if manifest, _, err := findAndReadManifest(); err == nil {
			orgID = manifest.Node.OrgID
		}

		if orgID != "" {
			if apiSource, ok := source.(*worker.APISource); ok {
				// Wire anonymous activity telemetry through the same
				// authenticated Redis API client the heartbeat uses. Gated
				// at emit time by the anon_telemetry_enabled flag; payloads
				// carry only node/debug context, never user PII.
				telemetry.Configure(
					apiSource.Client(),
					platform.ConfigDir(),
					nodeName,
					headscaleNodeID,
					orgID,
					Version,
				)

				// Periodically report the node's ActualState (installed-module
				// set + per-module health) to the control plane (#353,
				// report-only v1). Same device-authed client, same opt-out gate
				// as activity telemetry; node_id is the Headscale hostname so
				// the server can re-derive org and ignore any payload org claim.
				if emitter := nodestate.New(nodestate.Config{
					Poster:    apiSource.Client(),
					Inspector: nodestate.DockerInspector(),
					ConfigDir: platform.ConfigDir(),
					NodeID:    nodeName,
					Version:   Version,
				}); emitter != nil {
					go emitter.Run(ctx)
					activity("info", "Node-state reporting started")
				}

				// Only the control center publishes heartbeat when NO dedicated
				// worker owns this node. A `citadel work` daemon holding the lock
				// already publishes heartbeat for this nodeId (see runWork); a
				// second publisher here would double the control-plane heartbeat
				// volume (the 429 self-DoS class the worklock work targeted) and
				// flap node status between two competing publishers. The monitor-only
				// gate below stops job consumption but not heartbeat publishing, so
				// gating the publisher start here is what closes #571 (Phase 0).
				// When skipped, the heartbeat indicator (ccHeartbeatFn) is left unset
				// so the TUI never falsely claims "Heartbeat publishing started".
				if workerHeld {
					activity("info", fmt.Sprintf("Dedicated worker holds this node (PID %d); control center will NOT publish a second heartbeat (monitor-only)", workerPID))
				} else if apiPublisher, err := heartbeat.NewAPIPublisher(heartbeat.APIPublisherConfig{
					Client:          apiSource.Client(),
					NodeID:          nodeName,
					HeadscaleNodeID: headscaleNodeID,
					OrgID:           orgID,
					DebugFunc:       nil,
					LogFn:           activity, // Route logs through TUI
				}, collector); err == nil {
					// Include current permissions in heartbeat
					apiPublisher.SetPermissionsProvider(currentPermissionsForHeartbeat)

					if inferenceQueueReconciler != nil {
						inferenceQueueReconciler.Log = func(format string, args ...any) {
							activity("info", fmt.Sprintf(format, args...))
						}
						// Same shape as runWork's registration (cmd/work.go): ignore
						// the passed-in NodeStatus and re-check serving via
						// nodeIsServingModels (DiscoverLocalEngines) so this reconciler
						// agrees with the startup check it mirrors, and dispatch via
						// `go` because nodeIsServingModels's underlying docker ps has
						// no context deadline -- running it inline here would stall
						// every subsequent heartbeat publish on a wedged runtime. The
						// reconciler self-limits to one in-flight probe.
						apiPublisher.SetOnStatus(func(_ *status.NodeStatus) { go inferenceQueueReconciler.Reconcile(ctx) })
					}

					// Fabric Pulse stats collector (citadel-cli#587), same wiring
					// as runWork: cached GPU + inference internals attached to
					// the heartbeat as the optional "stats" block. Started only
					// when this control center is the node's heartbeat publisher
					// (workerHeld is false here), so a node never runs two
					// collectors. CITADEL_HEARTBEAT_STATS=false is the kill
					// switch.
					if !pulse.Disabled() {
						pulseStats := pulse.NewCollector(pulse.CollectorConfig{
							Interval: pulse.ResolveInterval(""),
						})
						go pulseStats.Run(ctx)
						apiPublisher.SetStatsProvider(pulseStats.Latest)
					}
					go func() {
						activity("info", "Heartbeat publishing started")

						// Update TUI heartbeat indicator periodically
						heartbeatTicker := time.NewTicker(30 * time.Second)
						defer heartbeatTicker.Stop()

						go func() {
							// Initial heartbeat indicator
							if ccHeartbeatFn != nil {
								ccHeartbeatFn(true)
							}
							for {
								select {
								case <-ctx.Done():
									return
								case <-heartbeatTicker.C:
									if ccHeartbeatFn != nil {
										ccHeartbeatFn(true)
									}
								}
							}
						}()

						if err := apiPublisher.Start(ctx); err != nil && err != context.Canceled {
							activity("warning", fmt.Sprintf("Heartbeat error: %v", err))
						}
					}()
				}
			}
		}
	}

	// If a dedicated worker holds the lock, the control center does NOT run a
	// job-consuming runner (that is the whole point of the fix). Heartbeat and
	// telemetry are already wired above; block here in monitor-only mode until the
	// TUI context is cancelled, letting the real worker own all job consumption.
	if workerHeld {
		activity("success", "Monitor mode active (dedicated worker owns jobs)")
		<-ctx.Done()
		return ctx.Err()
	}

	// No dedicated worker on this node: the control center IS the only worker, so it
	// must handle the FULL node-job set (legacy + workflow + AGENT_UPDATE +
	// WHATSAPP_PROVISION), not a subset. Build handlers via the shared helper so the
	// registered set matches `citadel work` exactly and WHATSAPP_PROVISION /
	// AGENT_UPDATE never fail with "no handler" in a control-center-only run.

	// Pairing-display manager (citadel #659 P0), mirroring runWork's wiring:
	// this process is now the sole job consumer for this node (workerHeld is
	// false, or we returned above), so it is the right place to reconcile any
	// stale code left by a previous crashed process before job consumption
	// starts, and to clear on graceful shutdown. Safe/idempotent to Configure
	// again even if a dedicated `citadel work` also configured its own
	// process's singleton at some point -- they are separate processes with
	// separate in-memory managers, coordinated only through the shared
	// on-disk crash marker.
	pairingdisplay.Configure(network.GetNodeConfigDir())
	if pairingdisplay.Get().ReconcileStale() {
		activity("info", "cleared a stale pairing-code display left by a previous process")
	}
	defer pairingdisplay.Get().Shutdown()

	// Create worker ID
	workerID := fmt.Sprintf("citadel-tui-%s", uuid.New().String()[:8])

	// Create handlers with activity callback to route job output through TUI.
	wsDir := resolveWorkspaceDir()
	ccPerms := config.LoadPermissions(platform.ConfigDir())

	// Workflow executor for WORKFLOW_RUN jobs, mirroring runWork's wiring.
	ccWfExec := workflow.NewExecutor(workflow.ExecutorConfig{
		Shell: workflow.ShellConfig{WorkspaceDir: wsDir},
	})

	_, ccConfigDir, _ := findAndReadManifest()
	var ccPinnedServices []string
	if m, _, err := findAndReadManifest(); err == nil {
		ccPinnedServices = manifestPinnedServices(m)
	}
	nodeJobOpts := nodeJobHandlerOpts{
		LogFn:                     activity,
		WorkspaceDir:              wsDir,
		ConfigDir:                 ccConfigDir,
		AllowReadOutsideWorkspace: resolveAllowReadOutsideWorkspace(),
		ShellDisabled:             !ccPerms.Shell,
		ShellHasPasscode:          nodeHasPasscode,
		ShellVerifyPasscode:       nodePasscodeVerifier,
		DesktopDisabled:           !ccPerms.Desktop,
		FilesDisabled:             !ccPerms.Files,
		WorkflowExec:              ccWfExec,
		HandlerLog:                func(format string, args ...any) { activity("info", fmt.Sprintf(format, args...)) },
		PinnedServices:            ccPinnedServices,
	}
	// Swap manager (if any) is discarded here: this collector does not wire
	// WorkerLiveness/PinnedServices/ModelHotswap either (a pre-existing gap —
	// see CLAUDE.md's Service Preemption section), so surfacing swap activity
	// on the control-center-only heartbeat is a documented follow-up, not
	// silently regressed by this change.
	//
	// Same gap for the cache index (citadel #682 P2a): this path never calls
	// jobs.InitCacheIndexStore, unlike runWork (cmd/work.go). A
	// MODEL_CACHE_PULL/MODEL_CACHE_EVICT/SERVICE_START dispatched through the
	// control-center's own consume loop therefore gets a nil cache index
	// store and skips the write safely (jobs.CacheIndexStore()'s nil
	// contract) -- consistent with, not worse than, the other TUI-collector
	// gaps noted above, and not a P2a requirement.
	handlers, _ := buildNodeJobHandlers(nodeJobOpts)

	// Create runner with TUI callbacks
	runner := worker.NewRunner(source, handlers, worker.RunnerConfig{
		WorkerID:     workerID,
		NodeID:       headscaleNodeID,
		AgentVersion: Version,
		Verbose:      false,
		ActivityFn:   activity, // Route logs through TUI
		JobRecordFn: func(record usage.UsageRecord) {
			// Job recording callback - could be extended to pass to TUI
			// For now, the activity log covers job status
			_ = record
		},
	})

	if streamFactory != nil {
		runner.WithStreamWriterFactory(streamFactory)
	}

	// Register the node-targeted privileged handlers (AGENT_UPDATE, WHATSAPP_PROVISION)
	// on the live runner, shared with runWork. This closes the "no handler" hazard for
	// a control-center-only node.
	registerPrivilegedNodeJobHandlers(runner, nodeJobOpts)

	activity("success", "Worker started, listening for jobs...")

	// Run the worker (blocks until context is cancelled)
	return runner.Run(ctx)
}

// ccAutoUpdate checks for updates and auto-updates if available.
// Returns true if the binary was updated (caller should restart).
func ccAutoUpdate() bool {
	// Never auto-install when the user opted out (--no-auto-update /
	// CITADEL_NO_AUTO_UPDATE) or when this is a locally-built dev binary: a
	// hand-copied dev/test binary must not silently replace itself with a
	// release before it can be exercised. Explicit `citadel update` still works.
	if !autoUpdateAllowed() {
		return false
	}

	// Check for updates
	spinner := whimsy.NewSimpleSpinner([]string{"Checking for updates..."})
	spinner.Start()

	client := update.NewClient(Version)
	release, err := client.CheckForUpdate()
	if err != nil {
		spinner.StopWithWarning(fmt.Sprintf("Update check failed: %v", err))
		return false
	}

	if release == nil {
		spinner.StopWithSuccess(fmt.Sprintf("Running latest version (%s)", Version))
		return false
	}

	spinner.StopWithSuccess(fmt.Sprintf("Update available: %s → %s", Version, release.TagName))

	// Download update
	dlSpinner := whimsy.NewSimpleSpinner([]string{"Downloading update..."})
	dlSpinner.Start()

	pendingPath := update.GetPendingBinaryPath()
	if err := client.DownloadAndVerify(release, pendingPath); err != nil {
		dlSpinner.StopWithError(fmt.Sprintf("Download failed: %v", err))
		return false
	}

	dlSpinner.StopWithSuccess("Downloaded and verified")

	// Install update
	installSpinner := whimsy.NewSimpleSpinner([]string{"Installing update..."})
	installSpinner.Start()

	if err := update.ApplyUpdate(pendingPath); err != nil {
		installSpinner.StopWithError(fmt.Sprintf("Install failed: %v", err))
		return false
	}

	// Update state
	state, _ := update.LoadState()
	update.RecordUpdate(state, Version, release.TagName)
	update.UpdateLastCheck(state)
	_ = update.SaveState(state)

	installSpinner.StopWithSuccess(fmt.Sprintf("Updated to %s, restarting...", release.TagName))

	// Small delay to show the message
	time.Sleep(500 * time.Millisecond)

	// Restart the binary
	execPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get executable path: %v\n", err)
		fmt.Println("Please restart citadel manually.")
		return true
	}

	if runtime.GOOS == "windows" {
		// Windows doesn't support syscall.Exec; start a new process and exit
		cmd := exec.Command(execPath, os.Args[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Start()
		os.Exit(0)
	}

	// Unix: replace the current process in-place
	if err := syscall.Exec(execPath, os.Args, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to restart: %v\n", err)
		fmt.Println("Please restart citadel manually.")
	}

	return true
}

// buildProxmoxConfig checks for saved Proxmox configuration or auto-detects
// a local Proxmox installation and returns a ProxmoxConfig for the TUI.
func buildProxmoxConfig() controlcenter.ProxmoxConfig {
	configDir := platform.ConfigDir()

	// Try saved configuration first
	if pmxCfg, err := pmx.LoadConfig(configDir); err == nil && pmxCfg != nil && pmxCfg.BaseURL != "" {
		return controlcenter.ProxmoxConfig{
			Enabled:  true,
			BaseURL:  pmxCfg.BaseURL,
			TokenID:  pmxCfg.TokenID,
			Secret:   pmxCfg.TokenSecret,
			NodeName: pmxCfg.NodeName,
		}
	}

	// Try auto-detection (checks /etc/pve and pvesh)
	if pveInfo, err := platform.DetectProxmox(); err == nil && pveInfo.IsInstalled {
		return controlcenter.ProxmoxConfig{
			Enabled:  true,
			BaseURL:  "https://localhost:8006",
			NodeName: pveInfo.NodeName,
		}
	}

	return controlcenter.ProxmoxConfig{}
}
