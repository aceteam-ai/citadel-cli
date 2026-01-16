// cmd/expose.go
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/aceboss/citadel-cli/internal/platform"
	"github.com/spf13/cobra"
)

var (
	exposeService   string
	exposeList      bool
	exposeRemove    bool
	exposeHTTPS     bool
	exposeLocalPort int // --to flag for destination port
)

// Known service ports
var servicePorts = map[string]int{
	"vllm":     8000,
	"ollama":   11434,
	"llamacpp": 8080,
	"lmstudio": 1234,
}

var exposeCmd = &cobra.Command{
	Use:   "expose [port] or [network-port:local-port]",
	Short: "Expose a local port over the Tailscale network",
	Long: `Expose a local service port so other nodes in the network can access it.

By default, services are accessible via your Tailscale IP. This command
uses 'tailscale serve' to create friendly HTTPS URLs.

Port mapping can be specified as:
  - Single port: expose the same port (e.g., 8080 -> localhost:8080)
  - Port pair: network-port:local-port (e.g., 443:8000 -> localhost:8000 as 443)
  - With --to flag: citadel expose 443 --to 8000

Without any arguments, shows the current node's access information.`,
	Example: `  # Show current access info (Tailscale IP and hostname)
  citadel expose

  # Expose a specific port (same port locally and on network)
  citadel expose 8080

  # Expose local port 8000 as port 443 on the network
  citadel expose 443:8000
  citadel expose 443 --to 8000

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
			mapping := getPortMapping(args)
			removeExposedPort(mapping.networkPort)
			return
		}

		// If no args and no service, show access info
		if len(args) == 0 && exposeService == "" {
			showAccessInfo()
			return
		}

		// Expose a port
		mapping := getPortMapping(args)
		exposePortMapping(mapping)
	},
}

// portMapping holds the network port and local port for exposure
type portMapping struct {
	networkPort int // Port exposed on the network
	localPort   int // Local port to forward to
}

func getPortMapping(args []string) portMapping {
	// If service flag is set, use service port
	if exposeService != "" {
		port, ok := servicePorts[exposeService]
		if !ok {
			fmt.Fprintf(os.Stderr, "Error: unknown service '%s'\n", exposeService)
			fmt.Fprintln(os.Stderr, "Known services: vllm, ollama, llamacpp, lmstudio")
			os.Exit(1)
		}
		return portMapping{networkPort: port, localPort: port}
	}

	// Otherwise parse port from args
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Error: specify a port number")
		os.Exit(1)
	}

	portArg := args[0]

	// Check for port:port syntax (network:local)
	if strings.Contains(portArg, ":") {
		parts := strings.SplitN(portArg, ":", 2)
		networkPort, err1 := strconv.Atoi(parts[0])
		localPort, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || networkPort < 1 || networkPort > 65535 || localPort < 1 || localPort > 65535 {
			fmt.Fprintf(os.Stderr, "Error: invalid port mapping '%s' (use network-port:local-port)\n", portArg)
			os.Exit(1)
		}
		return portMapping{networkPort: networkPort, localPort: localPort}
	}

	// Single port
	port, err := strconv.Atoi(portArg)
	if err != nil || port < 1 || port > 65535 {
		fmt.Fprintf(os.Stderr, "Error: invalid port number '%s'\n", portArg)
		os.Exit(1)
	}

	// Check if --to flag is set for local port
	localPort := port
	if exposeLocalPort > 0 {
		localPort = exposeLocalPort
	}

	return portMapping{networkPort: port, localPort: localPort}
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
		fmt.Fprintln(os.Stderr, "Error: not connected to AceTeam Network")
		fmt.Fprintln(os.Stderr, "Run 'citadel login' to connect first")
		os.Exit(1)
	}

	var status struct {
		Self struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
			Online       bool     `json:"Online"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing network status: %v\n", err)
		os.Exit(1)
	}

	// Check if actually online
	if !status.Self.Online {
		fmt.Fprintln(os.Stderr, "Error: not connected to AceTeam Network")
		fmt.Fprintln(os.Stderr, "Run 'citadel login' to connect first")
		os.Exit(1)
	}

	// Display access info
	if len(status.Self.TailscaleIPs) > 0 {
		fmt.Printf("Tailscale IP:  %s\n", status.Self.TailscaleIPs[0])
	}
	if status.Self.DNSName != "" {
		// Remove trailing dot and tailnet suffix for cleaner display
		dnsName := strings.TrimSuffix(status.Self.DNSName, ".")
		if idx := strings.Index(dnsName, "."); idx > 0 {
			dnsName = dnsName[:idx]
		}
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

func exposePortMapping(mapping portMapping) {
	tailscaleCLI := getTailscaleCLIPath()

	if mapping.networkPort != mapping.localPort {
		fmt.Printf("Exposing localhost:%d as port %d on network...\n", mapping.localPort, mapping.networkPort)
	} else {
		fmt.Printf("Exposing port %d...\n", mapping.networkPort)
	}

	// Use tailscale serve to expose the port
	// Format: tailscale serve https:<network-port> / http://127.0.0.1:<local-port>
	var serveCmd *exec.Cmd
	localURL := fmt.Sprintf("http://127.0.0.1:%d", mapping.localPort)

	// If network port differs from local, we need to specify the port in serve
	var serveArgs []string
	if mapping.networkPort != mapping.localPort && mapping.networkPort != 443 {
		// Use specific port syntax: tailscale serve --https=<port> --bg <local-url>
		serveArgs = []string{"serve", fmt.Sprintf("--https=%d", mapping.networkPort), "--bg", localURL}
	} else if mapping.networkPort == 443 {
		// Default HTTPS port
		serveArgs = []string{"serve", "--bg", localURL}
	} else {
		// Same port - use direct syntax
		serveArgs = []string{"serve", "--bg", localURL}
	}

	if platform.IsWindows() {
		serveCmd = exec.Command(tailscaleCLI, serveArgs...)
	} else {
		serveCmd = exec.Command("sudo", append([]string{tailscaleCLI}, serveArgs...)...)
	}

	output, err := serveCmd.CombinedOutput()
	if err != nil {
		// Check if it's just a "already serving" type message
		outputStr := string(output)
		if strings.Contains(outputStr, "already") {
			fmt.Printf("Port %d is already exposed.\n", mapping.networkPort)
			return
		}
		fmt.Fprintf(os.Stderr, "Error exposing port: %s\n", outputStr)
		os.Exit(1)
	}

	// Get the HTTPS URL
	statusCmd := exec.Command(tailscaleCLI, "serve", "status", "--json")
	statusOutput, _ := statusCmd.Output()

	fmt.Printf("Port mapping %d -> localhost:%d is now active!\n\n", mapping.networkPort, mapping.localPort)
	fmt.Println("Access URLs:")

	// Show the Tailscale IP access
	ipCmd := exec.Command(tailscaleCLI, "ip", "-4")
	if ipOutput, err := ipCmd.Output(); err == nil {
		ip := strings.TrimSpace(string(ipOutput))
		fmt.Printf("  http://%s:%d -> localhost:%d\n", ip, mapping.networkPort, mapping.localPort)
	}

	// Parse serve status for HTTPS URL
	if len(statusOutput) > 0 {
		var serveStatus map[string]any
		if json.Unmarshal(statusOutput, &serveStatus) == nil {
			// Try to extract the serve URL
			if web, ok := serveStatus["Web"].(map[string]any); ok {
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

// getTailscaleCLIPath returns the path to the tailscale CLI
func getTailscaleCLIPath() string {
	if platform.IsWindows() {
		fullPath := `C:\Program Files\Tailscale\tailscale.exe`
		if _, err := os.Stat(fullPath); err == nil {
			return fullPath
		}
	}
	return "tailscale"
}

func init() {
	rootCmd.AddCommand(exposeCmd)
	exposeCmd.Flags().StringVar(&exposeService, "service", "", "Expose a known service by name (vllm, ollama, llamacpp, lmstudio)")
	exposeCmd.Flags().BoolVar(&exposeList, "list", false, "List currently exposed ports")
	exposeCmd.Flags().BoolVar(&exposeRemove, "remove", false, "Stop exposing a port")
	exposeCmd.Flags().IntVar(&exposeLocalPort, "to", 0, "Local port to forward to (e.g., --to 8000 forwards to localhost:8000)")
}
