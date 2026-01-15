// cmd/down.go
/*
Copyright © 2025 Jason Sun <jason@aceteam.ai>
*/
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

// downCmd represents the down command
var downCmd = &cobra.Command{
	Use:        "down",
	Short:      "Stops and removes the services defined in citadel.yaml",
	Hidden:     true, // Deprecated: use 'citadel stop' instead
	Deprecated: "use 'citadel stop' instead",
	Long: `Reads the citadel.yaml manifest and runs 'docker compose down' for each
service, stopping and removing the containers, networks, and volumes created by 'up'.`,
	Run: func(cmd *cobra.Command, args []string) {
		manifest, configDir, err := findAndReadManifest()
		if err != nil {
			// The error from findAndReadManifest is already user-friendly
			fmt.Fprintf(os.Stderr, "  %s\n", badColor.Sprint(err.Error()))
			return
		}
		if err != nil {
			// If the manifest doesn't exist, there's nothing to do.
			if os.IsNotExist(err) {
				fmt.Println("🤷 No citadel.yaml found, nothing to bring down.")
				return
			}
			fmt.Fprintf(os.Stderr, "❌ Error reading manifest: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("--- Tearing down services for node: %s ---\n", manifest.Node.Name)

		// We process in reverse order for graceful shutdown, though not strictly necessary.
		for i := len(manifest.Services) - 1; i >= 0; i-- {
			service := manifest.Services[i]
			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			fmt.Printf("🔻 Stopping service: %s (%s)\n", service.Name, fullComposePath)
			err := stopService(service)
			if err != nil {
				fmt.Fprintf(os.Stderr, "   ❌ Failed to stop service %s: %v\n", service.Name, err)
			} else {
				fmt.Printf("   ✅ Service %s is down.\n", service.Name)
			}
		}
		fmt.Println("\n🎉 Citadel Node services are offline.")
	},
}

func stopService(s Service) error {
	if s.ComposeFile == "" {
		return fmt.Errorf("service %s has no compose_file defined", s.Name)
	}

	// Check if the compose file exists before trying to use it
	if _, err := os.Stat(s.ComposeFile); os.IsNotExist(err) {
		return fmt.Errorf("compose file '%s' not found, cannot stop service", s.ComposeFile)
	}

	composeCmd := exec.Command("docker", "compose", "-f", s.ComposeFile, "down")
	output, err := composeCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose down failed: %s", string(output))
	}
	return nil
}

func init() {
	rootCmd.AddCommand(downCmd)
}
