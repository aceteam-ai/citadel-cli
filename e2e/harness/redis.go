package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// RedisHarness provides Redis test utilities
type RedisHarness struct {
	client *redis.Client
}

// NewRedisHarness creates a new Redis harness
func NewRedisHarness(url string) (*RedisHarness, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("invalid redis URL: %w", err)
	}

	client := redis.NewClient(opts)

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &RedisHarness{client: client}, nil
}

// Job represents a job in the queue
type Job struct {
	ID      string                 `json:"jobId"`
	Type    string                 `json:"type"`
	Payload map[string]interface{} `json:"payload"`
}

// EnqueueJob adds a job to a Redis Stream
func (h *RedisHarness) EnqueueJob(ctx context.Context, queue string, job *Job) (string, error) {
	if job.ID == "" {
		job.ID = uuid.New().String()
	}

	payloadBytes, err := json.Marshal(job.Payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	result, err := h.client.XAdd(ctx, &redis.XAddArgs{
		Stream: queue,
		Values: map[string]interface{}{
			"jobId":   job.ID,
			"type":    job.Type,
			"payload": string(payloadBytes),
		},
	}).Result()

	if err != nil {
		return "", fmt.Errorf("failed to enqueue job: %w", err)
	}

	return result, nil
}

// WaitForJobResult waits for a job result via Pub/Sub
func (h *RedisHarness) WaitForJobResult(ctx context.Context, jobID string, timeout time.Duration) ([]string, error) {
	channel := fmt.Sprintf("stream:v1:%s", jobID)

	pubsub := h.client.Subscribe(ctx, channel)
	defer pubsub.Close()

	// Wait for subscription confirmation
	_, err := pubsub.Receive(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe: %w", err)
	}

	var results []string
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ch := pubsub.Channel()
	for {
		select {
		case msg := <-ch:
			results = append(results, msg.Payload)
			// Check if this is a completion message
			var data map[string]interface{}
			if err := json.Unmarshal([]byte(msg.Payload), &data); err == nil {
				if status, ok := data["status"].(string); ok && (status == "completed" || status == "failed") {
					return results, nil
				}
			}
		case <-timeoutCtx.Done():
			return results, fmt.Errorf("timeout waiting for job result")
		}
	}
}

// GetJobStatus retrieves job status from Redis
func (h *RedisHarness) GetJobStatus(ctx context.Context, jobID string) (string, error) {
	key := fmt.Sprintf("job:%s:status", jobID)
	status, err := h.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "unknown", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to get job status: %w", err)
	}
	return status, nil
}

// SetDeviceAuthState sets device authorization state in Redis
func (h *RedisHarness) SetDeviceAuthState(ctx context.Context, deviceCode, state string, data map[string]interface{}) error {
	key := fmt.Sprintf("device:%s", deviceCode)
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal data: %w", err)
	}

	return h.client.Set(ctx, key, string(dataBytes), 10*time.Minute).Err()
}

// GetDeviceAuthState retrieves device authorization state from Redis
func (h *RedisHarness) GetDeviceAuthState(ctx context.Context, deviceCode string) (map[string]interface{}, error) {
	key := fmt.Sprintf("device:%s", deviceCode)
	data, err := h.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get device state: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(data), &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal data: %w", err)
	}

	return result, nil
}

// CreateConsumerGroup creates a Redis consumer group
func (h *RedisHarness) CreateConsumerGroup(ctx context.Context, stream, group string) error {
	err := h.client.XGroupCreateMkStream(ctx, stream, group, "0").Err()
	if err != nil && err.Error() != "BUSYGROUP Consumer Group name already exists" {
		return err
	}
	return nil
}

// Cleanup removes test keys from Redis
func (h *RedisHarness) Cleanup(ctx context.Context, patterns ...string) error {
	for _, pattern := range patterns {
		keys, err := h.client.Keys(ctx, pattern).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := h.client.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Close closes the Redis connection
func (h *RedisHarness) Close() error {
	return h.client.Close()
}

// Client returns the underlying Redis client
func (h *RedisHarness) Client() *redis.Client {
	return h.client
}
