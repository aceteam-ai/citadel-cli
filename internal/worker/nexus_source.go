package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// NexusSource implements JobSource for Nexus HTTP polling.
// This is the job source for user's on-premise nodes that poll Nexus for work.
type NexusSource struct {
	client       *nexus.Client
	nexusURL     string
	pollInterval time.Duration
	ticker       *time.Ticker
	mockMode     bool
}

// NexusSourceConfig holds configuration for NexusSource.
type NexusSourceConfig struct {
	// NexusURL is the Nexus server URL (e.g., "https://nexus.aceteam.ai")
	NexusURL string

	// PollInterval is how often to poll for new jobs (default: 5s)
	PollInterval time.Duration

	// MockMode enables mock job testing (--test flag)
	MockMode bool
}

// NewNexusSource creates a new Nexus HTTP polling job source.
func NewNexusSource(cfg NexusSourceConfig) *NexusSource {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Second
	}
	return &NexusSource{
		nexusURL:     cfg.NexusURL,
		pollInterval: cfg.PollInterval,
		mockMode:     cfg.MockMode,
	}
}

// Name returns the source identifier.
func (s *NexusSource) Name() string {
	return "nexus"
}

// Connect establishes connection to Nexus.
func (s *NexusSource) Connect(ctx context.Context) error {
	var opts []nexus.ClientOption
	if s.mockMode {
		opts = append(opts, nexus.WithMockMode())
	}
	s.client = nexus.NewClient(s.nexusURL, opts...)
	s.ticker = time.NewTicker(s.pollInterval)
	fmt.Printf("   - Nexus endpoint: %s\n", s.nexusURL)
	fmt.Printf("   - Poll interval: %v\n", s.pollInterval)
	return nil
}

// Next blocks until a job is available or context is cancelled.
// It polls Nexus at the configured interval.
func (s *NexusSource) Next(ctx context.Context) (*Job, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ticker.C:
		// Poll for next job
		nexusJob, err := s.client.GetNextJob()
		if err != nil {
			return nil, fmt.Errorf("failed to get next job from Nexus: %w", err)
		}

		if nexusJob == nil {
			return nil, nil // No job available
		}

		// Convert nexus.Job to worker.Job
		return s.convertJob(nexusJob), nil
	}
}

// convertJob converts a nexus.Job to a worker.Job.
func (s *NexusSource) convertJob(nj *nexus.Job) *Job {
	// Convert map[string]string to map[string]any
	payload := make(map[string]any)
	for k, v := range nj.Payload {
		payload[k] = v
	}

	return &Job{
		ID:        nj.ID,
		Type:      nj.Type,
		Payload:   payload,
		Source:    "nexus",
		MessageID: nj.ID, // For Nexus, job ID is the message ID
	}
}

// Ack acknowledges successful job completion.
// For Nexus, this reports SUCCESS status.
func (s *NexusSource) Ack(ctx context.Context, job *Job) error {
	update := nexus.JobStatusUpdate{
		Status: "SUCCESS",
		Output: "", // Output is set by handler
	}
	return s.client.UpdateJobStatus(job.ID, update)
}

// Nack indicates job failure.
// For Nexus, this reports FAILURE status.
func (s *NexusSource) Nack(ctx context.Context, job *Job, err error) error {
	update := nexus.JobStatusUpdate{
		Status: "FAILURE",
		Output: err.Error(),
	}
	return s.client.UpdateJobStatus(job.ID, update)
}

// Close cleanly disconnects from Nexus.
func (s *NexusSource) Close() error {
	if s.ticker != nil {
		s.ticker.Stop()
	}
	return nil
}

// Ensure NexusSource implements JobSource
var _ JobSource = (*NexusSource)(nil)
