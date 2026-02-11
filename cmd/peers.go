// cmd/peers.go
package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/discovery"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	peersCapability   string
	peersOnlineOnly   bool
	peersIncludeStats bool
)

var peersCmd = &cobra.Command{
	Use:   "peers",
	Short: "Discover peer nodes in your organization",
	Long: `Queries the AceTeam service discovery API to find peer nodes
in your organization. Optionally filter by capability tags.

Examples:
  # List all peers
  citadel peers

  # Find peers with GPU capabilities
  citadel peers --capability gpu:a100

  # Find online peers with LLM models, include hardware stats
  citadel peers --capability llm:llama3 --online-only --stats`,
	Run: runPeers,
}

func runPeers(cmd *cobra.Command, args []string) {
	// Resolve API key and base URL
	apiKey := os.Getenv("CITADEL_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "Error: CITADEL_API_KEY environment variable is required.")
		fmt.Fprintln(os.Stderr, "  Set it to your AceTeam device API key.")
		os.Exit(1)
	}

	baseURL := os.Getenv("ACETEAM_URL")
	if baseURL == "" {
		baseURL = os.Getenv("HEARTBEAT_URL") // backward compat
	}
	if baseURL == "" {
		baseURL = "https://aceteam.ai"
	}

	client := discovery.NewClient(discovery.ClientConfig{
		BaseURL: baseURL,
		APIKey:  apiKey,
	})

	// Parse capabilities
	var capabilities []string
	if peersCapability != "" {
		capabilities = strings.Split(peersCapability, ",")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	nodes, err := client.DiscoverNodes(ctx, capabilities, peersIncludeStats, peersOnlineOnly)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error discovering peers: %v\n", err)
		os.Exit(1)
	}

	if len(nodes) == 0 {
		if len(capabilities) > 0 {
			fmt.Printf("No peers found matching capabilities: %s\n", strings.Join(capabilities, ", "))
		} else {
			fmt.Println("No peers found in your organization.")
		}
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)

	if peersIncludeStats {
		fmt.Fprintln(w, "NAME\tSTATUS\tIP\tCAPABILITIES\tCPU\tMEM\tGPU")
		fmt.Fprintln(w, "----\t------\t--\t------------\t---\t---\t---")
	} else {
		fmt.Fprintln(w, "NAME\tSTATUS\tIP\tCAPABILITIES")
		fmt.Fprintln(w, "----\t------\t--\t------------")
	}

	goodColor := color.New(color.FgGreen)
	badColor := color.New(color.FgRed)

	for _, node := range nodes {
		statusStr := badColor.Sprint("OFFLINE")
		if node.Online {
			statusStr = goodColor.Sprint("ONLINE")
		}

		// Display first IP address
		ip := "-"
		if len(node.IPAddresses) > 0 {
			ip = node.IPAddresses[0]
		}

		// Format capabilities (show first 3, then +N more)
		capsStr := formatCapabilities(node.Capabilities, 3)

		if peersIncludeStats && node.Status != nil {
			cpuStr := "-"
			memStr := "-"
			gpuStr := "-"

			if node.Status.CPUUsage != nil {
				cpuStr = fmt.Sprintf("%.0f%%", *node.Status.CPUUsage)
			}
			if node.Status.MemoryUsage != nil {
				memStr = fmt.Sprintf("%.0f%%", *node.Status.MemoryUsage)
			}
			if node.Status.GPUUsage != nil {
				gpuStr = fmt.Sprintf("%.0f%%", *node.Status.GPUUsage)
			}

			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				node.GivenName, statusStr, ip, capsStr, cpuStr, memStr, gpuStr)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				node.GivenName, statusStr, ip, capsStr)
		}
	}

	w.Flush()
	fmt.Printf("\n%d peer(s) found", len(nodes))
	if len(capabilities) > 0 {
		fmt.Printf(" matching: %s", strings.Join(capabilities, ", "))
	}
	fmt.Println()
}

func formatCapabilities(caps []string, maxShow int) string {
	if len(caps) == 0 {
		return "(none)"
	}
	if len(caps) <= maxShow {
		return strings.Join(caps, ", ")
	}
	shown := strings.Join(caps[:maxShow], ", ")
	return fmt.Sprintf("%s (+%d more)", shown, len(caps)-maxShow)
}

func init() {
	rootCmd.AddCommand(peersCmd)
	peersCmd.Flags().StringVar(&peersCapability, "capability", "", "Filter by capability tags (comma-separated, e.g., gpu:a100,llm:llama3)")
	peersCmd.Flags().BoolVar(&peersOnlineOnly, "online-only", false, "Only show online peers")
	peersCmd.Flags().BoolVar(&peersIncludeStats, "stats", false, "Include hardware stats (CPU, memory, GPU usage)")
}
