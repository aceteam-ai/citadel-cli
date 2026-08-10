package redisapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Publish publishes a message to a Redis Pub/Sub channel.
// Uses WebSocket when connected, falls back to HTTP otherwise.
func (c *Client) Publish(ctx context.Context, channel string, message any) error {
	// Prefer WebSocket when connected
	if c.wsClient != nil && c.wsClient.IsConnected() {
		c.debug("publish: using WebSocket for channel %s", channel)
		return c.wsClient.Publish(ctx, channel, message)
	}

	// Fall back to HTTP
	c.debug("publish: using HTTP for channel %s", channel)

	// Serialize message to JSON
	msgJSON, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	req := PublishRequest{
		Channel: channel,
		Message: string(msgJSON),
	}

	// A 2xx from doRequest IS the ack. The route returns a non-2xx for every
	// failure it has (403 missing scope, 400 bad channel pattern, 403 org
	// mismatch), and doRequest turns those into an error carrying the status and
	// body. Re-checking a body flag on top only creates a second contract that
	// can drift out of sync with the route — which is exactly what #721 was.
	var resp PublishResponse
	if err := c.doRequest(ctx, http.MethodPost, "/api/fabric/redis/pubsub/publish", req, &resp); err != nil {
		return err
	}

	c.debug("publish: channel %s acked (published=%v)", channel, resp.Published)
	return nil
}

// PublishStreamEvent publishes a streaming event to Redis Pub/Sub.
func (c *Client) PublishStreamEvent(ctx context.Context, jobID, rayID, eventType string, data map[string]any) error {
	streamName := fmt.Sprintf("stream:v1:%s", jobID)

	// Inject node identity metadata into every event for operator attribution
	if c.nodeMeta != nil {
		if data == nil {
			data = make(map[string]any)
		}
		data["meta"] = c.nodeMeta
	}

	event := StreamEvent{
		Version:   "1.0",
		Type:      eventType,
		JobID:     jobID,
		RayID:     rayID,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      data,
	}

	return c.Publish(ctx, streamName, event)
}

// PublishClaimed publishes a "claimed" event for a job, emitted the moment the
// worker reads it off the queue (before handler execution). The backend
// dispatcher (aceteam#6000) uses it to fast-fail a wedged/dead node that never
// claims, instead of waiting the full result budget.
func (c *Client) PublishClaimed(ctx context.Context, jobID, rayID, agentVersion string) error {
	return c.PublishStreamEvent(ctx, jobID, rayID, "claimed", map[string]any{
		"agent_version": agentVersion,
	})
}

// PublishStart publishes a "start" event for a job.
func (c *Client) PublishStart(ctx context.Context, jobID, rayID, message string) error {
	return c.PublishStreamEvent(ctx, jobID, rayID, "start", map[string]any{
		"message": message,
	})
}

// PublishChunk publishes a "chunk" event for streaming responses.
func (c *Client) PublishChunk(ctx context.Context, jobID, rayID, content string, index int) error {
	return c.PublishStreamEvent(ctx, jobID, rayID, "chunk", map[string]any{
		"content": content,
		"index":   index,
	})
}

// PublishEnd publishes an "end" event when job completes.
func (c *Client) PublishEnd(ctx context.Context, jobID, rayID string, result map[string]any) error {
	return c.PublishStreamEvent(ctx, jobID, rayID, "end", map[string]any{
		"result": result,
	})
}

// PublishError publishes an "error" event when job fails.
func (c *Client) PublishError(ctx context.Context, jobID, rayID, errMsg string, recoverable bool) error {
	return c.PublishStreamEvent(ctx, jobID, rayID, "error", map[string]any{
		"error":       errMsg,
		"recoverable": recoverable,
	})
}

// PublishCancelled publishes a "cancelled" terminal event when a job is cancelled.
func (c *Client) PublishCancelled(ctx context.Context, jobID, rayID, reason string) error {
	return c.PublishStreamEvent(ctx, jobID, rayID, "cancelled", map[string]any{
		"reason": reason,
	})
}

// IsJobCancelled checks whether a cancellation flag exists for the given job.
func (c *Client) IsJobCancelled(ctx context.Context, jobID string) (bool, error) {
	key := fmt.Sprintf("job:cancelled:%s", jobID)
	_, ttl, err := c.GetKey(ctx, key)
	if err != nil {
		return false, err
	}
	// ttl == -2 means key doesn't exist
	return ttl != -2, nil
}

// GetKey retrieves a value from Redis KV storage.
//
// The returned TTL follows Redis semantics and is the authoritative existence
// signal for callers: -2 means the key does not exist, -1 means it exists with
// no expiry, >= 0 is the remaining lifetime in seconds.
func (c *Client) GetKey(ctx context.Context, key string) (string, int, error) {
	path := fmt.Sprintf("/api/fabric/redis/kv?key=%s", url.QueryEscape(key))

	var resp KVGetResponse
	err := c.doRequest(ctx, http.MethodGet, path, nil, &resp)
	if err != nil {
		return "", -2, err
	}

	// A JSON null value is the route's "key does not exist". Do not invent an
	// empty string for it, and do not trust the TTL alone: the route serves GET
	// and TTL from a non-atomic Promise.all, so a key that expires between the
	// two reads comes back with a real value and a TTL of -2. Normalize that to
	// -1 so the "-2 means absent" contract above holds for callers.
	if resp.Value == nil {
		return "", -2, nil
	}
	ttl := resp.TTL
	if ttl == -2 {
		ttl = -1
	}
	return *resp.Value, ttl, nil
}

// SetKey stores a value in Redis KV storage.
func (c *Client) SetKey(ctx context.Context, key string, value any, ttl int) error {
	// Serialize value to JSON string if not already a string
	var valueStr string
	switch v := value.(type) {
	case string:
		valueStr = v
	default:
		jsonData, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("failed to marshal value: %w", err)
		}
		valueStr = string(jsonData)
	}

	req := KVSetRequest{
		Key:   key,
		Value: valueStr,
		TTL:   ttl,
	}

	// 2xx is the ack; see Publish. This route does send `success: true`, so the
	// old body check was correct here, but keeping one rule for the whole file
	// is what stops the next route tweak from silently breaking a caller.
	var resp KVSetResponse
	if err := c.doRequest(ctx, http.MethodPost, "/api/fabric/redis/kv", req, &resp); err != nil {
		return err
	}

	return nil
}

// DeleteKey removes a key from Redis KV storage.
func (c *Client) DeleteKey(ctx context.Context, key string) (bool, error) {
	path := fmt.Sprintf("/api/fabric/redis/kv?key=%s", url.QueryEscape(key))

	var resp KVDeleteResponse
	err := c.doRequest(ctx, http.MethodDelete, path, nil, &resp)
	if err != nil {
		return false, err
	}

	return resp.Deleted, nil
}

// StreamAdd adds an entry to a Redis Stream (for status publishing).
func (c *Client) StreamAdd(ctx context.Context, stream string, values map[string]string, maxLen int64) error {
	req := StreamAddRequest{
		Stream: stream,
		Values: values,
		MaxLen: maxLen,
		Approx: true,
	}

	// 2xx is the ack; see Publish. This route does send `success: true`, so the
	// old body check was correct here too.
	var resp StreamAddResponse
	if err := c.doRequest(ctx, http.MethodPost, "/api/fabric/redis/streams/add", req, &resp); err != nil {
		return err
	}

	c.debug("stream add: %s acked (messageId=%s)", stream, resp.MessageID)
	return nil
}
