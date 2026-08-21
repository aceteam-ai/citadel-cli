package cmd

import (
	"fmt"
	"github.com/spf13/cobra"
)

var snap worker.WorkerSnapshot

var doctorCmd = &cobra.Command{
	Use:     "doctor",
	Aliases: []string{"dr"},
	Short:   "Run health checks",
	RunE: func(cmd *cobra.Command, args []string) error {
		diagnostics := agentDoctor(snap)
		// Print all checks
		if checks, ok := diagnostics["checks"].([]map[string]any); ok {
			fmt.Println("Health Checks:")
			for _, check := range checks {
				status := "✓"
				if checkOk, exists := check["ok"].(bool); exists && !checkOk {
					status = "✗"
				}
				fmt.Printf("%s %s: %s\n", status, check["name"], check["detail"])
			}
		}
		// Print diagnosis
		if diagnosis, ok := diagnostics["diagnosis"].(string); ok {
			fmt.Printf("\nDiagnosis: %s\n", diagnosis)
		}
		// Exit non-zero if unhealthy
		if healthy, ok := diagnostics["healthy"].(bool); ok && !healthy {
			return fmt.Errorf("health check failed")
		}
		return nil
	},
}

func init() { // Adds the command to root command
	rootCmd.AddCommand(doctorCmd)
}