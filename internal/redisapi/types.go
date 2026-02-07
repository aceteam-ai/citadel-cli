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
	Queue    string `json:"queue"`
	Group    string `json:"group"`
	Consumer string `json:"consumer"`
	Count    int    `json:"count,omitempty"`
	BlockMs  int    `json:"blockMs,omitempty"`
}

// ConsumeResponse is the response from POST /api/fabric/redis/jobs/consume
type ConsumeResponse struct {
	Messages []StreamMessage `json:"messages"`
}

// StreamMessage represents a message from Redis Streams
type StreamMessage struct {
	ID   string            `json:"id"`
	Data StreamMessageData `json:"data"`
}

// StreamMessageData contains the job data within a stream message
type StreamMessageData struct {
	JobID      string `json:"jobId"`
	Payload    string `json:"payload"` // JSON-encoded job payload
	EnqueuedAt string `json:"enqueuedAt"`
}

// Job represents a parsed job ready for processing
type Job struct {
	MessageID string         `json:"message_id"`
	JobID     string         `json:"job_id"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload"`
	RawData   map[string]any `json:"raw_data,omitempty"`
}

// AcknowledgeRequest is the request body for POST /api/fabric/redis/jobs/acknowledge
type AcknowledgeRequest struct {
	Queue     string `json:"queue"`
	Group     string `json:"group"`
	MessageID string `json:"messageId"`
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

// StreamAddRequest is the request body for POST /api/fabric/redis/streams/add
type StreamAddRequest struct {
	Stream string            `json:"stream"`
	Values map[string]string `json:"values"`
	MaxLen int64             `json:"maxLen,omitempty"`
	Approx bool              `json:"approx,omitempty"`
}

// StreamAddResponse is the response from POST /api/fabric/redis/streams/add
type StreamAddResponse struct {
	Success   bool   `json:"success"`
	MessageID string `json:"messageId,omitempty"`
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
	Type      string         `json:"type"` // "start", "chunk", "end", "error", "cancelled"
	JobID     string         `json:"jobId"`
	RayID     string         `json:"rayId,omitempty"`
	Timestamp string         `json:"timestamp"`
	Data      map[string]any `json:"data,omitempty"`
}
