// cmd/rename.go
package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/spf13/cobra"
)

var (
	renameForce         bool
	renameSkipReconnect bool
)

var renameCmd = &cobra.Command{
	Use:   "rename <new-name>",
	Short: "Rename this node (OS hostname + Headscale node name)",
	Long: `Renames this Citadel node. This updates:

  1. The OS hostname (best-effort; requires root on Linux)
  2. The node name stored in the manifest (citadel.yaml)
  3. The saved hostname in the global config
  4. The Headscale node name (by reconnecting to the network)

The original pre-registration hostname recorded during 'citadel init' is
preserved and can be restored by passing it as <new-name>.

Examples:
  citadel rename gpu-server-1
  citadel rename --no-reconnect my-node   # update names without reconnecting`,
	Args: cobra.ExactArgs(1),
	Run:  runRename,
}

func runRename(cmd *cobra.Command, args []string) {
	newName := args[0]

	if !platform.IsValidHostname(newName) {
		fmt.Fprintf(os.Stderr, "❌ '%s' is not a valid hostname.\n", newName)
		fmt.Fprintln(os.Stderr, "   Names must be 1-63 characters, letters/digits/hyphens, not starting or ending with a hyphen.")
		os.Exit(1)
	}

	// Resolve the current node name from the manifest for display/confirmation.
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	oldName := manifest.Node.Name

	if oldName == newName {
		fmt.Printf("Node is already named '%s'. Nothing to do.\n", newName)
		return
	}

	if !renameForce {
		fmt.Printf("Rename node '%s' to '%s'? (y/N) ", oldName, newName)
		if !confirmRename() {
			fmt.Println("Aborted.")
			return
		}
	}

	fmt.Printf("--- Renaming node to '%s' ---\n", newName)

	// 1. Update the OS hostname (best-effort; no-op on non-Linux, needs root).
	if err := platform.SetHostname(newName); err != nil {
		fmt.Printf("   - Warning: could not set OS hostname: %v\n", err)
		fmt.Println("     (run with sudo to update the OS hostname)")
	} else if platform.IsLinux() {
		fmt.Printf("   - OS hostname set to %s\n", newName)
	}

	// 2. Update the manifest node name.
	manifest.Node.Name = newName
	manifestPath := filepath.Join(configDir, "citadel.yaml")
	if err := writeManifest(manifestPath, manifest); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to update manifest: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("   - Manifest updated")

	// 3. Persist the new hostname to the global config for re-run stability.
	if err := saveHostnameToConfig(newName); err != nil {
		fmt.Printf("   - Warning: could not save hostname to config: %v\n", err)
	} else {
		fmt.Println("   - Config updated")
	}

	// 4. Reconnect so Headscale picks up the new node name.
	if renameSkipReconnect {
		fmt.Println("   - Skipping network reconnect (--no-reconnect)")
		fmt.Println("\n✅ Local names updated. Reconnect or restart the worker to update Headscale.")
		return
	}

	if !network.HasState() {
		fmt.Println("\n✅ Renamed locally. Run 'citadel login' to register the new name with Headscale.")
		return
	}

	if err := reconnectWithNewName(newName); err != nil {
		fmt.Printf("   - Warning: could not reconnect with new name: %v\n", err)
		fmt.Println("\n   Local names were updated. Restart the worker or run 'citadel login' to")
		fmt.Println("   propagate the new name to the coordination server.")
		return
	}

	fmt.Printf("   - Reconnected to network as '%s'\n", newName)
	fmt.Printf("\n✅ Node renamed to '%s'.\n", newName)
}

// reconnectWithNewName disconnects from the network and reconnects using the
// existing state directory with the new hostname so that the Headscale node
// name is updated. The machine key (and thus the node's IP) is preserved
// because the state directory is reused.
func reconnectWithNewName(newName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Drop any existing connection so we can re-establish with the new hostname.
	_ = network.Disconnect()

	config := network.ServerConfig{
		Hostname:   newName,
		ControlURL: nexusURL,
		StateDir:   network.GetStateDir(),
	}

	srv, err := network.Connect(ctx, config)
	if err != nil {
		return err
	}

	// Disconnect cleanly so tsnet flushes its state files, matching the
	// behavior of connectToNetwork during 'citadel init'.
	_ = network.Disconnect()
	_ = srv

	return nil
}

// confirmRename reads a yes/no answer from stdin.
func confirmRename() bool {
	var response string
	fmt.Scanln(&response)
	switch response {
	case "y", "Y", "yes", "Yes", "YES":
		return true
	default:
		return false
	}
}

func init() {
	rootCmd.AddCommand(renameCmd)
	renameCmd.Flags().BoolVarP(&renameForce, "force", "f", false, "Skip the confirmation prompt")
	renameCmd.Flags().BoolVar(&renameSkipReconnect, "no-reconnect", false, "Update local names only; do not reconnect to the network")
}
