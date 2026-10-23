package redisapi

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/aceteam-ai/citadel-cli/internal/workercontrol"
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
	msg.Data.observation = msg.Data.observation.WithSource(req.Queue, req.Group, "redis-api")
	job, err := ParseStreamMessage(msg)
	if err != nil {
		return nil, err
	}
	if workercontrol.IsQueue(req.Queue) || job.Control != nil {
		if job.Control == nil || job.Control.Identity.Queue != req.Queue || job.Control.Identity.Group != req.Group {
			return nil, workercontrol.ErrInvalid
		}
	}
	return job, nil
}

// ParseStreamMessage converts a StreamMessage to a Job.
func ParseStreamMessage(msg StreamMessage) (*Job, error) {
	job := &Job{
		observation: msg.Data.observation.Clone(),
		MessageID:   msg.ID,
		JobID:       msg.Data.JobID,
		Type:        msg.Data.Type, // Top-level stream field (e.g., SHELL_COMMAND)
		RawData:     make(map[string]any),
	}
	if msg.Data.Type == "WORKER_CONTROL" || msg.Data.WorkerControl != "" {
		raw, _ := json.Marshal(workercontrol.Data{JobID: msg.Data.JobID, Type: msg.Data.Type, Payload: msg.Data.Payload, WorkerControl: msg.Data.WorkerControl})
		envelope, payload, err := workercontrol.ParseData(raw, msg.ID, "", "")
		if err != nil {
			return nil, err
		}
		job.Control = &envelope
		job.Payload = map[string]any{"action": payload.Action, "target_node": payload.TargetNode}
		job.RawPayload = []byte(msg.Data.Payload)
		return job, nil
	}

	// Preserve raw fields from stream message data
	if msg.Data.EnqueuedAt != "" {
		job.RawData["enqueuedAt"] = msg.Data.EnqueuedAt
	}
	if msg.Data.RayID != "" {
		job.RawData["rayId"] = msg.Data.RayID
	}

	// Parse the payload JSON string
	if msg.Data.Payload != "" {
		var payload map[string]any
		if err := json.Unmarshal([]byte(msg.Data.Payload), &payload); err != nil {
			return nil, fmt.Errorf("failed to parse job payload: %w", err)
		}
		job.Payload = payload
		job.RawPayload = []byte(msg.Data.Payload)

		// Fall back to type from payload if not at top level
		if job.Type == "" {
			if jobType, ok := payload["type"].(string); ok {
				job.Type = jobType
			}
		}
	}

	return job, nil
}

// AcknowledgeJob acknowledges a successfully processed job.
//
// A 2xx is the ack. The route's `acknowledged` field is XACK's return value, so
// it is false whenever the message was already out of the consumer group's PEL
// — a duplicate ack, which is a no-op and not an error. Treating it as failure
// (the pre-#721 behaviour, which additionally read a `success` field the route
// never sent) made every HTTP ack report failure.
func (c *Client) AcknowledgeJob(ctx context.Context, req AcknowledgeRequest) error {
	if workercontrol.IsQueue(req.Queue) || req.Group == workercontrol.Protocol || req.Group == "worker-control-v1" {
		return workercontrol.ErrDenied
	}
	var resp AcknowledgeResponse
	if err := c.doRequest(ctx, http.MethodPost, "/api/fabric/redis/jobs/acknowledge", req, &resp); err != nil {
		return err
	}

	c.debug("acknowledge: message %s on %s (acknowledged=%v)", req.MessageID, req.Queue, resp.Acknowledged)
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

// SetJobProgress mirrors the latest progress event into job:<id>:progress with a
// 7-day TTL, the API-proxy counterpart of redis.Client.SetJobProgress. The whole
// event (including nested counts) is stored as one JSON value. Best-effort; the
// pub/sub "progress" event is the primary path (aceteam#10876 C1).
func (c *Client) SetJobProgress(ctx context.Context, jobID string, event map[string]any) error {
	key := fmt.Sprintf("job:%s:progress", jobID)
	return c.SetKey(ctx, key, event, 604800) // 7-day TTL
}

// doRequestWithTimeout is like doRequest but with a custom timeout.
func (c *Client) doRequestWithTimeout(ctx context.Context, method, path string, body any, result any, timeout time.Duration) error {
	// Create a new HTTP client with custom timeout for this request
	client := &http.Client{
		Timeout: timeout,
	}

	// Consumption and authenticated control publication run concurrently. Never
	// swap the shared client while a claim/result request may be using it.
	return c.doRequestUsing(ctx, client, method, path, body, result)
}
