// Package heartbeat provides periodic status reporting to the AceTeam control plane.
//
// This file implements the config Pub/Sub subscriber for receiving real-time device
// configuration updates from the AceTeam backend via Redis Pub/Sub.
package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/redis/go-redis/v9"
)

// nodeIDPattern is defined in redis.go and shared across the package

// Resource limits for config messages to prevent DoS attacks
const (
	maxDeviceNameLength = 256
	maxServicesCount    = 50
	maxTagsCount        = 100
	maxSSHUsersCount    = 50
)

// ConfigMessage represents a config update received via Pub/Sub.
type ConfigMessage struct {
	Type      string       `json:"type"`      // "config_updated"
	NodeID    string       `json:"nodeId"`
	Config    ConfigUpdate `json:"config"`
	UpdatedAt string       `json:"updatedAt"`
}

// ConfigUpdate contains the configuration fields from the Pub/Sub message.
type ConfigUpdate struct {
	DeviceName              string   `json:"deviceName"`
	Services                []string `json:"services"`
	AutoStartServices       bool     `json:"autoStartServices"`
	SSHEnabled              bool     `json:"sshEnabled"`
	SSHAllowedUsers         []string `json:"sshAllowedUsers"`
	SSHDeviceKeys           []string `json:"sshDeviceKeys"`
	ShareInferenceWithOrg   bool     `json:"shareInferenceWithOrg"`
	VisibleToTeam           bool     `json:"visibleToTeam"`
	CustomTags              []string `json:"customTags"`
	HealthMonitoringEnabled bool     `json:"healthMonitoringEnabled"`
	AlertOnOffline          bool     `json:"alertOnOffline"`
	AlertOnHighTemp         bool     `json:"alertOnHighTemp"`
	Status                  string   `json:"status"`
	UpdatedAt               string   `json:"updatedAt"`
}

// ConfigSubscriber subscribes to real-time config updates via Redis Pub/Sub.
// It listens on channel "config:node:{nodeId}" and applies configuration changes
// using the existing ConfigHandler.
type ConfigSubscriber struct {
	client        *redis.Client
	nodeID        string
	channel       string
	configHandler *jobs.ConfigHandler

	// Backoff configuration for reconnection
	minBackoff time.Duration
	maxBackoff time.Duration
	backoff    time.Duration

	// Log callback (optional, for TUI mode)
	logFn func(level, msg string)
}

// ConfigSubscriberConfig holds configuration for the ConfigSubscriber.
type ConfigSubscriberConfig struct {
	// RedisURL is the Redis connection URL
	RedisURL string

	// RedisPassword is the Redis password (optional)
	RedisPassword string

	// NodeID is the Headscale node ID or device name
	NodeID string

	// ConfigDir is where citadel config is stored (optional, defaults to ~/citadel-node)
	ConfigDir string

	// LogFn is an optional callback for logging (if nil, prints to stdout)
	LogFn func(level, msg string)
}

// NewConfigSubscriber creates a new config Pub/Sub subscriber.
func NewConfigSubscriber(cfg ConfigSubscriberConfig) (*ConfigSubscriber, error) {
	if cfg.RedisURL == "" {
		return nil, fmt.Errorf("RedisURL is required")
	}
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("NodeID is required")
	}
	if !nodeIDPattern.MatchString(cfg.NodeID) {
		return nil, fmt.Errorf("invalid NodeID format: must be 1-64 alphanumeric characters, dots, hyphens, or underscores")
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

	return &ConfigSubscriber{
		client:        client,
		nodeID:        cfg.NodeID,
		channel:       fmt.Sprintf("config:node:%s", cfg.NodeID),
		configHandler: jobs.NewConfigHandler(cfg.ConfigDir),
		minBackoff:    1 * time.Second,
		maxBackoff:    5 * time.Minute,
		backoff:       1 * time.Second,
		logFn:         cfg.LogFn,
	}, nil
}

// log outputs a message - uses logFn callback if set, otherwise prints to stdout.
func (s *ConfigSubscriber) log(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if s.logFn != nil {
		s.logFn(level, msg)
	} else {
		fmt.Printf("%s\n", msg)
	}
}

// Start begins subscribing to config updates.
// This method blocks until the context is cancelled, automatically reconnecting
// with exponential backoff on connection failures.
func (s *ConfigSubscriber) Start(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := s.subscribe(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Exponential backoff on failures
			s.backoff = min(s.backoff*2, s.maxBackoff)
			s.log("warning", "   - Config subscription error (retrying in %v): %v", s.backoff, err)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.backoff):
				continue
			}
		}
		// Reset backoff on clean disconnect (shouldn't normally happen)
		s.backoff = s.minBackoff
	}
}

// subscribe handles the actual Pub/Sub subscription and message processing.
func (s *ConfigSubscriber) subscribe(ctx context.Context) error {
	// Verify connection first
	if err := s.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}

	pubsub := s.client.Subscribe(ctx, s.channel)
	defer pubsub.Close()

	// Wait for subscription confirmation
	_, err := pubsub.Receive(ctx)
	if err != nil {
		return fmt.Errorf("failed to subscribe to %s: %w", s.channel, err)
	}

	// Reset backoff on successful subscription
	s.backoff = s.minBackoff

	// Process messages
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				return fmt.Errorf("subscription channel closed")
			}
			if err := s.handleMessage(msg); err != nil {
				// Log error but don't disconnect - continue processing other messages
				s.log("warning", "   - Warning: Failed to handle config message: %v", err)
			}
		}
	}
}

// handleMessage parses and applies a config update message.
func (s *ConfigSubscriber) handleMessage(msg *redis.Message) error {
	var configMsg ConfigMessage
	if err := json.Unmarshal([]byte(msg.Payload), &configMsg); err != nil {
		return fmt.Errorf("failed to parse config message: %w", err)
	}

	// Validate message type
	if configMsg.Type != "config_updated" {
		return fmt.Errorf("unknown message type: %s", configMsg.Type)
	}

	// Validate nodeID matches this subscriber (defense in depth)
	if configMsg.NodeID != s.nodeID {
		return fmt.Errorf("config message for different node: expected %s, got %s", s.nodeID, configMsg.NodeID)
	}

	// Validate resource limits to prevent DoS
	if err := s.validateConfigLimits(&configMsg.Config); err != nil {
		return fmt.Errorf("config validation failed: %w", err)
	}

	s.log("info", "   - Config update received")

	// Convert to DeviceConfig for existing handler
	deviceConfig := jobs.DeviceConfig{
		DeviceName:              configMsg.Config.DeviceName,
		Services:                configMsg.Config.Services,
		AutoStartServices:       configMsg.Config.AutoStartServices,
		SSHEnabled:              configMsg.Config.SSHEnabled,
		CustomTags:              configMsg.Config.CustomTags,
		HealthMonitoringEnabled: configMsg.Config.HealthMonitoringEnabled,
		AlertOnOffline:          configMsg.Config.AlertOnOffline,
		AlertOnHighTemp:         configMsg.Config.AlertOnHighTemp,
	}

	// Create job for handler
	configJSON, err := json.Marshal(deviceConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	job := &nexus.Job{
		Payload: map[string]string{"config": string(configJSON)},
	}

	// Apply config using existing handler
	_, err = s.configHandler.Execute(jobs.JobContext{}, job)
	if err != nil {
		return fmt.Errorf("failed to apply config: %w", err)
	}

	s.log("success", "   - Config applied successfully")
	return nil
}

// validateConfigLimits checks resource limits on config fields to prevent DoS attacks.
func (s *ConfigSubscriber) validateConfigLimits(config *ConfigUpdate) error {
	if len(config.DeviceName) > maxDeviceNameLength {
		return fmt.Errorf("device name too long: %d > %d", len(config.DeviceName), maxDeviceNameLength)
	}
	if len(config.Services) > maxServicesCount {
		return fmt.Errorf("too many services: %d > %d", len(config.Services), maxServicesCount)
	}
	if len(config.CustomTags) > maxTagsCount {
		return fmt.Errorf("too many tags: %d > %d", len(config.CustomTags), maxTagsCount)
	}
	if len(config.SSHAllowedUsers) > maxSSHUsersCount {
		return fmt.Errorf("too many SSH users: %d > %d", len(config.SSHAllowedUsers), maxSSHUsersCount)
	}
	return nil
}

// Close closes the Redis connection.
func (s *ConfigSubscriber) Close() error {
	if s.client != nil {
		return s.client.Close()
	}
	return nil
}

// Channel returns the Pub/Sub channel name.
func (s *ConfigSubscriber) Channel() string {
	return s.channel
}

// NodeID returns the node ID.
func (s *ConfigSubscriber) NodeID() string {
	return s.nodeID
}
