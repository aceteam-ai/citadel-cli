// cmd/expose.go
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/spf13/cobra"
)

var (
	exposeService string
	exposeList    bool
	exposeRemove  bool
	exposeHTTPS   bool
)

// Known service ports
var servicePorts = map[string]int{
	"vllm":     8000,
	"ollama":   11434,
	"llamacpp": 8080,
	"lmstudio": 1234,
}

var exposeCmd = &cobra.Command{
	Use:   "expose [port]",
	Short: "Expose a local port over the Tailscale network",
	Long: `Expose a local service port so other nodes in the network can access it.

By default, services are accessible via your Tailscale IP. This command
uses 'tailscale serve' to create friendly HTTPS URLs.

Without any arguments, shows the current node's access information.`,
	Example: `  # Show current access info (Tailscale IP and hostname)
  citadel expose

  # Expose a specific port
  citadel expose 8080

  # Expose a known service by name
  citadel expose --service ollama

  # List currently exposed ports
  citadel expose --list

  # Stop exposing a port
  citadel expose --remove 8080`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		// Handle --list flag
		if exposeList {
			listExposedPorts()
			return
		}

		// Handle --remove flag
		if exposeRemove {
			if len(args) == 0 && exposeService == "" {
				fmt.Fprintln(os.Stderr, "Error: specify a port or --service to remove")
				os.Exit(1)
			}
			port := getPort(args)
			removeExposedPort(port)
			return
		}

		// If no args and no service, show access info
		if len(args) == 0 && exposeService == "" {
			showAccessInfo()
			return
		}

		// Expose a port
		port := getPort(args)
		exposePort(port)
	},
}

func getPort(args []string) int {
	// If service flag is set, use service port
	if exposeService != "" {
		port, ok := servicePorts[exposeService]
		if !ok {
			fmt.Fprintf(os.Stderr, "Error: unknown service '%s'\n", exposeService)
			fmt.Fprintln(os.Stderr, "Known services: vllm, ollama, llamacpp, lmstudio")
			os.Exit(1)
		}
		return port
	}

	// Otherwise parse port from args
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Error: specify a port number")
		os.Exit(1)
	}

	port, err := strconv.Atoi(args[0])
	if err != nil || port < 1 || port > 65535 {
		fmt.Fprintf(os.Stderr, "Error: invalid port number '%s'\n", args[0])
		os.Exit(1)
	}
	return port
}

func showAccessInfo() {
	fmt.Println("Network Access Information")
	fmt.Println("==========================")
	fmt.Println()

	// Get Tailscale status
	tailscaleCLI := getTailscaleCLIPath()
	statusCmd := exec.Command(tailscaleCLI, "status", "--json")
	output, err := statusCmd.Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error: not connected to Tailscale network")
		fmt.Fprintln(os.Stderr, "Run 'citadel login' to connect first")
		os.Exit(1)
	}

	var status struct {
		Self struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing Tailscale status: %v\n", err)
		os.Exit(1)
	}

	// Display access info
	if len(status.Self.TailscaleIPs) > 0 {
		fmt.Printf("Tailscale IP:  %s\n", status.Self.TailscaleIPs[0])
	}
	if status.Self.DNSName != "" {
		// Remove trailing dot from DNS name
		dnsName := strings.TrimSuffix(status.Self.DNSName, ".")
		fmt.Printf("Hostname:      %s\n", dnsName)
	}
	fmt.Println()

	// Show known service ports
	fmt.Println("Common Service Ports:")
	for name, port := range servicePorts {
		if len(status.Self.TailscaleIPs) > 0 {
			fmt.Printf("  %s: http://%s:%d\n", name, status.Self.TailscaleIPs[0], port)
		}
	}
	fmt.Println()
	fmt.Println("To expose a port with HTTPS, run: citadel expose <port>")
}

func exposePort(port int) {
	tailscaleCLI := getTailscaleCLIPath()

	fmt.Printf("Exposing port %d...\n", port)

	// Use tailscale serve to expose the port
	var serveCmd *exec.Cmd
	localURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	if platform.IsWindows() {
		serveCmd = exec.Command(tailscaleCLI, "serve", "--bg", localURL)
	} else {
		serveCmd = exec.Command("sudo", tailscaleCLI, "serve", "--bg", localURL)
	}

	output, err := serveCmd.CombinedOutput()
	if err != nil {
		// Check if it's just a "already serving" type message
		outputStr := string(output)
		if strings.Contains(outputStr, "already") {
			fmt.Printf("Port %d is already exposed.\n", port)
			return
		}
		fmt.Fprintf(os.Stderr, "Error exposing port: %s\n", outputStr)
		os.Exit(1)
	}

	// Get the HTTPS URL
	statusCmd := exec.Command(tailscaleCLI, "serve", "status", "--json")
	statusOutput, _ := statusCmd.Output()

	fmt.Printf("Port %d is now exposed!\n\n", port)
	fmt.Println("Access URLs:")

	// Show the Tailscale IP access
	ipCmd := exec.Command(tailscaleCLI, "ip", "-4")
	if ipOutput, err := ipCmd.Output(); err == nil {
		ip := strings.TrimSpace(string(ipOutput))
		fmt.Printf("  http://%s:%d\n", ip, port)
	}

	// Parse serve status for HTTPS URL
	if len(statusOutput) > 0 {
		var serveStatus map[string]interface{}
		if json.Unmarshal(statusOutput, &serveStatus) == nil {
			// Try to extract the serve URL
			if web, ok := serveStatus["Web"].(map[string]interface{}); ok {
				for url := range web {
					fmt.Printf("  %s\n", url)
				}
			}
		}
	}
}

func removeExposedPort(port int) {
	tailscaleCLI := getTailscaleCLIPath()

	fmt.Printf("Removing port %d...\n", port)

	// Use tailscale serve off to remove
	var serveCmd *exec.Cmd
	portStr := strconv.Itoa(port)

	if platform.IsWindows() {
		serveCmd = exec.Command(tailscaleCLI, "serve", "off", portStr)
	} else {
		serveCmd = exec.Command("sudo", tailscaleCLI, "serve", "off", portStr)
	}

	output, err := serveCmd.CombinedOutput()
	if err != nil {
		outputStr := string(output)
		if strings.Contains(outputStr, "not serving") || strings.Contains(outputStr, "nothing") {
			fmt.Printf("Port %d was not exposed.\n", port)
			return
		}
		fmt.Fprintf(os.Stderr, "Error: %s\n", outputStr)
		os.Exit(1)
	}

	fmt.Printf("Port %d is no longer exposed.\n", port)
}

func listExposedPorts() {
	tailscaleCLI := getTailscaleCLIPath()

	statusCmd := exec.Command(tailscaleCLI, "serve", "status")
	output, err := statusCmd.CombinedOutput()
	if err != nil {
		outputStr := string(output)
		if strings.Contains(outputStr, "not serving") || strings.Contains(outputStr, "No serve") {
			fmt.Println("No ports currently exposed.")
			return
		}
		fmt.Fprintf(os.Stderr, "Error: %s\n", outputStr)
		os.Exit(1)
	}

	fmt.Println("Currently exposed ports:")
	fmt.Println(string(output))
}

// getTailscaleCLIPath returns the path to the tailscale CLI.
// Delegates to the centralized platform.GetTailscaleCLI() which handles
// PATH lookup and platform-specific fallback locations for Windows, macOS, and Linux.
func getTailscaleCLIPath() string {
	return platform.GetTailscaleCLI()
}

func init() {
	rootCmd.AddCommand(exposeCmd)
	exposeCmd.Flags().StringVar(&exposeService, "service", "", "Expose a known service by name (vllm, ollama, llamacpp, lmstudio)")
	exposeCmd.Flags().BoolVar(&exposeList, "list", false, "List currently exposed ports")
	exposeCmd.Flags().BoolVar(&exposeRemove, "remove", false, "Stop exposing a port")
}
