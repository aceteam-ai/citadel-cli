package redisapi

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"time"
)

// ConsumeJob reads the next available job from the queue.
// Returns nil if no job is available within the block timeout.
func (c *Client) ConsumeJob(ctx context.Context, req ConsumeRequest) (*Job, error) {
	// Use a longer timeout for blocking requests
	blockTimeout := time.Duration(req.BlockMs)*time.Millisecond + 5*time.Second

	// Create a context with the appropriate timeout
	reqCtx, cancel := context.WithTimeout(ctx, blockTimeout)
	defer cancel()

	var resp ConsumeResponse
	err := c.doRequestWithTimeout(reqCtx, http.MethodPost, "/api/fabric/redis/jobs/consume", req, &resp, blockTimeout)
	if err != nil {
		return nil, err
	}

	if len(resp.Messages) == 0 {
		return nil, nil // No job available
	}

	// Convert the first StreamMessage to a Job
	msg := resp.Messages[0]
	return parseStreamMessage(msg)
}

// parseStreamMessage converts a StreamMessage to a Job.
func parseStreamMessage(msg StreamMessage) (*Job, error) {
	job := &Job{
		MessageID: msg.ID,
		JobID:     msg.Data.JobID,
		RawData:   make(map[string]any),
	}

	// Preserve raw fields from stream message data
	if msg.Data.EnqueuedAt != "" {
		job.RawData["enqueuedAt"] = msg.Data.EnqueuedAt
	}

	// Parse the payload JSON string
	if msg.Data.Payload != "" {
		var payload map[string]any
		if err := json.Unmarshal([]byte(msg.Data.Payload), &payload); err != nil {
			return nil, fmt.Errorf("failed to parse job payload: %w", err)
		}
		job.Payload = payload

		// Extract type from payload if present
		if jobType, ok := payload["type"].(string); ok {
			job.Type = jobType
		}
	}

	return job, nil
}

// AcknowledgeJob acknowledges a successfully processed job.
func (c *Client) AcknowledgeJob(ctx context.Context, req AcknowledgeRequest) error {
	var resp AcknowledgeResponse
	err := c.doRequest(ctx, http.MethodPost, "/api/fabric/redis/jobs/acknowledge", req, &resp)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("acknowledge failed: %s", resp.Message)
	}

	return nil
}

// SetJobStatus stores job status via the API.
func (c *Client) SetJobStatus(ctx context.Context, jobID, status string, data map[string]any) error {
	key := fmt.Sprintf("job:%s:status", jobID)

	fields := map[string]any{
		"status":     status,
		"worker_id":  c.workerID,
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	}

	if data != nil {
		maps.Copy(fields, data)
	}

	// Serialize the fields to JSON string for KV storage
	return c.SetKey(ctx, key, fields, 86400) // 24-hour TTL
}

// doRequestWithTimeout is like doRequest but with a custom timeout.
func (c *Client) doRequestWithTimeout(ctx context.Context, method, path string, body any, result any, timeout time.Duration) error {
	// Create a new HTTP client with custom timeout for this request
	client := &http.Client{
		Timeout: timeout,
	}

	// Temporarily swap the client
	origClient := c.httpClient
	c.httpClient = client
	defer func() { c.httpClient = origClient }()

	return c.doRequest(ctx, method, path, body, result)
}
