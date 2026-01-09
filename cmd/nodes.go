// cmd/nodes.go
/*
Copyright © 2025 Jason Sun <jason@aceteam.ai>
*/
package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/aceboss/citadel-cli/internal/nexus"
	"github.com/spf13/cobra"
)

// nodesCmd represents the nodes command
var nodesCmd = &cobra.Command{
	Use:     "nodes",
	Aliases: []string{"ls", "list"},
	Short:   "List all compute nodes in your AceTeam fabric",
	Long: `Connects to the Nexus control plane and retrieves a list of all
registered compute nodes, showing their status, IP address, and last-seen time.`,
	Run: func(cmd *cobra.Command, args []string) {
		// Use the global nexusURL flag from root.go
		client := nexus.NewClient(nexusURL)

		fmt.Println("--- Fetching nodes from Nexus... ---")
		nodes, err := client.ListNodes()
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error fetching nodes: %v\n", err)
			os.Exit(1)
		}

		if len(nodes) == 0 {
			fmt.Println("🤷 No nodes found in your fabric.")
			return
		}

		// Use a tabwriter for nicely formatted, aligned columns
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "NAME\tSTATUS\tIP ADDRESS\tLAST SEEN")
		fmt.Fprintln(w, "----\t------\t----------\t---------")

		for _, node := range nodes {
			status := "🟢 ONLINE"
			if node.Status != "online" {
				status = "🔴 OFFLINE"
			}
			// Format the "Last Seen" time to be human-readable
			lastSeen := time.Since(node.LastSeen).Round(time.Second).String() + " ago"
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", node.Name, status, node.IPAddress, lastSeen)
		}

		w.Flush()
	},
}

func init() {
	rootCmd.AddCommand(nodesCmd)
}
