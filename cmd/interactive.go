// cmd/interactive.go
package cmd

import (
	"fmt"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/tui"
	"github.com/aceteam-ai/citadel-cli/internal/tui/repl"
	"github.com/spf13/cobra"
)

var interactiveCmd = &cobra.Command{
	Use:     "interactive",
	Aliases: []string{"i", "shell", "repl"},
	Short:   "Start interactive mode with slash commands",
	Long: `Starts an interactive shell with slash commands for managing your Citadel node.

Available commands:
  /status    - Show node status dashboard
  /services  - List and manage services
  /logs      - View service logs
  /peers     - Show network peers
  /jobs      - Show job queue status
  /help      - Show available commands
  /quit      - Exit interactive mode`,
	Example: `  # Start interactive mode
  citadel interactive

  # Or use the short alias
  citadel i`,
	Run: func(cmd *cobra.Command, args []string) {
		runInteractiveMode()
	},
}

func init() {
	rootCmd.AddCommand(interactiveCmd)
}

// runInteractiveMode starts the interactive REPL
func runInteractiveMode() {
	// Check if TTY
	if !tui.IsTTY() {
		fmt.Fprintln(os.Stderr, "Interactive mode requires a terminal (TTY)")
		os.Exit(1)
	}

	// Get service names from manifest
	var services []string
	if manifest, _, err := findAndReadManifest(); err == nil && manifest != nil {
		for _, svc := range manifest.Services {
			services = append(services, svc.Name)
		}
	}

	// Run the REPL
	cfg := repl.Config{
		Version:  Version,
		Services: services,
	}

	if err := repl.Run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
