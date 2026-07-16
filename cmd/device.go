// cmd/device.go
// Device mode (#5959): give plain mesh devices (laptops) the same durable
// fabric identity Citadel nodes have, so their mesh membership self-heals
// instead of silently expiring.
//
// A device is NOT a compute node — no worker, no job queues. It:
//  1. enrolls interactively once (browser approve = the trust-root event),
//     receiving an org authkey + a long-TTL fabric CA leaf for its keypair;
//  2. joins the mesh through the SYSTEM tailscale client (incl. the macOS
//     App Store app), not embedded tsnet;
//  3. runs a tiny daemon that watches the session and, when it breaks or the
//     node key nears expiry, proves possession of its leaf over mTLS to the
//     nexus reenroll service, gets a fresh org authkey, and re-runs
//     tailscale up. Membership becomes derived state; offboarding is one CRL
//     revocation.
//
// Bonus: an enrolled laptop is a ready BYOC candidate — the identity store is
// the same one `citadel init` uses, so flipping device -> node later is a
// config change, not a re-enrollment.
package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/devicemode"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/ui"
	"github.com/spf13/cobra"
)

// deviceCheckIntervalEnv overrides the daemon's health-check interval in
// seconds. Non-positive values fall back to the default (they do NOT disable
// the loop — a disabled device daemon is just a stopped daemon).
const deviceCheckIntervalEnv = "CITADEL_DEVICE_CHECK_INTERVAL"

const defaultDeviceCheckInterval = 15 * time.Minute

var deviceCmd = &cobra.Command{
	Use:   "device",
	Short: "Enroll and maintain this machine as a personal device on your org's network",
	Long: `Device mode connects a personal machine (laptop, workstation) to your
organization's AceTeam Network with a durable, self-healing identity.

Unlike 'citadel init' (which provisions a compute node), device mode runs no
workloads. It enrolls this machine once via a browser approval, then keeps its
network membership alive automatically: when the session breaks or its key
nears expiry, the device proves its identity with a certificate and rejoins
without any human involvement.

Requires the Tailscale client to be installed (device mode drives it; it does
not replace it).`,
}

var deviceEnrollCmd = &cobra.Command{
	Use:   "enroll",
	Short: "Enroll this machine as a device (one-time browser approval)",
	Long: `Enroll generates this machine's identity keypair, opens an approval
request, and prints a link (and QR code) for you to approve in your browser
while signed in to AceTeam. On approval the machine receives:

  - an identity certificate (issued by your org's fabric CA, ~1 year), and
  - a one-time key to join your org's network,

then joins the network via the system Tailscale client. Run
'citadel device install' afterwards to keep the membership self-healing.`,
	RunE: runDeviceEnroll,
}

var deviceRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the device daemon (watches the session, self-heals it)",
	RunE:  runDeviceDaemon,
}

var deviceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show device identity and network session health",
	RunE:  runDeviceStatus,
}

var deviceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install the device daemon as a background service (launchd/systemd)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := devicemode.LoadConfig(); err != nil {
			return fmt.Errorf("this machine is not enrolled yet — run 'citadel device enroll' first")
		}
		if err := devicemode.InstallService(); err != nil {
			return err
		}
		fmt.Println("✅ Device daemon installed. It will keep this machine's network membership alive.")
		return nil
	},
}

var deviceUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the device daemon background service",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := devicemode.UninstallService(); err != nil {
			return err
		}
		fmt.Println("Device daemon service removed.")
		return nil
	},
}

func runDeviceEnroll(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	store := nodeidentity.Default()
	httpClient := &http.Client{Timeout: 15 * time.Second}

	machineID, err := devicemode.MachineID()
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()

	code, err := devicemode.NewPairingCode()
	if err != nil {
		return err
	}

	if err := devicemode.StartDevicePairing(
		ctx, httpClient, authServiceURL, code, store, hostname, machineID,
	); err != nil {
		return err
	}

	pairURL := devicemode.PairURL(authServiceURL, code)
	fmt.Println("Approve this device in your browser (you must be signed in to AceTeam):")
	fmt.Printf("\n   %s\n\n", pairURL)
	fmt.Println(ui.RenderQRCode(pairURL))
	fmt.Println("Waiting for approval (link expires in 5 minutes)...")

	result, err := devicemode.WaitForApproval(ctx, httpClient, authServiceURL, code)
	if err != nil {
		return err
	}
	fmt.Println("✅ Approved.")

	if result.LeafPem == "" {
		// Without a leaf there is no self-heal identity — the whole point of
		// device mode. Join anyway (the authkey is valid) but be loud.
		fmt.Fprintln(os.Stderr, "⚠️  The server did not issue an identity certificate (fabric CA inactive?).")
		fmt.Fprintln(os.Stderr, "   This device will join the network but will NOT self-heal; re-enroll later.")
	} else if err := store.StoreLeaf(result.LeafPem, result.ChainPem); err != nil {
		return fmt.Errorf("store identity certificate: %w", err)
	} else {
		fmt.Printf("   Identity certificate stored (%s)\n", store.LeafPath())
	}

	cfg := &devicemode.Config{
		NodeUID:     result.NodeUID,
		NexusURL:    nexusURL,
		ReenrollURL: reenrollURLFor(nexusURL),
		APIBaseURL:  authServiceURL,
	}
	if err := devicemode.SaveConfig(cfg); err != nil {
		return err
	}

	bin, err := devicemode.FindTailscale()
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  %v\n", err)
		fmt.Fprintln(os.Stderr, "   Install Tailscale, then join manually with the authkey printed by")
		fmt.Fprintln(os.Stderr, "   'citadel device status', or re-run 'citadel device enroll'.")
		return fmt.Errorf("tailscale CLI not found")
	}

	fmt.Println("\n--- 🌐 Joining your org's network ---")
	if err := devicemode.Up(ctx, bin, cfg.NexusURL, result.Authkey, false); err != nil {
		return tailscaleUpErrorHint(err)
	}

	fmt.Println("✅ This device is enrolled and on your org's network.")
	fmt.Println("   Run 'citadel device install' to keep it self-healing in the background.")
	return nil
}

func runDeviceDaemon(cmd *cobra.Command, args []string) error {
	cfg, err := devicemode.LoadConfig()
	if err != nil {
		return fmt.Errorf("this machine is not enrolled yet — run 'citadel device enroll' first (%v)", err)
	}
	store := nodeidentity.Default()

	interval := defaultDeviceCheckInterval
	if raw := os.Getenv(deviceCheckIntervalEnv); raw != "" {
		if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
			interval = time.Duration(secs) * time.Second
		}
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("Device daemon: node_uid=%s check every %s\n", cfg.NodeUID, interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	deviceTick(ctx, cfg, store)
	for {
		select {
		case <-ctx.Done():
			fmt.Println("Device daemon stopping.")
			return nil
		case <-ticker.C:
			deviceTick(ctx, cfg, store)
		}
	}
}

// deviceTick runs one health evaluation + (if needed) self-heal pass. It never
// returns an error: the daemon's job is to keep trying, so failures are logged
// and retried next tick.
func deviceTick(ctx context.Context, cfg *devicemode.Config, store *nodeidentity.Store) {
	now := time.Now()

	leaf, err := store.LoadLeaf()
	if err != nil {
		logDevice("no identity certificate (%v) — run 'citadel device enroll'", err)
		return
	}

	bin, err := devicemode.FindTailscale()
	if err != nil {
		logDevice("%v", err)
		return
	}

	tickCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	st, err := devicemode.Status(tickCtx, bin)
	if err != nil {
		logDevice("cannot read tailscale status (%v) — is the Tailscale app/daemon running?", err)
		return
	}

	decision := devicemode.Decide(st.BackendState, st.Self.KeyExpiry, leaf.NotAfter, now)
	logDevice("state=%s leaf_expires=%s: %s",
		st.BackendState, leaf.NotAfter.Format("2006-01-02"), decision.Reason)

	if decision.LeafExpired {
		return
	}
	if decision.LeafExpiresSoon {
		logDevice("WARNING: identity certificate expires %s — re-run 'citadel device enroll' before then",
			leaf.NotAfter.Format("2006-01-02"))
	}
	if !decision.Reenroll {
		return
	}

	clientCert, err := devicemode.LoadClientCertificate(store)
	if err != nil {
		logDevice("self-heal blocked: %v", err)
		return
	}
	result, err := devicemode.Reenroll(tickCtx, cfg.ReenrollURL, clientCert)
	if err != nil {
		logDevice("reenroll failed (will retry): %v", err)
		return
	}
	logDevice("reenrolled as node_uid=%s; re-authenticating tailscale", result.NodeUID)

	if err := devicemode.Up(tickCtx, bin, cfg.NexusURL, result.Authkey, true); err != nil {
		logDevice("tailscale re-auth failed (will retry): %v", tailscaleUpErrorHint(err))
		return
	}
	logDevice("session restored")
}

func runDeviceStatus(cmd *cobra.Command, args []string) error {
	cfg, err := devicemode.LoadConfig()
	if err != nil {
		fmt.Println("Not enrolled. Run 'citadel device enroll' to add this machine to your org's network.")
		return nil
	}
	fmt.Printf("Device:      node_uid=%s\n", cfg.NodeUID)
	fmt.Printf("Coordinator: %s\n", cfg.NexusURL)

	store := nodeidentity.Default()
	now := time.Now()
	var leafNotAfter time.Time
	if leaf, err := store.LoadLeaf(); err != nil {
		fmt.Printf("Identity:    MISSING (%v)\n", err)
	} else {
		leafNotAfter = leaf.NotAfter
		days := int(leaf.NotAfter.Sub(now).Hours() / 24)
		fmt.Printf("Identity:    certificate valid until %s (%d days)\n",
			leaf.NotAfter.Format("2006-01-02"), days)
	}

	bin, err := devicemode.FindTailscale()
	if err != nil {
		fmt.Printf("Network:     %v\n", err)
		return nil
	}
	st, err := devicemode.Status(cmd.Context(), bin)
	if err != nil {
		fmt.Printf("Network:     cannot read status (%v)\n", err)
		return nil
	}
	fmt.Printf("Network:     %s\n", st.BackendState)
	if st.Self.KeyExpiry != nil && !st.Self.KeyExpiry.IsZero() {
		fmt.Printf("Key expiry:  %s\n", st.Self.KeyExpiry.Format(time.RFC3339))
	}

	if !leafNotAfter.IsZero() {
		decision := devicemode.Decide(st.BackendState, st.Self.KeyExpiry, leafNotAfter, now)
		fmt.Printf("Health:      %s\n", decision.Reason)
	}
	return nil
}

// reenrollURLFor derives the mTLS reenroll endpoint from the coordinator URL.
func reenrollURLFor(nexus string) string {
	return strings.TrimSuffix(nexus, "/") + "/fabric/reenroll"
}

// tailscaleUpErrorHint decorates a failed `tailscale up` with the most common
// remedy (on Linux the tailscale socket is root-owned unless an operator is
// configured).
func tailscaleUpErrorHint(err error) error {
	if platform.IsLinux() {
		return fmt.Errorf("%w\nHint: allow your user to manage Tailscale once with:\n  sudo tailscale set --operator=$USER", err)
	}
	return err
}

func logDevice(format string, args ...interface{}) {
	fmt.Printf("[%s] %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func init() {
	rootCmd.AddCommand(deviceCmd)
	deviceCmd.AddCommand(deviceEnrollCmd)
	deviceCmd.AddCommand(deviceRunCmd)
	deviceCmd.AddCommand(deviceStatusCmd)
	deviceCmd.AddCommand(deviceInstallCmd)
	deviceCmd.AddCommand(deviceUninstallCmd)
}
