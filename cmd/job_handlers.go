// cmd/job_handlers.go
// Job execution helpers used by test.go for diagnostic testing
package cmd

import (
	"fmt"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// A map to hold all our registered job handlers.
var jobHandlers map[string]jobs.JobHandler

// nodePasscodeVerifier verifies a presented passcode against the node's persisted
// bcrypt passcode (aceteam#6524). It reloads permissions on every call so a
// passcode set/rotated via APPLY_DEVICE_CONFIG or the control center takes effect
// without a worker restart, mirroring the gateway's per-request passcode gate.
// Wired into the SHELL_COMMAND handler so an ENABLED shell still fails closed
// unless the correct node passcode is presented. VerifyPasscode itself fails
// closed (no passcode set, or empty/wrong pin, returns false).
func nodePasscodeVerifier(pin string) bool {
	return config.LoadPermissions(platform.ConfigDir()).VerifyPasscode(pin)
}

// executeJob finds the right handler and runs a job.
func executeJob(client *nexus.Client, job *nexus.Job) (string, error) {
	var output []byte
	var err error
	var status string

	handler, ok := jobHandlers[job.Type]
	if !ok {
		err = fmt.Errorf("unsupported job type: %s", job.Type)
	} else {
		jobCtx := jobs.JobContext{}
		output, err = handler.Execute(jobCtx, job)
	}

	if err != nil {
		status = "FAILURE"
		errorMsg := fmt.Sprintf("Execution Error: %v", err)
		// Combine the error and any command output for a full report
		if len(output) > 0 {
			errorMsg = fmt.Sprintf("%s\n---\n%s", errorMsg, string(output))
		}
		output = []byte(errorMsg)
		fmt.Fprintf(os.Stderr, "     - [Job %s] ❌ Execution failed: %v\n", job.ID, err)
	} else {
		status = "SUCCESS"
		fmt.Printf("     - [Job %s] ✅ Execution successful.\n", job.ID)
	}

	update := nexus.JobStatusUpdate{
		Status: status,
		Output: string(output),
	}

	if reportErr := client.UpdateJobStatus(job.ID, update); reportErr != nil {
		fmt.Fprintf(os.Stderr, "     - [Job %s] ⚠️ CRITICAL: Failed to report status back to Nexus: %v\n", job.ID, reportErr)
	}
	return status, err
}

func init() {
	// Honor the same default-deny kill-switch as the worker path: SHELL_COMMAND
	// is refused unless the node has explicitly opted in via the persisted
	// `shell` permission. Without this the legacy Nexus/diagnostic path would
	// run commands as root regardless of the permission (aceteam #6149, Phase 0).
	shellHandler := jobs.NewShellCommandHandler("")
	shellHandler.Disabled = !config.LoadPermissions(platform.ConfigDir()).Shell
	// Even on this legacy Nexus/diagnostic path an enabled shell is passcode-gated
	// (aceteam#6524): executeJob polls remote jobs and reports back, so leaving the
	// verifier nil here would run enabled shell with no passcode. Fail closed.
	shellHandler.VerifyPasscode = nodePasscodeVerifier

	// Register all job handlers for test command
	jobHandlers = map[string]jobs.JobHandler{
		"SHELL_COMMAND":        shellHandler,
		"TMUX_SESSION":         jobs.NewTmuxSessionHandler(""),
		"DOWNLOAD_MODEL":       &jobs.DownloadModelHandler{},
		"OLLAMA_PULL":          &jobs.OllamaPullHandler{},
		"LLAMACPP_INFERENCE":   &jobs.LlamaCppInferenceHandler{},
		"VLLM_INFERENCE":       &jobs.VLLMInferenceHandler{},
		"OLLAMA_INFERENCE":     &jobs.OllamaInferenceHandler{},
		"embedding":            &jobs.EmbeddingHandler{},
		"SANDBOX_SUSPEND":      &jobs.SandboxSuspendHandler{},
		"SANDBOX_RESUME":       &jobs.SandboxResumeHandler{},
		"MODEL_CACHE_PULL":     &jobs.ModelCachePullHandler{},
		"MODEL_CACHE_EVICT":    &jobs.ModelCacheEvictHandler{},
		"IOS_BUILD":            jobs.NewIOSBuildHandler(""),
		"ANDROID_BUILD":        jobs.NewAndroidBuildHandler(""),
		"GOMOBILE_BUILD":       jobs.NewGomobileBuildHandler(""),
		"COBROWSE":             jobs.NewCobrowseHandler(),
		"FILE_INDEX":           jobs.NewFileIndexHandler("", ""),
		"FILE_SEMANTIC_SEARCH": jobs.NewFileSemanticSearchHandler("", ""),
		"HTTP_PROXY":           &jobs.HTTPProxyHandler{},
		"WEB_FETCH":            &jobs.WebFetchHandler{},
	}
}
