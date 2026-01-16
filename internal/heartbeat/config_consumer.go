// Package heartbeat provides periodic status reporting to the AceTeam control plane.
//
// This file implements the config queue consumer for receiving device configuration
// jobs from the Python worker via Redis Streams.
package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	// ConfigQueueName is the Redis stream for config jobs
	ConfigQueueName = "jobs:v1:config"

	// ConfigConsumerGroup is the consumer group for config jobs
	ConfigConsumerGroup = "citadel-config-consumers"
)

// ConfigConsumer consumes device configuration jobs from Redis Streams.
// It runs in parallel with the main job worker to apply device configs
// received from the onboarding wizard.
type ConfigConsumer struct {
	client        *redis.Client
	workerID      string
	queueName     string
	consumerGroup string
	blockMs       int
	configHandler *jobs.ConfigHandler
}

// ConfigConsumerConfig holds configuration for the ConfigConsumer.
type ConfigConsumerConfig struct {
	// RedisURL is the Redis connection URL
	RedisURL string

	// RedisPassword is the Redis password (optional)
	RedisPassword string

	// ConfigDir is where citadel config is stored (optional, defaults to ~/citadel-node)
	ConfigDir string

	// BlockMs is how long to wait for a job before returning (default: 5000)
	BlockMs int
}

// NewConfigConsumer creates a new config queue consumer.
func NewConfigConsumer(cfg ConfigConsumerConfig) (*ConfigConsumer, error) {
	if cfg.BlockMs == 0 {
		cfg.BlockMs = 5000
	}

	// Parse Redis URL
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Redis URL: %w", err)
	}

	if cfg.RedisPassword != "" {
		opts.Password = cfg.RedisPassword
	}

	client := redis.NewClient(opts)

	return &ConfigConsumer{
		client:        client,
		workerID:      fmt.Sprintf("citadel-config-%s", uuid.New().String()[:8]),
		queueName:     ConfigQueueName,
		consumerGroup: ConfigConsumerGroup,
		blockMs:       cfg.BlockMs,
		configHandler: jobs.NewConfigHandler(cfg.ConfigDir),
	}, nil
}

// Start begins consuming config jobs from Redis.
// This method blocks until the context is cancelled.
func (c *ConfigConsumer) Start(ctx context.Context) error {
	// Verify connection
	if err := c.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}

	// Create consumer group if it doesn't exist
	err := c.client.XGroupCreateMkStream(ctx, c.queueName, c.consumerGroup, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("failed to create consumer group: %w", err)
	}

	// Main consumption loop
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			job, msgID, err := c.readJob(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				fmt.Printf("   - ⚠️ Config consumer read error: %v\n", err)
				continue
			}

			if job == nil {
				continue // No job available
			}

			// Process the config job
			c.processJob(ctx, job, msgID)
		}
	}
}

// readJob reads the next config job from the stream.
func (c *ConfigConsumer) readJob(ctx context.Context) (*nexus.Job, string, error) {
	streams, err := c.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    c.consumerGroup,
		Consumer: c.workerID,
		Streams:  []string{c.queueName, ">"},
		Count:    1,
		Block:    time.Duration(c.blockMs) * time.Millisecond,
	}).Result()

	if err != nil {
		if err == redis.Nil {
			return nil, "", nil // No message available
		}
		return nil, "", fmt.Errorf("failed to read from config stream: %w", err)
	}

	if len(streams) == 0 || len(streams[0].Messages) == 0 {
		return nil, "", nil
	}

	msg := streams[0].Messages[0]
	job, err := c.parseMessage(msg)
	return job, msg.ID, err
}

// parseMessage converts a Redis stream message to a nexus.Job.
func (c *ConfigConsumer) parseMessage(msg redis.XMessage) (*nexus.Job, error) {
	job := &nexus.Job{
		Payload: make(map[string]string),
	}

	// Extract jobId
	if jobID, ok := msg.Values["jobId"].(string); ok {
		job.ID = jobID
	}

	// Extract type
	if jobType, ok := msg.Values["type"].(string); ok {
		job.Type = jobType
	}

	// Parse payload - it could be a JSON string or individual fields
	if payloadStr, ok := msg.Values["payload"].(string); ok {
		// Try to parse as JSON object containing config
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(payloadStr), &payload); err == nil {
			// Extract config from payload and serialize it back for the handler
			if configData, ok := payload["config"]; ok {
				configJSON, _ := json.Marshal(configData)
				job.Payload["config"] = string(configJSON)
			} else {
				// The entire payload might be the config
				job.Payload["config"] = payloadStr
			}
		} else {
			// Use as-is
			job.Payload["config"] = payloadStr
		}
	}

	// Also check for top-level config field
	if configStr, ok := msg.Values["config"].(string); ok {
		job.Payload["config"] = configStr
	}

	return job, nil
}

// processJob applies the device configuration.
func (c *ConfigConsumer) processJob(ctx context.Context, job *nexus.Job, msgID string) {
	fmt.Printf("   - 📥 Config job received: %s (type: %s)\n", job.ID, job.Type)

	// Only process APPLY_DEVICE_CONFIG jobs
	if job.Type != "APPLY_DEVICE_CONFIG" {
		fmt.Printf("   - ⚠️ Ignoring unknown config job type: %s\n", job.Type)
		c.ackJob(ctx, msgID)
		return
	}

	// Execute the config handler
	jobCtx := jobs.JobContext{}
	output, err := c.configHandler.Execute(jobCtx, job)

	if err != nil {
		fmt.Printf("   - ❌ Config job failed: %v\n", err)
		// Don't ACK - let it retry
		return
	}

	fmt.Printf("   - ✅ Config applied: %s\n", string(output))
	c.ackJob(ctx, msgID)
}

// ackJob acknowledges a processed message.
func (c *ConfigConsumer) ackJob(ctx context.Context, msgID string) {
	if err := c.client.XAck(ctx, c.queueName, c.consumerGroup, msgID).Err(); err != nil {
		fmt.Printf("   - ⚠️ Failed to ACK config job: %v\n", err)
	}
}

// Close closes the Redis connection.
func (c *ConfigConsumer) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

// QueueName returns the config queue name.
func (c *ConfigConsumer) QueueName() string {
	return c.queueName
}
