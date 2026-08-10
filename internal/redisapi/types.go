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
	Type       string `json:"type"`    // Job type (e.g., "SHELL_COMMAND"), top-level stream field
	Payload    string `json:"payload"` // JSON-encoded job payload
	EnqueuedAt string `json:"enqueuedAt"`
	RayID      string `json:"rayId"`
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

// AcknowledgeResponse is the response from POST /api/fabric/redis/jobs/acknowledge.
//
// The route returns `{ acknowledged: boolean }` where `acknowledged` is
// `XACK > 0`. It is NOT a success flag: a re-ack of a message already out of the
// consumer group's PEL is a legitimate 200 with `acknowledged: false`, which the
// ws_source WS-ack-failed-then-HTTP-retry path produces routinely. Callers must
// key off the HTTP status, not this field. See #721.
type AcknowledgeResponse struct {
	Acknowledged bool `json:"acknowledged"`
}

// PublishRequest is the request body for POST /api/fabric/redis/pubsub/publish
type PublishRequest struct {
	Channel string `json:"channel"`
	Message string `json:"message"` // JSON-encoded payload
}

// PublishResponse is the response from POST /api/fabric/redis/pubsub/publish.
//
// The route returns `{ published: true }`. It never sent `success`, so the old
// `Success` field decoded to false on every successful 200 and the client
// reported "publish failed" for a publish the server had executed (#721).
// The field is kept only for logging; success is decided by the HTTP status.
type PublishResponse struct {
	Published bool `json:"published"`
}

// StreamAddRequest is the request body for POST /api/fabric/redis/streams/add
type StreamAddRequest struct {
	Stream string            `json:"stream"`
	Values map[string]string `json:"values"`
	MaxLen int64             `json:"maxLen,omitempty"`
	Approx bool              `json:"approx,omitempty"`
}

// StreamAddResponse is the response from POST /api/fabric/redis/streams/add.
// This route does send `success`, alongside the Redis stream message ID.
type StreamAddResponse struct {
	Success   bool   `json:"success"`
	MessageID string `json:"messageId,omitempty"`
}

// KVGetRequest is the query params for GET /api/fabric/redis/kv
type KVGetRequest struct {
	Key string `json:"key"`
}

// KVGetResponse is the response from GET /api/fabric/redis/kv.
//
// The route returns `{ key, value, ttl }` and has never sent an `exists` field.
// The struct used to declare one, so it decoded to false on every response and
// GetKey reported every key as missing (#721) — which silently disabled
// mid-job cancellation on API-path nodes, since IsJobCancelled is the only
// caller. Existence is now derived from `value`/`ttl` inside GetKey.
//
// Value is a pointer because the route sends JSON null for a missing key, which
// is the only way to tell "absent" from "present and empty".
type KVGetResponse struct {
	Key   string  `json:"key"`
	Value *string `json:"value"`
	TTL   int     `json:"ttl"` // -1 if no TTL, -2 if key doesn't exist
}

// KVSetRequest is the request body for POST /api/fabric/redis/kv
type KVSetRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	TTL   int    `json:"ttl,omitempty"` // TTL in seconds, 0 for no expiry
}

// KVSetResponse is the response from POST /api/fabric/redis/kv.
// This route does send `success`. It does not send a message field.
type KVSetResponse struct {
	Success bool `json:"success"`
}

// KVDeleteResponse is the response from DELETE /api/fabric/redis/kv.
// `deleted` is DEL > 0, i.e. false when the key was already absent.
type KVDeleteResponse struct {
	Deleted bool `json:"deleted"`
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

// NodeMeta holds node identity metadata injected into every stream event.
type NodeMeta struct {
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name"`
}

// WorkerConfigResponse is the response from GET /api/fabric/worker-config.
// This endpoint returns the queue and org information needed by the worker
// without exposing direct Redis credentials.
type WorkerConfigResponse struct {
	Queue         string   `json:"queue"`
	QueueNames    []string `json:"queue_names,omitempty"`
	ConsumerGroup string   `json:"consumer_group,omitempty"`
	OrgID         string   `json:"org_id,omitempty"`
}
