// internal/nexus/client.go
package nexus

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Node struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	IPAddress string    `json:"ip_address"`
	Status    string    `json:"status"`
	LastSeen  time.Time `json:"last_seen"`
}

type Job struct {
	ID      string            `json:"id"`
	Type    string            `json:"type"`
	Payload map[string]string `json:"payload"`
}

type JobStatusUpdate struct {
	Status string `json:"status"`
	Output string `json:"output"`
}

type Client struct {
	baseURL    string
	httpClient *http.Client
	mockMode   bool
	mockJobs   []Job
	mockIndex  int
}

// ClientOption is a functional option for configuring Client.
type ClientOption func(*Client)

// WithMockMode enables mock mode for testing.
func WithMockMode() ClientOption {
	return func(c *Client) {
		c.mockMode = true
	}
}

func NewClient(baseURL string, opts ...ClientOption) *Client {
	c := &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.mockMode {
		c.loadMockJobs()
	}
	return c
}

func (c *Client) ListNodes() ([]Node, error) {
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

func (c *Client) GetNextJob() (*Job, error) {
	if c.mockMode {
		return c.getNextMockJob()
	}
	return c.getNextJobHTTP()
}

func (c *Client) getNextJobHTTP() (*Job, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/api/v1/jobs/next", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	// 204 No Content means no job available
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("nexus API returned %s: %s", resp.Status, string(body))
	}

	var job Job
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return nil, fmt.Errorf("failed to decode job response: %w", err)
	}
	return &job, nil
}

func (c *Client) UpdateJobStatus(jobID string, update JobStatusUpdate) error {
	if c.mockMode {
		return c.updateMockJobStatus(jobID, update)
	}
	return c.updateJobStatusHTTP(jobID, update)
}

func (c *Client) updateJobStatusHTTP(jobID string, update JobStatusUpdate) error {
	payload, err := json.Marshal(update)
	if err != nil {
		return fmt.Errorf("failed to marshal status update: %w", err)
	}

	req, err := http.NewRequest("POST", c.baseURL+"/api/v1/jobs/"+jobID+"/status", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("nexus API returned %s: %s", resp.Status, string(body))
	}
	return nil
}

// --- MOCK IMPLEMENTATION (for --test mode) ---

//go:embed mock_jobs.json
var MockJobsFS embed.FS

func (c *Client) loadMockJobs() {
	fmt.Println("[TEST] Loading mock jobs...")
	data, err := MockJobsFS.ReadFile("mock_jobs.json")
	if err != nil {
		fmt.Printf("[TEST] Warning: failed to read mock_jobs.json: %v\n", err)
		return
	}
	if err := json.Unmarshal(data, &c.mockJobs); err != nil {
		fmt.Printf("[TEST] Warning: failed to parse mock_jobs.json: %v\n", err)
		return
	}
	fmt.Printf("[TEST] Loaded %d mock jobs\n", len(c.mockJobs))
}

func (c *Client) getNextMockJob() (*Job, error) {
	if c.mockIndex < len(c.mockJobs) {
		job := c.mockJobs[c.mockIndex]
		c.mockIndex++
		fmt.Printf("[TEST] Serving mock job %d/%d (type: %s)\n", c.mockIndex, len(c.mockJobs), job.Type)
		return &job, nil
	}
	// No more mock jobs
	return nil, nil
}

func (c *Client) updateMockJobStatus(jobID string, update JobStatusUpdate) error {
	fmt.Printf("[TEST] Job %s status: %s\n", jobID, update.Status)
	return nil
}
