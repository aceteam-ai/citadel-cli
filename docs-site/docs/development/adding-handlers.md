---
sidebar_position: 3
title: Adding Job Handlers
---

# Adding Job Handlers

Job handlers are pluggable components that process specific types of workloads. When a job arrives (from Redis Streams or Nexus), the runner checks each registered handler to find one that can process it, then dispatches the job.

## Handler Interfaces

Citadel has two handler interfaces depending on the job source.

### Worker Mode (Redis Streams)

Used by `citadel work` for high-throughput job processing:

```go
// internal/worker/handler.go

type JobHandler interface {
    // CanHandle returns true if this handler can process the given job type.
    CanHandle(jobType string) bool

    // Execute processes the job.
    // The stream parameter allows streaming responses (can be nil for non-streaming).
    // Returns a JobResult with status and output.
    Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error)
}
```

### Nexus Mode

Used by the agent for Nexus HTTP polling:

```go
// internal/jobs/handler.go

type JobHandler interface {
    Execute(ctx JobContext, job *nexus.Job) (output []byte, err error)
}
```

## Step-by-Step: Adding a New Handler

### 1. Create the handler file

Create a new file in `internal/jobs/` (for Nexus mode) or implement the worker `JobHandler` interface (for Redis mode).

```go
// internal/jobs/myhandler.go
package jobs

import (
    "github.com/aceteam-ai/citadel-cli/internal/nexus"
)

type MyCustomHandler struct{}

func (h *MyCustomHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
    // Parse job.Payload for your handler's input
    // Do the work
    // Return output bytes and any error
    return []byte("done"), nil
}
```

### 2. Implement the interface

Ensure your handler satisfies the interface contract:

- Parse the job payload (typically JSON) for input parameters.
- Perform the work. Handlers should be **idempotent** -- running the same job twice should produce the same result without side effects.
- Return output bytes on success, or an error to signal failure and trigger a retry.

### 3. Register the handler

For Nexus mode, register in `cmd/job_handlers.go`:

```go
func init() {
    jobHandlers = map[string]jobs.JobHandler{
        "SHELL_COMMAND":      &jobs.ShellCommandHandler{},
        "MY_CUSTOM_JOB":      &jobs.MyCustomHandler{},  // Add your handler
        // ...
    }
}
```

For Worker mode, register in `cmd/work.go` where handlers are added to the runner.

### 4. Add tests

Write unit tests for your handler in `internal/jobs/myhandler_test.go`:

```go
func TestMyCustomHandler(t *testing.T) {
    handler := &jobs.MyCustomHandler{}
    output, err := handler.Execute(jobs.JobContext{}, &nexus.Job{
        ID:      "test-1",
        Type:    "MY_CUSTOM_JOB",
        Payload: `{"key": "value"}`,
    })
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if string(output) != "done" {
        t.Errorf("expected 'done', got '%s'", string(output))
    }
}
```

## Built-in Handlers

| Handler | Job Type | Description |
|---------|----------|-------------|
| `ShellCommandHandler` | `SHELL_COMMAND` | Executes shell commands on the node |
| `DownloadModelHandler` | `DOWNLOAD_MODEL` | Downloads model files to local storage |
| `OllamaPullHandler` | `OLLAMA_PULL` | Pulls models via the Ollama API |
| `VLLMInferenceHandler` | `VLLM_INFERENCE` | Routes inference requests to vLLM |
| `OllamaInferenceHandler` | `OLLAMA_INFERENCE` | Routes inference requests to Ollama |
| `LlamaCppInferenceHandler` | `LLAMACPP_INFERENCE` | Routes inference requests to llama.cpp |
| `ConfigHandler` | `APPLY_DEVICE_CONFIG` | Applies device configuration from the web dashboard |

## Streaming Responses

For handlers that produce incremental output (such as LLM token generation), use the `StreamWriter` interface:

```go
func (h *MyStreamingHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
    stream.WriteStart("Processing started")

    for i, chunk := range generateChunks(job) {
        stream.WriteChunk(chunk, i)
    }

    stream.WriteEnd(map[string]any{"tokens": 42})
    return &JobResult{Status: "SUCCESS"}, nil
}
```

The `StreamWriter` publishes chunks to Redis Pub/Sub in real time, allowing clients to receive streaming responses. If streaming is not needed, the runner passes a `NoOpStreamWriter` that discards all calls.

## Error Handling

- **Return an error** from `Execute` to signal that the job failed. The runner will handle retries based on its retry policy.
- **Return nil error with failure status** if the job completed but the result indicates a problem (e.g., model not found). This prevents unnecessary retries.
- Handlers should be **idempotent**. A job may be delivered more than once if the worker crashes between processing and acknowledgment.
