// cmd/demo.go
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aceteam-ai/citadel-cli/internal/demo"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/spf13/cobra"
)

var demoPort int

var demoCmd = &cobra.Command{
	Use:   "demo",
	Short: "Start a demo web server showing node info",
	Long: `Start a simple HTTP server that displays node information.

This is useful for:
- Testing network connectivity between peers
- Demonstrating that the node is accessible
- Quick health checks

The server displays:
- Node hostname and platform
- Network IP (if connected)
- GPU information (if available)
- Service status

Example:
  citadel demo              # Start on default port 7777
  citadel demo --port 8080  # Start on custom port`,
	RunE: runDemo,
}

func init() {
	demoCmd.Flags().IntVarP(&demoPort, "port", "p", 7777, "Port to listen on")
	rootCmd.AddCommand(demoCmd)
}

func runDemo(cmd *cobra.Command, args []string) error {
	fmt.Printf("Starting demo server on http://localhost:%d\n", demoPort)
	fmt.Println("Press Ctrl+C to stop")

	// Create info callback
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

	server := demo.NewServer(demoPort, Version, getInfo)

	// Handle graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nShutting down...")
		cancel()
	}()

	if network.IsGlobalConnected() {
		if ip, err := network.GetGlobalIPv4(); err == nil {
			fmt.Printf("Accessible at http://%s:%d on the network\n", ip, demoPort)
		}
	}

	return server.Start(ctx)
}
