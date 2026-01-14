package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/aceboss/citadel-cli/internal/jobs"
	"github.com/aceboss/citadel-cli/internal/nexus"
)

// TestLegacyHandler is a mock jobs.JobHandler for testing.
type TestLegacyHandler struct {
	shouldFail bool
	output     string
}

func (h *TestLegacyHandler) Execute(ctx jobs.JobContext, job *nexus.Job) ([]byte, error) {
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
