package cmd

import "github.com/aceteam-ai/citadel-cli/internal/workflow"

// Both real worker entrypoints load only their node's machine-local policy.
func workflowLLMPolicy(nodeConfigDir string) workflow.LLMPolicy {
	return workflow.LoadLLMPolicy(nodeConfigDir)
}
