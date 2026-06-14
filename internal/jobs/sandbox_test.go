package jobs

import (
	"encoding/json"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// --- SANDBOX_SUSPEND tests ---

func TestSandboxSuspend_MissingSandboxID(t *testing.T) {
	h := &SandboxSuspendHandler{}
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "test-1",
		Type: "SANDBOX_SUSPEND",
		Payload: map[string]string{
			"container_id": "abc123",
		},
	})
	if err == nil {
		t.Fatal("expected error for missing sandbox_id, got nil")
	}
}

func TestSandboxSuspend_MissingContainerID(t *testing.T) {
	h := &SandboxSuspendHandler{}
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "test-2",
		Type: "SANDBOX_SUSPEND",
		Payload: map[string]string{
			"sandbox_id": "sb-1",
		},
	})
	if err == nil {
		t.Fatal("expected error for missing container_id, got nil")
	}
}

func TestSandboxSuspend_EmptyPayload(t *testing.T) {
	h := &SandboxSuspendHandler{}
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "test-3",
		Type:    "SANDBOX_SUSPEND",
		Payload: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for empty payload, got nil")
	}
}

func TestSandboxSuspend_DockerNotAvailable(t *testing.T) {
	h := &SandboxSuspendHandler{}
	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "test-4",
		Type: "SANDBOX_SUSPEND",
		Payload: map[string]string{
			"sandbox_id":   "sb-1",
			"container_id": "nonexistent-container-xyz",
		},
	})
	// When docker is not available or container doesn't exist, the handler
	// returns a result with Success=false (not an error), because it
	// marshals the failure into a sandboxResult.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result sandboxResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if result.Success {
		t.Error("expected Success=false for nonexistent container")
	}
	if result.Action != "suspend" {
		t.Errorf("action = %q, want %q", result.Action, "suspend")
	}
	if result.SandboxID != "sb-1" {
		t.Errorf("sandbox_id = %q, want %q", result.SandboxID, "sb-1")
	}
	if result.ContainerID != "nonexistent-container-xyz" {
		t.Errorf("container_id = %q, want %q", result.ContainerID, "nonexistent-container-xyz")
	}
	if result.Error == "" {
		t.Error("expected non-empty error message")
	}
}

// --- SANDBOX_RESUME tests ---

func TestSandboxResume_MissingSandboxID(t *testing.T) {
	h := &SandboxResumeHandler{}
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "test-5",
		Type: "SANDBOX_RESUME",
		Payload: map[string]string{
			"container_id": "abc123",
		},
	})
	if err == nil {
		t.Fatal("expected error for missing sandbox_id, got nil")
	}
}

func TestSandboxResume_MissingContainerID(t *testing.T) {
	h := &SandboxResumeHandler{}
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "test-6",
		Type: "SANDBOX_RESUME",
		Payload: map[string]string{
			"sandbox_id": "sb-2",
		},
	})
	if err == nil {
		t.Fatal("expected error for missing container_id, got nil")
	}
}

func TestSandboxResume_EmptyPayload(t *testing.T) {
	h := &SandboxResumeHandler{}
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "test-7",
		Type:    "SANDBOX_RESUME",
		Payload: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for empty payload, got nil")
	}
}

func TestSandboxResume_DockerNotAvailable(t *testing.T) {
	h := &SandboxResumeHandler{}
	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "test-8",
		Type: "SANDBOX_RESUME",
		Payload: map[string]string{
			"sandbox_id":   "sb-2",
			"container_id": "nonexistent-container-xyz",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result sandboxResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if result.Success {
		t.Error("expected Success=false for nonexistent container")
	}
	if result.Action != "resume" {
		t.Errorf("action = %q, want %q", result.Action, "resume")
	}
	if result.SandboxID != "sb-2" {
		t.Errorf("sandbox_id = %q, want %q", result.SandboxID, "sb-2")
	}
	if result.ContainerID != "nonexistent-container-xyz" {
		t.Errorf("container_id = %q, want %q", result.ContainerID, "nonexistent-container-xyz")
	}
	if result.Error == "" {
		t.Error("expected non-empty error message")
	}
}

// --- Interface compliance ---

func TestSandboxHandlersImplementJobHandler(t *testing.T) {
	var _ JobHandler = (*SandboxSuspendHandler)(nil)
	var _ JobHandler = (*SandboxResumeHandler)(nil)
}
