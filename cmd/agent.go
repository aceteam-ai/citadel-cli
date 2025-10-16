// cmd/agent.go
package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// agentCmd represents the agent command
var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Run the Citadel agent to listen for jobs from the Nexus",
	Long: `This is a long-running command that connects to the AceTeam Nexus
and waits for remote jobs to execute on this node. It should typically be
run as a background service.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("--- 🚀 Starting Citadel Agent ---")
		fmt.Printf("   - Connecting to Nexus at %s...\n", nexusURL)

		// --- Placeholder for gRPC Connection ---
		// nexusClient, err := nexus.NewClient(nexusURL)
		// if err != nil {
		//   fmt.Fprintf(os.Stderr, "❌ Could not connect to Nexus: %v\n", err)
		//   os.Exit(1)
		// }
		// fmt.Println("   - ✅ Connection established. Listening for jobs.")
		// -----------------------------------------

		// Create a channel to listen for OS signals
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

		// This is the main agent loop.
		go func() {
			for {
				// --- Placeholder for Job Listening Logic ---
				// job, err := nexusClient.GetNextJob()
				// if err != nil {
				//   fmt.Fprintf(os.Stderr, "Error fetching job: %v. Retrying in 10s.\n", err)
				//   time.Sleep(10 * time.Second)
				//   continue
				// }
				//
				// if job != nil {
				//   fmt.Printf("Received job: %s\n", job.ID)
				//   go executeJob(job) // Execute in a goroutine to not block listening
				// }
				// -----------------------------------------

				// In lieu of a real connection, we just print a heartbeat.
				fmt.Println("   - Agent is alive, waiting for jobs...")
				time.Sleep(30 * time.Second)
			}
		}()

		// Wait for a termination signal
		<-sigs
		fmt.Println("\n---  shutting down agent ---")
		// nexusClient.Close()
		fmt.Println("   - ✅ Agent stopped.")
	},
}

func init() {
	rootCmd.AddCommand(agentCmd)
}
