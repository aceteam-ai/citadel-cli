// cmd/test.go
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aceboss/citadel-cli/internal/nexus"
	"github.com/aceboss/citadel-cli/internal/ui"
	"github.com/spf13/cobra"
)

var testService string

var testCmd = &cobra.Command{
	Use:   "test",
	Short: "Run a diagnostic test for a specific service",
	Long: `This command runs a pre-defined suite of mock jobs against a newly provisioned
service to ensure it is working correctly. It is typically called automatically
by 'init --test'.`,
	Run: func(cmd *cobra.Command, args []string) {
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
		fmt.Println("[DEBUG] POST: Loading test suite from mock_jobs.json...")
		var allMockJobs []nexus.Job
		data, _ := nexus.MockJobsFS.ReadFile("mock_jobs.json")
		json.Unmarshal(data, &allMockJobs)

		var jobsToRun []*nexus.Job
		for _, job := range allMockJobs {
			for _, requiredType := range requiredJobTypes {
				if job.Type == requiredType {
					jobCopy := job
					jobsToRun = append(jobsToRun, &jobCopy)
					break
				}
			}
		}
		fmt.Printf("[DEBUG] POST: Filtering test suite for '%s' jobs. Found %d relevant tests.\n", testService, len(jobsToRun))

		if len(jobsToRun) == 0 {
			fmt.Fprintf(os.Stderr, "⚠️ No mock jobs found for service '%s'.\n", testService)
			os.Exit(0)
		}

		// 3. Run the filtered jobs
		client := nexus.NewClient(nexusURL)
		status := ui.NewStatusLine()
		jobFailed := false

		fmt.Println()
		status.Info(fmt.Sprintf("Running %d test(s) for %s", len(jobsToRun), testService))
		fmt.Println()

		for i, job := range jobsToRun {
			status.Step(i+1, len(jobsToRun), fmt.Sprintf("Running %s", job.Type))
			result, _ := executeJob(client, job, status)
			if result != "SUCCESS" {
				jobFailed = true
			}
			time.Sleep(1 * time.Second)
		}

		// 4. Report final status
		fmt.Println()
		if jobFailed {
			status.Fail("Test failed - check logs above for errors")
			os.Exit(1)
		} else {
			status.Success("All tests passed - node is verified and ready")
			os.Exit(0)
		}
	},
}

func init() {
	rootCmd.AddCommand(testCmd)
	testCmd.Flags().StringVar(&testService, "service", "", "The service to run tests for (e.g., vllm, ollama)")
	testCmd.MarkFlagRequired("service")
}
