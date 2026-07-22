package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
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
	//
	// Scalars keep their fmt.Sprint form (so existing handlers' strconv/bool
	// parsing is unchanged). NESTED values (a JSON object or array -- e.g. the
	// SERVICE_START "env" map, citadel-cli#462) are json-encoded instead of
	// fmt.Sprint'd: fmt.Sprint on a map yields the unparseable Go form
	// "map[K:v]", which loses the structure. This is a shared chokepoint for
	// every legacy handler, so the change is deliberately limited to maps/slices.
	if job.Payload != nil {
		nexusJob.Payload = make(map[string]string)
		for k, v := range job.Payload {
			switch val := v.(type) {
			case string:
				nexusJob.Payload[k] = val
			case nil:
				// skip nil values
			case map[string]any, []any:
				if encoded, err := json.Marshal(val); err == nil {
					nexusJob.Payload[k] = string(encoded)
				} else {
					nexusJob.Payload[k] = fmt.Sprint(val)
				}
			default:
				nexusJob.Payload[k] = fmt.Sprint(val)
			}
		}
	}

	// Execute the legacy handler with log callback. Thread the worker context so
	// handlers that shell out (e.g. SHELL_COMMAND) honor a per-job deadline or
	// cancellation and actually terminate their child process (aceteam#6000).
	jobCtx := jobs.JobContext{LogFn: a.logFn, Ctx: ctx}
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
	// ShellDisabled, when true, registers the SHELL_COMMAND handler in a
	// refusing state: it is still dispatchable (so the node returns the
	// "disabled" error rather than "unsupported job type"), but every command
	// is rejected. Wired from the persisted `shell` node permission.
	ShellDisabled bool
	// MeetingProfileDir overrides the persistent, signed-in bot Chrome profile
	// directory used by MEETING_JOIN (issue #5122). If empty, the handler falls
	// back to platform.EnvMeetingProfileDir, then platform's ConfigDir()-rooted
	// default — see jobs.MeetingJoinHandler.ProfileDir and
	// docs/meeting-bot-profile-seeding.md.
	MeetingProfileDir string
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
	shellHandler := jobs.NewShellCommandHandler(opts.WorkspaceDir)
	shellHandler.Disabled = opts.ShellDisabled

	handlers := []*LegacyHandlerAdapter{
		NewLegacyHandlerAdapter(JobTypeShellCommand, shellHandler),
		NewLegacyHandlerAdapter(JobTypeTmuxSession, jobs.NewTmuxSessionHandler("")),
		NewLegacyHandlerAdapter(JobTypeDownloadModel, &jobs.DownloadModelHandler{}),
		NewLegacyHandlerAdapter(JobTypeOllamaPull, &jobs.OllamaPullHandler{}),
		NewLegacyHandlerAdapter(JobTypeLlamaCppInference, &jobs.LlamaCppInferenceHandler{}),
		NewLegacyHandlerAdapter(JobTypeVLLMInference, &jobs.VLLMInferenceHandler{}),
		NewLegacyHandlerAdapter(JobTypeOllamaInference, &jobs.OllamaInferenceHandler{}),
		// Text-embedding jobs route to the local TEI service's OpenAI-compatible
		// /v1/embeddings endpoint (issue #351). Registered unconditionally — like
		// the other inference engines it needs no workspace sandbox; nodes that
		// don't run TEI simply never receive `embedding` jobs (they only land on
		// nodes carrying the task:embedding capability tag).
		NewLegacyHandlerAdapter(JobTypeEmbedding, &jobs.EmbeddingHandler{}),
		NewLegacyHandlerAdapter(JobTypeApplyDeviceConfig, jobs.NewConfigHandler("")),
		NewLegacyHandlerAdapter(JobTypeExtraction, &jobs.ExtractionHandler{}),
		NewLegacyHandlerAdapter(JobTypeHTTPProxy, &jobs.HTTPProxyHandler{}),
		// WEB_FETCH is the SSRF-guarded successor to HTTP_PROXY (aceteam#5995).
		// HTTP_PROXY stays registered for WeChat provisioning + fleet back-compat;
		// new consumers (youtube egress, web tools) use WEB_FETCH.
		NewLegacyHandlerAdapter(JobTypeWebFetch, &jobs.WebFetchHandler{}),
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
		// Resource snapshot (issue #427): returns the node's full GPU/host
		// resource-consumer picture over the fabric. Registered unconditionally
		// like the inference handlers — it needs no workspace sandbox, and gating
		// it would leave the backend's node-resource pull timing out on nodes
		// without a configured workspace.
		NewLegacyHandlerAdapter(JobTypeResourceSnapshot, &jobs.ResourceSnapshotHandler{}),
		// Node-local speech synthesis (kokoro TTS sidecar, aceteam#6104). The
		// synthesis counterpart to TRANSCRIBE_AUDIO, but registered
		// UNCONDITIONALLY like the other inference handlers: it takes text inline
		// and returns audio inline (base64), touching no workspace file, so
		// gating it behind WorkspaceDir would needlessly bar workspace-less nodes
		// from TTS. Nodes that don't run the kokoro sidecar simply never receive
		// SYNTHESIZE_SPEECH jobs (they only land on nodes carrying the engine:tts
		// tag).
		NewLegacyHandlerAdapter(JobTypeSynthesizeSpeech, jobs.NewSynthesizeSpeechHandler()),
		// Turn delivery to a payload-launched BYOC instance (aceteam#5241).
		// Registered unconditionally: it resolves the target from its own
		// on-disk instance store (~/.citadel/instances/state.json, shared with
		// the service handler) rather than the citadel.yaml manifest, so it
		// needs neither a workspace nor a config dir. On a node that never
		// launched an instance the store lookup misses and the handler fails
		// closed rather than mis-delivering.
		NewLegacyHandlerAdapter(JobTypeInstanceMessage, jobs.NewInstanceMessageHandler()),
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

		// Node-local semantic index (aceteam#6087). FILE_INDEX walks the
		// workspace and (re)embeds changed files via the node's TEI service;
		// FILE_SEMANTIC_SEARCH runs a KNN over that index. The DB path is
		// resolved by the handler (CITADEL_INDEX_DB or index.db beside the
		// workspace). FILE_INDEX honors the read-outside-workspace relaxation
		// like the other read handlers.
		indexHandler := jobs.NewFileIndexHandler(opts.WorkspaceDir, "")
		indexHandler.AllowOutsideWorkspace = opts.AllowReadOutsideWorkspace

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
			NewLegacyHandlerAdapter(JobTypeFileIndex, indexHandler),
			NewLegacyHandlerAdapter(JobTypeFileSemanticSearch, jobs.NewFileSemanticSearchHandler(opts.WorkspaceDir, "")),
			// Node-local meeting transcription (faster-whisper sidecar). Registered
			// with the workspace so it can validate audio paths the same way the
			// file handlers do.
			NewLegacyHandlerAdapter(JobTypeTranscribeAudio, jobs.NewTranscribeAudioHandler(opts.WorkspaceDir)),
		)

		// Auto-join meeting notetaker (aceteam#5098): records the call into a
		// per-meeting null sink, then transcribes it via the handler above. Needs
		// the workspace both to write the recording and to validate the audio path
		// for transcription, so it lives inside this workspace gate. Gated on the
		// persisted meeting toggle (default-on, the house opt-out convention) so
		// the handler registers unless the operator has explicitly opted out —
		// matching the `meeting` capability advertisement gate in
		// internal/capabilities.
		if config.LoadMeeting(platform.ConfigDir()).MeetingEnabled {
			handlers = append(handlers,
				NewLegacyHandlerAdapter(JobTypeMeetingJoin, newMeetingJoinHandler(opts)),
			)
		}
	}

	// Register service-management handlers when a config directory is available.
	if opts.ConfigDir != "" {
		svcHandler := jobs.NewServiceHandlerWithWorkspace(opts.ConfigDir, opts.WorkspaceDir)
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

// newMeetingJoinHandler wires opts.MeetingProfileDir into the handler so a
// node's startup config can pin the persistent, signed-in bot Chrome profile
// (issue #5122) without relying solely on the platform.EnvMeetingProfileDir
// env var. Leaving it unset (the common case) preserves the handler's own
// env-var-then-ConfigDir()-default resolution.
func newMeetingJoinHandler(opts LegacyHandlerOpts) *jobs.MeetingJoinHandler {
	h := jobs.NewMeetingJoinHandler(opts.WorkspaceDir)
	h.ProfileDir = opts.MeetingProfileDir
	// Wire the during-call interactive layer (issue #5435) from the persisted
	// meeting config (default-on, opt-out — same house convention as the meeting
	// capability itself). The config accessors clamp non-positive cadence values
	// to sane defaults.
	m := config.LoadMeeting(platform.ConfigDir())
	h.StreamingEnabled = m.StreamingEnabled
	h.StreamingInterval = m.StreamingInterval()
	h.StreamingWindow = m.StreamingWindow()

	// Sovereign audio backup (aceteam#5097): default-on Opus upload + local
	// retention. Creds (device token + API base) are loaded FRESH on each upload
	// via the closure so a token rotated by the worker's in-place reauth is
	// honored — not frozen at handler construction.
	h.SetAudioBackup(m.AudioBackupEnabled, m.RetentionAge(), func() (string, string) {
		creds := config.LoadDeviceCreds(platform.ConfigDir())
		return creds.APIBaseURL, creds.Token
	})
	return h
}
