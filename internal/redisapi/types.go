// Package redisapi provides an HTTP client for the AceTeam Redis API proxy.
//
// This package replaces direct Redis connections with authenticated REST API calls,
// eliminating the security risk of exposing Redis credentials to devices and resolving
// network accessibility issues with internal Redis instances.
//
// All API calls require a device_api_token obtained during device authentication.
package redisapi

// ConsumeRequest is the request body for POST /api/fabric/redis/jobs/consume
type ConsumeRequest struct {
	Queue         string `json:"queue"`
	ConsumerGroup string `json:"consumer_group"`
	Consumer      string `json:"consumer"`
	Count         int    `json:"count"`
	BlockMs       int    `json:"block_ms"`
}

// ConsumeResponse is the response from POST /api/fabric/redis/jobs/consume
type ConsumeResponse struct {
	Jobs []Job `json:"jobs"`
}

// Job represents a job returned from the API
type Job struct {
	MessageID string         `json:"message_id"`
	JobID     string         `json:"job_id"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload"`
	RawData   map[string]any `json:"raw_data,omitempty"`
}

// AcknowledgeRequest is the request body for POST /api/fabric/redis/jobs/acknowledge
type AcknowledgeRequest struct {
	Queue         string `json:"queue"`
	ConsumerGroup string `json:"consumer_group"`
	MessageID     string `json:"message_id"`
}

// AcknowledgeResponse is the response from POST /api/fabric/redis/jobs/acknowledge
type AcknowledgeResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// PublishRequest is the request body for POST /api/fabric/redis/pubsub/publish
type PublishRequest struct {
	Channel string `json:"channel"`
	Message string `json:"message"` // JSON-encoded payload
}

// PublishResponse is the response from POST /api/fabric/redis/pubsub/publish
type PublishResponse struct {
	Success    bool  `json:"success"`
	Receivers  int64 `json:"receivers,omitempty"`
}

// StreamAddRequest is the request body for adding to Redis Streams (via publish endpoint)
type StreamAddRequest struct {
	Stream string         `json:"stream"`
	Values map[string]any `json:"values"`
	MaxLen int64          `json:"max_len,omitempty"`
	Approx bool           `json:"approx,omitempty"`
}

// StreamAddResponse is the response from stream add operations
type StreamAddResponse struct {
	Success   bool   `json:"success"`
	MessageID string `json:"message_id,omitempty"`
}

// KVGetRequest is the query params for GET /api/fabric/redis/kv
type KVGetRequest struct {
	Key string `json:"key"`
}

// KVGetResponse is the response from GET /api/fabric/redis/kv
type KVGetResponse struct {
	Value  string `json:"value"`
	TTL    int    `json:"ttl"` // -1 if no TTL, -2 if key doesn't exist
	Exists bool   `json:"exists"`
}

// KVSetRequest is the request body for POST /api/fabric/redis/kv
type KVSetRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	TTL   int    `json:"ttl,omitempty"` // TTL in seconds, 0 for no expiry
}

// KVSetResponse is the response from POST /api/fabric/redis/kv
type KVSetResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// KVDeleteResponse is the response from DELETE /api/fabric/redis/kv
type KVDeleteResponse struct {
	Deleted bool   `json:"deleted"`
	Message string `json:"message,omitempty"`
}

// APIError represents an error response from the API
type APIError struct {
	Error       string `json:"error"`
	Description string `json:"error_description,omitempty"`
	StatusCode  int    `json:"-"`
}

func (e *APIError) Err() string {
	if e.Description != "" {
		return e.Error + ": " + e.Description
	}
	return e.Error
}

// StreamEvent represents an event for streaming responses (published via Pub/Sub)
type StreamEvent struct {
	Version   string         `json:"version"`
	Type      string         `json:"type"` // "start", "chunk", "end", "error"
	JobID     string         `json:"jobId"`
	Timestamp string         `json:"timestamp"`
	Data      map[string]any `json:"data,omitempty"`
}
