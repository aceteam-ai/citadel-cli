// internal/nexus/client.go
package nexus

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Node represents a single compute node in the Nexus.
type Node struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	IPAddress string    `json:"ip_address"`
	Status    string    `json:"status"`
	LastSeen  time.Time `json:"last_seen"`
}

// Job represents a unit of work sent from Nexus to an agent.
type Job struct {
	ID      string            `json:"id"`
	Type    string            `json:"type"` // e.g., "SHELL_COMMAND"
	Payload map[string]string `json:"payload"`
}

// JobStatusUpdate is the payload sent back to Nexus to report a job's result.
type JobStatusUpdate struct {
	Status string `json:"status"` // "SUCCESS" or "FAILURE"
	Output string `json:"output"`
}

// Client is a client for interacting with the Nexus API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a new Nexus API client.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// ListNodes fetches all nodes from the Nexus.
// This is now a REAL implementation.
func (c *Client) ListNodes() ([]Node, error) {
	// Rationale: The Nexus API server should also be on the Tailnet.
	// When this request is made over Tailscale, Tailscale automatically adds
	// identity headers. The Nexus server uses these headers for authentication,
	// eliminating the need for API keys in the client.
	req, err := http.NewRequest("GET", c.baseURL+"/api/v1/nodes", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nexus API returned non-200 status: %s", resp.Status)
	}

	var nodes []Node
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		return nil, fmt.Errorf("failed to decode response body: %w", err)
	}

	return nodes, nil
}

// GetNextJob fetches the next available job for this agent.
// --- MOCK IMPLEMENTATION ---
var jobServed = false // Simulate a queue with only one job
func (c *Client) GetNextJob() (*Job, error) {
	if !jobServed {
		jobServed = true
		fmt.Println("[DEBUG] NexusClient: Serving one-time mock job.")
		return &Job{
			ID:   "job-abc-123",
			Type: "SHELL_COMMAND",
			Payload: map[string]string{
				"command": "ls -la /",
			},
		}, nil
	}
	// After the first call, simulate an empty queue.
	return nil, nil
}

// UpdateJobStatus reports the result of a job back to Nexus.
// --- MOCK IMPLEMENTATION ---
func (c *Client) UpdateJobStatus(jobID string, update JobStatusUpdate) error {
	payload, _ := json.Marshal(update)
	fmt.Printf("[DEBUG] NexusClient: Reporting status for job '%s': %s\n", jobID, string(payload))
	// In a real implementation, this would be an HTTP PUT or POST to a URL like:
	// c.baseURL + "/api/v1/jobs/" + jobID + "/status"
	return nil
}
