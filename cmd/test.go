// cmd/test.go
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aceboss/citadel-cli/internal/nexus"
	"github.com/spf13/cobra"
)

var testService string

var testCmd = &cobra.Command{
	Use:   "test",
	Short: "Run a diagnostic test for a specific service",
	Long: `This command runs a pre-defined suite of mock jobs against a newly provisioned
service to ensure it is working correctly. It is typically called automatically
by 'bootstrap --test'.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("--- 🔬 Running diagnostic test for service: %s ---\n", testService)

		// 1. Define which job types belong to which service test
		serviceJobMap := map[string][]string{
			"llamacpp": {"DOWNLOAD_MODEL", "LLAMACPP_INFERENCE"},
			"ollama":   {"OLLAMA_PULL", "OLLAMA_INFERENCE"},
			"vllm":     {"VLLM_INFERENCE"},
			"none":     {},
		}

		requiredJobTypes, ok := serviceJobMap[testService]
		if !ok {
			fmt.Fprintf(os.Stderr, "❌ Unknown service '%s' for testing.\n", testService)
			os.Exit(1)
		}

		if len(requiredJobTypes) == 0 {
			fmt.Println("✅ No test required for network-only configuration.")
			os.Exit(0)
		}

		// 2. Load all mock jobs and filter them
		var allMockJobs []nexus.Job
		data, _ := nexus.MockJobsFS.ReadFile("mock_jobs.json")
		json.Unmarshal(data, &allMockJobs)

		var jobsToRun []*nexus.Job
		for _, job := range allMockJobs {
			for _, requiredType := range requiredJobTypes {
				if job.Type == requiredType {
					// Important: create a new variable for the pointer
					jobCopy := job
					jobsToRun = append(jobsToRun, &jobCopy)
					break
				}
			}
		}

		if len(jobsToRun) == 0 {
			fmt.Fprintf(os.Stderr, "⚠️ No mock jobs found for service '%s'.\n", testService)
			os.Exit(0)
		}

		// 3. Run the filtered jobs
		client := nexus.NewClient(nexusURL)
		jobFailed := false
		for i, job := range jobsToRun {
			fmt.Printf("   - Running test job %d of %d (Type: %s)\n", i+1, len(jobsToRun), job.Type)
			status, _ := executeJob(client, job)
			if status != "SUCCESS" {
				jobFailed = true
			}
			// Add a small delay between jobs
			time.Sleep(1 * time.Second)
		}

		// 4. Report final status
		if jobFailed {
			fmt.Println("\n--- ❌ Test failed. Please check the logs above for errors. ---")
			os.Exit(1)
		} else {
			fmt.Println("\n--- ✅ Test successful! The node is operating correctly. ---")
		}
	},
}

func init() {
	rootCmd.AddCommand(testCmd)
	testCmd.Flags().StringVar(&testService, "service", "", "The service to run tests for (e.g., vllm, ollama)")
	testCmd.MarkFlagRequired("service")
}
