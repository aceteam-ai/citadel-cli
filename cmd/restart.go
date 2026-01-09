// cmd/restart.go
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

// restartCmd represents the restart command
var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the services defined in citadel.yaml",
	Long: `Restarts all services defined in the citadel.yaml manifest by running
'docker compose restart' for each service. This is useful when you want to reload
service configurations without fully stopping and starting containers.`,
	Example: `  # Restart all services
  citadel restart`,
	Run: func(cmd *cobra.Command, args []string) {
		manifest, configDir, err := findAndReadManifest()
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error loading configuration: %v\n", err)
			os.Exit(1)
		}

		if len(manifest.Services) == 0 {
			fmt.Println("🤷 No services configured in citadel.yaml")
			return
		}

		fmt.Printf("--- Restarting services for node: %s ---\n", manifest.Node.Name)

		for _, service := range manifest.Services {
			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			fmt.Printf("🔄 Restarting service: %s (%s)\n", service.Name, fullComposePath)

			composeCmd := exec.Command("docker", "compose", "-f", fullComposePath, "restart")
			output, err := composeCmd.CombinedOutput()
			if err != nil {
				fmt.Fprintf(os.Stderr, "   ❌ Failed to restart service %s: %s\n", service.Name, string(output))
			} else {
				fmt.Printf("   ✅ Service %s restarted.\n", service.Name)
			}
		}

		fmt.Println("\n🎉 All services have been restarted.")
	},
}

func init() {
	rootCmd.AddCommand(restartCmd)
}
