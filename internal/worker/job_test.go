package worker

import (
	"testing"
	"time"
)

func TestJobStatus(t *testing.T) {
	tests := []struct {
		name   string
		status JobStatus
		want   string
	}{
		{"success status", JobStatusSuccess, "success"},
		{"failure status", JobStatusFailure, "failure"},
		{"retry status", JobStatusRetry, "retry"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.status) != tt.want {
				t.Errorf("JobStatus = %v, want %v", tt.status, tt.want)
			}
		})
	}
}

func TestJobTypes(t *testing.T) {
	// Verify job type constants are defined correctly
	tests := []struct {
		name     string
		jobType  string
		expected string
	}{
		{"shell command", JobTypeShellCommand, "SHELL_COMMAND"},
		{"download model", JobTypeDownloadModel, "DOWNLOAD_MODEL"},
		{"ollama pull", JobTypeOllamaPull, "OLLAMA_PULL"},
		{"llamacpp inference", JobTypeLlamaCppInference, "LLAMACPP_INFERENCE"},
		{"vllm inference", JobTypeVLLMInference, "VLLM_INFERENCE"},
		{"ollama inference", JobTypeOllamaInference, "OLLAMA_INFERENCE"},
		{"llm inference", JobTypeLLMInference, "llm_inference"},
		{"embedding", JobTypeEmbedding, "embedding"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.jobType != tt.expected {
				t.Errorf("JobType = %v, want %v", tt.jobType, tt.expected)
			}
		})
	}
}

func TestJob(t *testing.T) {
	job := &Job{
		ID:        "test-job-123",
		Type:      JobTypeShellCommand,
		Payload:   map[string]any{"command": "echo hello"},
		Source:    "nexus",
		MessageID: "msg-456",
		Metadata: JobMetadata{
			CreatedAt:   time.Now(),
			Attempts:    1,
			MaxAttempts: 3,
			Priority:    1,
			Tags:        []string{"test", "shell"},
		},
	}

	if job.ID != "test-job-123" {
		t.Errorf("Job.ID = %v, want test-job-123", job.ID)
	}
	if job.Type != JobTypeShellCommand {
		t.Errorf("Job.Type = %v, want %v", job.Type, JobTypeShellCommand)
	}
	if job.Source != "nexus" {
		t.Errorf("Job.Source = %v, want nexus", job.Source)
	}
	if job.Metadata.Attempts != 1 {
		t.Errorf("Job.Metadata.Attempts = %v, want 1", job.Metadata.Attempts)
	}
	if len(job.Metadata.Tags) != 2 {
		t.Errorf("Job.Metadata.Tags length = %v, want 2", len(job.Metadata.Tags))
	}
}

func TestJobResult(t *testing.T) {
	result := &JobResult{
		Status:   JobStatusSuccess,
		Output:   map[string]any{"result": "ok"},
		Error:    nil,
		Duration: 100 * time.Millisecond,
	}

	if result.Status != JobStatusSuccess {
		t.Errorf("JobResult.Status = %v, want %v", result.Status, JobStatusSuccess)
	}
	if result.Duration != 100*time.Millisecond {
		t.Errorf("JobResult.Duration = %v, want 100ms", result.Duration)
	}
	if result.Output["result"] != "ok" {
		t.Errorf("JobResult.Output[result] = %v, want ok", result.Output["result"])
	}
}
