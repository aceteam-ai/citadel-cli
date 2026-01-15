package worker

import (
	"context"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// LegacyHandlerAdapter wraps a jobs.JobHandler to implement worker.JobHandler.
// This allows existing handlers to work with the new worker abstraction.
type LegacyHandlerAdapter struct {
	jobType string
	handler jobs.JobHandler
}

// NewLegacyHandlerAdapter creates an adapter for an existing job handler.
func NewLegacyHandlerAdapter(jobType string, handler jobs.JobHandler) *LegacyHandlerAdapter {
	return &LegacyHandlerAdapter{
		jobType: jobType,
		handler: handler,
	}
}

// CanHandle returns true if this adapter handles the given job type.
func (a *LegacyHandlerAdapter) CanHandle(jobType string) bool {
	return a.jobType == jobType
}

// Execute processes the job using the wrapped legacy handler.
func (a *LegacyHandlerAdapter) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	start := time.Now()

	// Convert worker.Job to nexus.Job for the legacy handler
	nexusJob := &nexus.Job{
		ID:   job.ID,
		Type: job.Type,
	}

	// Convert payload map[string]any to map[string]string for nexus.Job
	if job.Payload != nil {
		nexusJob.Payload = make(map[string]string)
		for k, v := range job.Payload {
			if strVal, ok := v.(string); ok {
				nexusJob.Payload[k] = strVal
			}
		}
	}

	// Execute the legacy handler
	jobCtx := jobs.JobContext{}
	output, err := a.handler.Execute(jobCtx, nexusJob)

	duration := time.Since(start)

	if err != nil {
		return &JobResult{
			Status:   JobStatusFailure,
			Error:    err,
			Duration: duration,
			Output: map[string]any{
				"error":  err.Error(),
				"output": string(output),
			},
		}, err
	}

	return &JobResult{
		Status:   JobStatusSuccess,
		Duration: duration,
		Output: map[string]any{
			"output": string(output),
		},
	}, nil
}

// Ensure LegacyHandlerAdapter implements JobHandler
var _ JobHandler = (*LegacyHandlerAdapter)(nil)

// CreateLegacyHandlers creates JobHandler adapters for all existing Nexus job handlers.
func CreateLegacyHandlers() []JobHandler {
	return []JobHandler{
		NewLegacyHandlerAdapter(JobTypeShellCommand, &jobs.ShellCommandHandler{}),
		NewLegacyHandlerAdapter(JobTypeDownloadModel, &jobs.DownloadModelHandler{}),
		NewLegacyHandlerAdapter(JobTypeOllamaPull, &jobs.OllamaPullHandler{}),
		NewLegacyHandlerAdapter(JobTypeLlamaCppInference, &jobs.LlamaCppInferenceHandler{}),
		NewLegacyHandlerAdapter(JobTypeVLLMInference, &jobs.VLLMInferenceHandler{}),
		NewLegacyHandlerAdapter(JobTypeOllamaInference, &jobs.OllamaInferenceHandler{}),
	}
}
