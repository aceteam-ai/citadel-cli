package worker

import (
	"context"
	"fmt"
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

	// Convert payload map[string]any to map[string]string for nexus.Job.
	// Redis payloads arrive via json.Unmarshal, so numbers are float64 and
	// booleans are bool. Coerce all values to strings so handlers can parse
	// them with strconv.
	if job.Payload != nil {
		nexusJob.Payload = make(map[string]string)
		for k, v := range job.Payload {
			switch val := v.(type) {
			case string:
				nexusJob.Payload[k] = val
			case nil:
				// skip nil values
			default:
				nexusJob.Payload[k] = fmt.Sprint(val)
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
	// ConfigDir is the path to the citadel.yaml manifest directory.
	// If empty, service-management handlers are not registered.
	ConfigDir string
	// AllowReadOutsideWorkspace, when true, lets read-only file handlers
	// (FILE_READ, FILE_READ_BYTES, FILE_LIST, FILE_SEARCH) access paths
	// outside the workspace sandbox. Write handlers are unaffected.
	AllowReadOutsideWorkspace bool
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
		NewLegacyHandlerAdapter(JobTypeShellCommand, jobs.NewShellCommandHandler(opts.WorkspaceDir)),
		NewLegacyHandlerAdapter(JobTypeTmuxSession, jobs.NewTmuxSessionHandler("")),
		NewLegacyHandlerAdapter(JobTypeDownloadModel, &jobs.DownloadModelHandler{}),
		NewLegacyHandlerAdapter(JobTypeOllamaPull, &jobs.OllamaPullHandler{}),
		NewLegacyHandlerAdapter(JobTypeLlamaCppInference, &jobs.LlamaCppInferenceHandler{}),
		NewLegacyHandlerAdapter(JobTypeVLLMInference, &jobs.VLLMInferenceHandler{}),
		NewLegacyHandlerAdapter(JobTypeOllamaInference, &jobs.OllamaInferenceHandler{}),
		NewLegacyHandlerAdapter(JobTypeApplyDeviceConfig, jobs.NewConfigHandler("")),
		NewLegacyHandlerAdapter(JobTypeExtraction, &jobs.ExtractionHandler{}),
		NewLegacyHandlerAdapter(JobTypeHTTPProxy, &jobs.HTTPProxyHandler{}),
		NewLegacyHandlerAdapter(JobTypeSandboxSuspend, &jobs.SandboxSuspendHandler{}),
		NewLegacyHandlerAdapter(JobTypeSandboxResume, &jobs.SandboxResumeHandler{}),
		NewLegacyHandlerAdapter(JobTypeModelCachePull, &jobs.ModelCachePullHandler{}),
		NewLegacyHandlerAdapter(JobTypeModelCacheEvict, &jobs.ModelCacheEvictHandler{}),
		NewLegacyHandlerAdapter(JobTypeIOSBuild, jobs.NewIOSBuildHandler(opts.WorkspaceDir)),
		NewLegacyHandlerAdapter(JobTypeAndroidBuild, jobs.NewAndroidBuildHandler(opts.WorkspaceDir)),
		NewLegacyHandlerAdapter(JobTypeGomobileBuild, jobs.NewGomobileBuildHandler(opts.WorkspaceDir)),
		// Desktop capture/input handlers (issue #4179). Registered
		// unconditionally: screenshot/type/keys need no workspace sandbox, and
		// gating them behind WorkspaceDir would leave the desktop_screenshot /
		// vnc_* MCP tools timing out on any node without a configured workspace.
		// FILE_SCREENSHOT and VNC_SCREENSHOT share one capture path (the
		// existing internal/desktop X11 capture); there is no separate VNC
		// framebuffer source.
		NewLegacyHandlerAdapter(JobTypeFileScreenshot, &jobs.ScreenshotHandler{}),
		NewLegacyHandlerAdapter(JobTypeVNCScreenshot, &jobs.ScreenshotHandler{}),
		NewLegacyHandlerAdapter(JobTypeVNCType, &jobs.TypeHandler{}),
		NewLegacyHandlerAdapter(JobTypeVNCKeys, &jobs.KeysHandler{}),
		// VNC_ACTIONS exposes the pointer/keyboard primitives shipped in #314
		// (move/click/mousedown/mouseup/scroll, including the drag sequence) over
		// the fabric Redis transport so the aceteam desktop_click / desktop_drag
		// MCP tools can drive a node end to end (issue #4180). Same unconditional
		// registration rationale as the screenshot/type/keys handlers above.
		NewLegacyHandlerAdapter(JobTypeVNCActions, &jobs.ActionsHandler{}),
		NewLegacyHandlerAdapter(JobTypeCobrowse, jobs.NewCobrowseHandler()),
	}

	// Register file-operation handlers when a workspace is configured.
	if opts.WorkspaceDir != "" {
		readHandler := jobs.NewFileReadHandler(opts.WorkspaceDir)
		readHandler.AllowOutsideWorkspace = opts.AllowReadOutsideWorkspace

		readBytesHandler := jobs.NewFileReadBytesHandler(opts.WorkspaceDir)
		readBytesHandler.AllowOutsideWorkspace = opts.AllowReadOutsideWorkspace

		listHandler := jobs.NewFileListHandler(opts.WorkspaceDir)
		listHandler.AllowOutsideWorkspace = opts.AllowReadOutsideWorkspace

		searchHandler := jobs.NewFileSearchHandler(opts.WorkspaceDir)
		searchHandler.AllowOutsideWorkspace = opts.AllowReadOutsideWorkspace

		handlers = append(handlers,
			NewLegacyHandlerAdapter(JobTypeFileRead, readHandler),
			NewLegacyHandlerAdapter(JobTypeFileReadBytes, readBytesHandler),
			NewLegacyHandlerAdapter(JobTypeFileWrite, jobs.NewFileWriteHandler(opts.WorkspaceDir)),
			NewLegacyHandlerAdapter(JobTypeFileWriteBytes, jobs.NewFileWriteBytesHandler(opts.WorkspaceDir)),
			NewLegacyHandlerAdapter(JobTypeFileEdit, jobs.NewFileEditHandler(opts.WorkspaceDir)),
			NewLegacyHandlerAdapter(JobTypeFileList, listHandler),
			NewLegacyHandlerAdapter(JobTypeFileSearch, searchHandler),
			NewLegacyHandlerAdapter(JobTypeFileList, jobs.NewFileListHandler(opts.WorkspaceDir)),
			NewLegacyHandlerAdapter(JobTypeFileSearch, jobs.NewFileSearchHandler(opts.WorkspaceDir)),
			// Node-local meeting transcription (faster-whisper sidecar). Registered
			// with the workspace so it can validate audio paths the same way the
			// file handlers do.
			NewLegacyHandlerAdapter(JobTypeTranscribeAudio, jobs.NewTranscribeAudioHandler(opts.WorkspaceDir)),
		)
	}

	// Register service-management handlers when a config directory is available.
	if opts.ConfigDir != "" {
		svcHandler := jobs.NewServiceHandler(opts.ConfigDir)
		handlers = append(handlers,
			NewLegacyHandlerAdapter(JobTypeServiceStart, svcHandler),
			NewLegacyHandlerAdapter(JobTypeServiceStop, svcHandler),
			NewLegacyHandlerAdapter(JobTypeServiceStatus, svcHandler),
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
