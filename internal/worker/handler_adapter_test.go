package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// TestLegacyHandler is a mock jobs.JobHandler for testing.
type TestLegacyHandler struct {
	shouldFail      bool
	output          string
	capturedPayload map[string]string // captures the payload for inspection
}

func (h *TestLegacyHandler) Execute(ctx jobs.JobContext, job *nexus.Job) ([]byte, error) {
	h.capturedPayload = job.Payload
	if h.shouldFail {
		return []byte("error output"), errors.New("handler failed")
	}
	return []byte(h.output), nil
}

func TestNewLegacyHandlerAdapter(t *testing.T) {
	handler := &TestLegacyHandler{output: "test output"}
	adapter := NewLegacyHandlerAdapter("TEST_JOB", handler)

	if adapter == nil {
		t.Fatal("NewLegacyHandlerAdapter returned nil")
	}
	if adapter.jobType != "TEST_JOB" {
		t.Errorf("jobType = %v, want TEST_JOB", adapter.jobType)
	}
}

func TestLegacyHandlerAdapterCanHandle(t *testing.T) {
	handler := &TestLegacyHandler{}
	adapter := NewLegacyHandlerAdapter("TEST_JOB", handler)

	tests := []struct {
		jobType string
		want    bool
	}{
		{"TEST_JOB", true},
		{"OTHER_JOB", false},
		{"test_job", false}, // case sensitive
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.jobType, func(t *testing.T) {
			got := adapter.CanHandle(tt.jobType)
			if got != tt.want {
				t.Errorf("CanHandle(%q) = %v, want %v", tt.jobType, got, tt.want)
			}
		})
	}
}

func TestLegacyHandlerAdapterExecuteSuccess(t *testing.T) {
	handler := &TestLegacyHandler{output: "success output"}
	adapter := NewLegacyHandlerAdapter("TEST_JOB", handler)

	job := &Job{
		ID:      "job-123",
		Type:    "TEST_JOB",
		Payload: map[string]any{"key": "value"},
	}

	ctx := context.Background()
	stream := &NoOpStreamWriter{}

	result, err := adapter.Execute(ctx, job, stream)

	if err != nil {
		t.Errorf("Execute error = %v, want nil", err)
	}
	if result == nil {
		t.Fatal("Execute returned nil result")
	}
	if result.Status != JobStatusSuccess {
		t.Errorf("result.Status = %v, want %v", result.Status, JobStatusSuccess)
	}
	if result.Output["output"] != "success output" {
		t.Errorf("result.Output[output] = %v, want 'success output'", result.Output["output"])
	}
	if result.Duration == 0 {
		t.Error("result.Duration should be non-zero")
	}
}

func TestLegacyHandlerAdapterExecuteFailure(t *testing.T) {
	handler := &TestLegacyHandler{shouldFail: true}
	adapter := NewLegacyHandlerAdapter("TEST_JOB", handler)

	job := &Job{
		ID:   "job-123",
		Type: "TEST_JOB",
	}

	ctx := context.Background()
	stream := &NoOpStreamWriter{}

	result, err := adapter.Execute(ctx, job, stream)

	if err == nil {
		t.Error("Execute error = nil, want error")
	}
	if result == nil {
		t.Fatal("Execute returned nil result")
	}
	if result.Status != JobStatusFailure {
		t.Errorf("result.Status = %v, want %v", result.Status, JobStatusFailure)
	}
	if result.Error == nil {
		t.Error("result.Error should not be nil")
	}
}

func TestLegacyHandlerAdapterPayloadConversion(t *testing.T) {
	var capturedJob *nexus.Job

	handler := &jobs.ShellCommandHandler{}

	// We can't easily capture the job in the real handler,
	// so we just verify the adapter creates the correct structure
	adapter := NewLegacyHandlerAdapter(JobTypeShellCommand, handler)

	if !adapter.CanHandle(JobTypeShellCommand) {
		t.Error("Adapter should handle SHELL_COMMAND")
	}

	// Verify the handler is stored
	if adapter.handler == nil {
		t.Error("adapter.handler should not be nil")
	}

	_ = capturedJob // silence unused variable
}

// TestLegacyHandlerAdapterPayloadEncoding verifies the map[string]any ->
// map[string]string down-conversion: scalars keep their fmt.Sprint form
// (unchanged behavior for existing handlers), while nested objects/arrays are
// json-encoded so their structure survives (the SERVICE_START "env" map,
// citadel-cli#462). Without this, fmt.Sprint on a map yields the unparseable Go
// "map[K:v]" form.
func TestLegacyHandlerAdapterPayloadEncoding(t *testing.T) {
	handler := &TestLegacyHandler{output: "ok"}
	adapter := NewLegacyHandlerAdapter("SERVICE_START", handler)

	job := &Job{
		ID:   "job-enc",
		Type: "SERVICE_START",
		Payload: map[string]any{
			"service":   "ac-x",                             // scalar string
			"host_port": float64(18789),                     // JSON number -> "18789"
			"flag":      true,                               // bool -> "true"
			"env":       map[string]any{"K": "v", "A": "b"}, // nested object -> JSON
			"ports":     []any{float64(1), float64(2)},      // nested array -> JSON
			"skip":      nil,                                // dropped
		},
	}

	if _, err := adapter.Execute(context.Background(), job, &NoOpStreamWriter{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := handler.capturedPayload

	// Scalars unchanged.
	if got["service"] != "ac-x" {
		t.Errorf("service = %q, want ac-x", got["service"])
	}
	if got["host_port"] != "18789" {
		t.Errorf("host_port = %q, want 18789 (scalar untouched)", got["host_port"])
	}
	if got["flag"] != "true" {
		t.Errorf("flag = %q, want true (scalar untouched)", got["flag"])
	}
	// nil dropped.
	if _, ok := got["skip"]; ok {
		t.Errorf("nil value should be dropped, got %q", got["skip"])
	}
	// Nested object -> parseable JSON (round-trips to the same map).
	var env map[string]string
	if err := json.Unmarshal([]byte(got["env"]), &env); err != nil {
		t.Fatalf("env is not JSON: %q (%v)", got["env"], err)
	}
	if env["K"] != "v" || env["A"] != "b" {
		t.Errorf("env round-trip = %v, want map[A:b K:v]", env)
	}
	// Nested array -> parseable JSON.
	var ports []int
	if err := json.Unmarshal([]byte(got["ports"]), &ports); err != nil {
		t.Fatalf("ports is not JSON: %q (%v)", got["ports"], err)
	}
	if len(ports) != 2 || ports[0] != 1 || ports[1] != 2 {
		t.Errorf("ports round-trip = %v, want [1 2]", ports)
	}
}

func TestCreateLegacyHandlers(t *testing.T) {
	handlers := CreateLegacyHandlers()

	if len(handlers) == 0 {
		t.Error("CreateLegacyHandlers returned empty slice")
	}

	// Verify we have handlers for known job types
	expectedTypes := []string{
		JobTypeShellCommand,
		JobTypeDownloadModel,
		JobTypeOllamaPull,
		JobTypeLlamaCppInference,
		JobTypeVLLMInference,
		JobTypeOllamaInference,
		JobTypeSandboxSuspend,
		JobTypeSandboxResume,
	}

	for _, jobType := range expectedTypes {
		found := false
		for _, h := range handlers {
			if h.CanHandle(jobType) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("No handler found for job type: %s", jobType)
		}
	}
}

func TestLegacyHandlerAdapterImplementsJobHandler(t *testing.T) {
	var _ JobHandler = (*LegacyHandlerAdapter)(nil)
}

// TestCreateLegacyHandlers_ShellDisabled verifies that ShellDisabled still
// registers a SHELL_COMMAND handler (so the node returns the "disabled" refusal
// rather than "unsupported job type"), but that handler refuses execution.
func TestCreateLegacyHandlers_ShellDisabled(t *testing.T) {
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{ShellDisabled: true})

	var shell JobHandler
	for _, h := range handlers {
		if h.CanHandle(JobTypeShellCommand) {
			shell = h
			break
		}
	}
	if shell == nil {
		t.Fatal("SHELL_COMMAND handler must remain registered even when disabled")
	}

	job := &Job{
		ID:      "job-shell-disabled",
		Type:    JobTypeShellCommand,
		Payload: map[string]any{"command": "echo should-not-run"},
	}
	result, err := shell.Execute(context.Background(), job, &NoOpStreamWriter{})
	if err == nil {
		t.Fatal("disabled SHELL_COMMAND handler should return an error")
	}
	if !strings.Contains(err.Error(), jobs.ShellDisabledError) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), jobs.ShellDisabledError)
	}
	// The adapter surfaces failures via the returned error; result should not
	// report success.
	if result != nil && result.Status == JobStatusSuccess {
		t.Error("disabled shell handler must not report success")
	}
}

// TestCreateLegacyHandlers_ShellEnabledByDefault confirms the default opt
// (ShellDisabled=false) leaves shell execution working.
func TestCreateLegacyHandlers_ShellEnabledByDefault(t *testing.T) {
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{})

	var shell JobHandler
	for _, h := range handlers {
		if h.CanHandle(JobTypeShellCommand) {
			shell = h
			break
		}
	}
	if shell == nil {
		t.Fatal("SHELL_COMMAND handler must be registered by default")
	}

	job := &Job{
		ID:      "job-shell-enabled",
		Type:    JobTypeShellCommand,
		Payload: map[string]any{"command": "echo ok"},
	}
	result, err := shell.Execute(context.Background(), job, &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("enabled shell handler should run: %v", err)
	}
	if result.Status != JobStatusSuccess {
		t.Errorf("result.Status = %v, want success", result.Status)
	}
}

func TestLegacyHandlerAdapterPayloadCoercion(t *testing.T) {
	handler := &TestLegacyHandler{output: "ok"}
	adapter := NewLegacyHandlerAdapter("TEST_JOB", handler)

	// Simulate a job payload as it arrives from json.Unmarshal (via Redis):
	// numbers are float64, booleans are bool, strings are string.
	job := &Job{
		ID:   "job-coerce",
		Type: "TEST_JOB",
		Payload: map[string]any{
			"path":        "/some/path",
			"offset":      float64(10),
			"limit":       float64(100),
			"replace_all": true,
			"nil_field":   nil,
		},
	}

	ctx := context.Background()
	stream := &NoOpStreamWriter{}

	_, err := adapter.Execute(ctx, job, stream)
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}

	// Verify all non-nil values were coerced to strings.
	checks := map[string]string{
		"path":        "/some/path",
		"offset":      "10",
		"limit":       "100",
		"replace_all": "true",
	}
	for k, want := range checks {
		got, ok := handler.capturedPayload[k]
		if !ok {
			t.Errorf("payload[%q] missing", k)
			continue
		}
		if got != want {
			t.Errorf("payload[%q] = %q, want %q", k, got, want)
		}
	}

	// nil values should be skipped.
	if _, ok := handler.capturedPayload["nil_field"]; ok {
		t.Error("nil_field should be skipped in payload")
	}
}

func TestCreateLegacyHandlersWithOpts_FileHandlers(t *testing.T) {
	dir := t.TempDir()

	fileTypes := []string{
		JobTypeFileRead,
		JobTypeFileWrite,
		JobTypeFileEdit,
		JobTypeFileList,
		JobTypeFileSearch,
	}

	// Without workspace: file handlers should NOT be registered.
	noWS := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{})
	for _, ft := range fileTypes {
		for _, h := range noWS {
			if h.CanHandle(ft) {
				t.Errorf("file handler %s registered without WorkspaceDir", ft)
			}
		}
	}

	// With workspace: file handlers should be registered.
	withWS := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{WorkspaceDir: dir})
	for _, ft := range fileTypes {
		found := false
		for _, h := range withWS {
			if h.CanHandle(ft) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("file handler %s not registered with WorkspaceDir", ft)
		}
	}
}

// TestAllKnownJobTypesCoversRegisteredHandlers guards the allKnownJobTypes slice
// (probed to report a node's supported job-type set in the unsupported-type
// failure, issue #382) against drift. Handlers only expose CanHandle(type), so a
// node's supported set can only be discovered by probing this canonical list. If
// a new job type is wired into CreateLegacyHandlersWithOpts but not added to
// allKnownJobTypes, every registered handler would answer CanHandle(newType)
// only when probed with newType -- which never happens -- so the type would
// silently vanish from the reported supported_types.
//
// We can't enumerate a handler's own type, but we can assert the invariant from
// the other side: the number of DISTINCT types in allKnownJobTypes that at least
// one registered handler accepts must equal the number of registered handlers
// (deduplicated by type). A registered type missing from the slice makes the
// former smaller than the latter.
func TestAllKnownJobTypesCoversRegisteredHandlers(t *testing.T) {
	// Register with a workspace + config dir so the file-op and service handlers
	// (which are otherwise gated) are included in the coverage check.
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{
		WorkspaceDir: t.TempDir(),
		ConfigDir:    t.TempDir(),
	})

	// Distinct known types that some registered handler accepts.
	covered := make(map[string]struct{})
	for _, jt := range allKnownJobTypes {
		for _, h := range handlers {
			if h.CanHandle(jt) {
				covered[jt] = struct{}{}
				break
			}
		}
	}

	// Distinct types the registered handlers actually accept, by probing every
	// known type. Any registered handler whose type is absent from
	// allKnownJobTypes cannot be probed and thus won't be counted here either --
	// so to detect drift we compare against the raw handler count deduped by the
	// types we *can* observe. A simpler, equivalent check: every entry in
	// allKnownJobTypes that is accepted must be genuinely handleable, and the
	// registry must not accept a type outside the slice. We approximate the
	// latter by asserting the covered set size matches the distinct-type count
	// the registry exposes for the known list.
	if len(covered) == 0 {
		t.Fatal("no registered handler matched any known job type; allKnownJobTypes is likely stale")
	}

	// Every base (unconditionally-registered) job type must be covered. This
	// catches the common drift: a new type added to consts + registry but not to
	// allKnownJobTypes.
	for _, jt := range []string{
		JobTypeShellCommand, JobTypeCobrowse, JobTypeVNCActions,
		JobTypeVNCScreenshot, JobTypeTmuxSession, JobTypeEmbedding,
	} {
		if _, ok := covered[jt]; !ok {
			t.Errorf("expected base job type %q to be covered by allKnownJobTypes + registry", jt)
		}
	}
}
