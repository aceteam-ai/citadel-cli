// cmd/up.go
// `citadel up` / `citadel down` — machine-wide network mode (issue #643).
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/spf13/cobra"
)

var (
	upAuthkey  string
	upNodeName string
	upCheck    bool
)

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Put this whole machine on the AceTeam Network (needs sudo)",
	Long: `Brings up a network interface so EVERY program on this machine can reach
your other nodes directly — ssh, curl, a browser, a database client — using
their network IPs and names.

This is different from 'citadel login', which connects only the citadel
process itself. Both use the same node identity, so this machine appears once
on your network either way.

Because it creates a network interface and edits the system routing table,
this command needs administrator privileges. 'citadel login' does not, and
remains the right choice if you only need citadel itself to reach the network.

Runs in the foreground until interrupted; Ctrl-C takes the machine back off
the network and restores routing and DNS.`,
	Example: `  # Put this machine on the network (foreground)
  sudo citadel up

  # First-time join with an authkey
  sudo citadel up --authkey tskey-auth-xxx

  # Check whether this machine can do it, without changing anything
  sudo citadel up --check

  # Take the machine back off the network
  sudo citadel down`,
	Run: func(cmd *cobra.Command, args []string) {
		if upCheck {
			if err := runUpCheck(); err != nil {
				badColor.Printf("Error: %v\n", err)
				os.Exit(1)
			}
			return
		}
		if err := runUp(); err != nil {
			badColor.Printf("Error: %v\n", err)
			os.Exit(1)
		}
	},
}

// runUpCheck reports whether machine-wide mode can work here WITHOUT leaving
// anything behind: it creates the network interface and immediately removes
// it, never starting the engine, installing routes, or touching DNS.
//
// This is the safe way to answer "will 'citadel up' work on this box?" — and
// it is safe to run on a machine already carrying other VPN software, or to
// kill part-way, because there is no state to strand.
func runUpCheck() error {
	res := network.PreflightMachineWide(platform.IsRoot())

	fmt.Println("Machine-wide network readiness:")
	if res.AlreadyUp {
		fmt.Println("  Already running: yes ('citadel up' is active on this machine)")
	}

	if !res.Elevated {
		badColor.Printf("  Administrator:   NO — %s\n", res.Detail)
		return fmt.Errorf("machine-wide mode is unavailable without administrator privileges")
	}
	goodColor.Println("  Administrator:   yes")

	if !res.DeviceOK {
		badColor.Printf("  Network device:  FAILED — %s\n", res.Detail)
		return fmt.Errorf("this machine cannot create the network interface machine-wide mode needs")
	}
	goodColor.Printf("  Network device:  yes (%s)\n", res.Device)
	fmt.Println("\nThis machine can run 'citadel up'.")
	return nil
}

func runUp() error {
	nodeName := upNodeName
	if nodeName == "" {
		if saved := getSavedHostname(); saved != "" {
			nodeName = saved
		} else {
			nodeName, _ = os.Hostname()
		}
	}

	// (network.SetLogf is wired once in root.go's PersistentPreRun -- #662.
	// It must stay wired: without it a failed bring-up reports only "timeout
	// waiting for network connection" and the backend-state transitions that
	// identify the real cause are silently discarded.)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("Bringing %s onto the AceTeam Network (machine-wide)...\n", nodeName)

	srv, err := network.ConnectMachineWide(ctx, network.ServerConfig{
		Hostname:   nodeName,
		ControlURL: nexusURL,
		AuthKey:    upAuthkey,
	}, platform.IsRoot())
	if err != nil {
		var needsElevation *network.ErrNeedsElevation
		if errors.As(err, &needsElevation) {
			// Deliberately does NOT fall back to userspace: a user who asked
			// for machine-wide routing must not be left thinking they got it.
			return fmt.Errorf("%w\n\n   Or use 'citadel login' for an unprivileged, citadel-only connection", err)
		}
		return err
	}

	ip, _ := srv.GetIPv4()
	goodColor.Printf("\n✓ %s is on the AceTeam Network\n", nodeName)
	fmt.Printf("  Network IP: %s\n", ip)
	fmt.Printf("  Scope:      whole machine — every program here can reach your nodes\n\n")

	if peers, perr := srv.GetPeers(ctx); perr == nil && len(peers) > 0 {
		fmt.Println("  Reachable now:")
		for _, p := range peers {
			if p.Online && p.IP != "" {
				fmt.Printf("    %-20s %s\n", p.Hostname, p.IP)
			}
		}
		fmt.Println()
	}

	fmt.Println("Press Ctrl-C to take this machine back off the network.")
	<-ctx.Done()

	fmt.Println("\nRestoring network configuration...")
	if err := network.Disconnect(); err != nil {
		return fmt.Errorf("clean shutdown failed: %w", err)
	}
	goodColor.Println("✓ Machine is off the AceTeam Network; routing and DNS restored")
	return nil
}

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Take this machine off the AceTeam Network and restore routing/DNS",
	Long: `Reverses 'citadel up'.

If a 'citadel up' is running in another terminal, stop it there (Ctrl-C) so it
can shut down cleanly. This command is the recovery path for when that did not
happen — after a crash or a hard reboot — and removes any network interface
configuration, routes, and DNS settings left behind.`,
	Run: func(cmd *cobra.Command, args []string) {
		if !platform.IsRoot() {
			badColor.Printf("Error: restoring system routing needs administrator privileges: %s\n",
				network.ElevationHint())
			os.Exit(1)
		}
		if network.MachineWideRunning() {
			fmt.Println("A 'citadel up' is running. Stop it with Ctrl-C in its terminal for a clean shutdown.")
			fmt.Println("Continuing anyway to clear any stale system configuration...")
		}
		network.CleanUpSystemState()
		goodColor.Println("✓ Routing and DNS restored")
	},
}

func init() {
	rootCmd.AddCommand(upCmd)
	rootCmd.AddCommand(downCmd)
	upCmd.Flags().StringVar(&upAuthkey, "authkey", "", "Pre-generated authkey for non-interactive join")
	upCmd.Flags().StringVar(&upNodeName, "node-name", "", "Override the node name (defaults to hostname)")
	upCmd.Flags().BoolVar(&upCheck, "check", false, "Report whether this machine can run machine-wide mode, then exit (changes nothing)")
}
