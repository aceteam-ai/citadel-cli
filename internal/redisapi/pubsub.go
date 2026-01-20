package redisapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Publish publishes a message to a Redis Pub/Sub channel.
func (c *Client) Publish(ctx context.Context, channel string, message any) error {
	// Serialize message to JSON
	msgJSON, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	req := PublishRequest{
		Channel: channel,
		Message: string(msgJSON),
	}

	var resp PublishResponse
	err = c.doRequest(ctx, http.MethodPost, "/api/fabric/redis/pubsub/publish", req, &resp)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("publish failed")
	}

	return nil
}

// PublishStreamEvent publishes a streaming event to Redis Pub/Sub.
func (c *Client) PublishStreamEvent(ctx context.Context, jobID string, eventType string, data map[string]any) error {
	streamName := fmt.Sprintf("stream:v1:%s", jobID)

	event := StreamEvent{
		Version:   "1.0",
		Type:      eventType,
		JobID:     jobID,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      data,
	}

	return c.Publish(ctx, streamName, event)
}

// PublishStart publishes a "start" event for a job.
func (c *Client) PublishStart(ctx context.Context, jobID string, message string) error {
	return c.PublishStreamEvent(ctx, jobID, "start", map[string]any{
		"message": message,
	})
}

// PublishChunk publishes a "chunk" event for streaming responses.
func (c *Client) PublishChunk(ctx context.Context, jobID string, content string, index int) error {
	return c.PublishStreamEvent(ctx, jobID, "chunk", map[string]any{
		"content": content,
		"index":   index,
	})
}

// PublishEnd publishes an "end" event when job completes.
func (c *Client) PublishEnd(ctx context.Context, jobID string, result map[string]any) error {
	return c.PublishStreamEvent(ctx, jobID, "end", map[string]any{
		"result": result,
	})
}

// PublishError publishes an "error" event when job fails.
func (c *Client) PublishError(ctx context.Context, jobID string, errMsg string, recoverable bool) error {
	return c.PublishStreamEvent(ctx, jobID, "error", map[string]any{
		"error":       errMsg,
		"recoverable": recoverable,
	})
}

// GetKey retrieves a value from Redis KV storage.
func (c *Client) GetKey(ctx context.Context, key string) (string, int, error) {
	path := fmt.Sprintf("/api/fabric/redis/kv?key=%s", key)

	var resp KVGetResponse
	err := c.doRequest(ctx, http.MethodGet, path, nil, &resp)
	if err != nil {
		return "", -2, err
	}

	if !resp.Exists {
		return "", -2, nil
	}

	return resp.Value, resp.TTL, nil
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

	var resp KVSetResponse
	err := c.doRequest(ctx, http.MethodPost, "/api/fabric/redis/kv", req, &resp)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("set key failed: %s", resp.Message)
	}

	return nil
}

// DeleteKey removes a key from Redis KV storage.
func (c *Client) DeleteKey(ctx context.Context, key string) (bool, error) {
	path := fmt.Sprintf("/api/fabric/redis/kv?key=%s", key)

	var resp KVDeleteResponse
	err := c.doRequest(ctx, http.MethodDelete, path, nil, &resp)
	if err != nil {
		return false, err
	}

	return resp.Deleted, nil
}

// StreamAdd adds an entry to a Redis Stream (for status publishing).
func (c *Client) StreamAdd(ctx context.Context, stream string, values map[string]any, maxLen int64) error {
	req := StreamAddRequest{
		Stream: stream,
		Values: values,
		MaxLen: maxLen,
		Approx: true,
	}

	var resp StreamAddResponse
	err := c.doRequest(ctx, http.MethodPost, "/api/fabric/redis/pubsub/publish", req, &resp)
	if err != nil {
		return err
	}

	return nil
}
