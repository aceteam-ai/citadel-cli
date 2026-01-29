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
	logFn   func(level, msg string)
}

// NewLegacyHandlerAdapter creates an adapter for an existing job handler.
func NewLegacyHandlerAdapter(jobType string, handler jobs.JobHandler) *LegacyHandlerAdapter {
	return &LegacyHandlerAdapter{
		jobType: jobType,
		handler: handler,
	}
}

// SetLogFn sets the logging callback for routing job output through TUI.
func (a *LegacyHandlerAdapter) SetLogFn(logFn func(level, msg string)) {
	a.logFn = logFn
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

	// Execute the legacy handler with log callback
	jobCtx := jobs.JobContext{LogFn: a.logFn}
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
// If logFn is provided, job output will be routed through it instead of stdout.
func CreateLegacyHandlers(logFn ...func(level, msg string)) []JobHandler {
	var fn func(level, msg string)
	if len(logFn) > 0 {
		fn = logFn[0]
	}

	handlers := []*LegacyHandlerAdapter{
		NewLegacyHandlerAdapter(JobTypeShellCommand, &jobs.ShellCommandHandler{}),
		NewLegacyHandlerAdapter(JobTypeDownloadModel, &jobs.DownloadModelHandler{}),
		NewLegacyHandlerAdapter(JobTypeOllamaPull, &jobs.OllamaPullHandler{}),
		NewLegacyHandlerAdapter(JobTypeLlamaCppInference, &jobs.LlamaCppInferenceHandler{}),
		NewLegacyHandlerAdapter(JobTypeVLLMInference, &jobs.VLLMInferenceHandler{}),
		NewLegacyHandlerAdapter(JobTypeOllamaInference, &jobs.OllamaInferenceHandler{}),
		NewLegacyHandlerAdapter(JobTypeApplyDeviceConfig, jobs.NewConfigHandler("")),
	}

	// Set log function on all handlers
	if fn != nil {
		for _, h := range handlers {
			h.SetLogFn(fn)
		}
	}

	// Convert to []JobHandler
	result := make([]JobHandler, len(handlers))
	for i, h := range handlers {
		result[i] = h
	}
	return result
}
