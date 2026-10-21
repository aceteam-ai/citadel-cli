package cmd

import "github.com/aceteam-ai/citadel-cli/internal/workflow"

// workflowHTTPPolicy is shared by both worker entrypoints. Tests supply an
// explicit temporary directory; this core never resolves machine state itself.
func workflowHTTPPolicy(nodeConfigDir string) workflow.HTTPPolicy {
	return workflow.LoadHTTPPolicy(nodeConfigDir)
}
