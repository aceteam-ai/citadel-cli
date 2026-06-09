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

// LegacyHandlerOpts configures optional behaviour for CreateLegacyHandlers.
type LegacyHandlerOpts struct {
	// LogFn routes job log output through a callback instead of stdout.
	LogFn func(level, msg string)
	// WorkspaceDir is the sandbox root for file-operation handlers.
	// If empty, file-operation handlers are not registered.
	WorkspaceDir string
}

// CreateLegacyHandlers creates JobHandler adapters for all existing Nexus job handlers.
// If logFn is provided, job output will be routed through it instead of stdout.
func CreateLegacyHandlers(logFn ...func(level, msg string)) []JobHandler {
	var fn func(level, msg string)
	if len(logFn) > 0 {
		fn = logFn[0]
	}
	return CreateLegacyHandlersWithOpts(LegacyHandlerOpts{LogFn: fn})
}

// CreateLegacyHandlersWithOpts is like CreateLegacyHandlers but accepts a
// structured options value for richer configuration (e.g. workspace directory
// for file-operation handlers).
func CreateLegacyHandlersWithOpts(opts LegacyHandlerOpts) []JobHandler {
	handlers := []*LegacyHandlerAdapter{
		NewLegacyHandlerAdapter(JobTypeShellCommand, &jobs.ShellCommandHandler{}),
		NewLegacyHandlerAdapter(JobTypeDownloadModel, &jobs.DownloadModelHandler{}),
		NewLegacyHandlerAdapter(JobTypeOllamaPull, &jobs.OllamaPullHandler{}),
		NewLegacyHandlerAdapter(JobTypeLlamaCppInference, &jobs.LlamaCppInferenceHandler{}),
		NewLegacyHandlerAdapter(JobTypeVLLMInference, &jobs.VLLMInferenceHandler{}),
		NewLegacyHandlerAdapter(JobTypeOllamaInference, &jobs.OllamaInferenceHandler{}),
		NewLegacyHandlerAdapter(JobTypeApplyDeviceConfig, jobs.NewConfigHandler("")),
		NewLegacyHandlerAdapter(JobTypeExtraction, &jobs.ExtractionHandler{}),
		NewLegacyHandlerAdapter(JobTypeHTTPProxy, &jobs.HTTPProxyHandler{}),
	}

	// Register file-operation handlers when a workspace is configured.
	if opts.WorkspaceDir != "" {
		handlers = append(handlers,
			NewLegacyHandlerAdapter(JobTypeFileRead, jobs.NewFileReadHandler(opts.WorkspaceDir)),
			NewLegacyHandlerAdapter(JobTypeFileWrite, jobs.NewFileWriteHandler(opts.WorkspaceDir)),
			NewLegacyHandlerAdapter(JobTypeFileEdit, jobs.NewFileEditHandler(opts.WorkspaceDir)),
			NewLegacyHandlerAdapter(JobTypeFileList, jobs.NewFileListHandler(opts.WorkspaceDir)),
			NewLegacyHandlerAdapter(JobTypeFileSearch, jobs.NewFileSearchHandler(opts.WorkspaceDir)),
		)
	}

	// Set log function on all handlers.
	if opts.LogFn != nil {
		for _, h := range handlers {
			h.SetLogFn(opts.LogFn)
		}
	}

	// Convert to []JobHandler.
	result := make([]JobHandler, len(handlers))
	for i, h := range handlers {
		result[i] = h
	}
	return result
}
