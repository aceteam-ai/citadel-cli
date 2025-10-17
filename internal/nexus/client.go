// internal/nexus/client.go
package nexus

import (
	"embed"
	"encoding/json"
	"fmt"
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
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
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

// --- MOCK IMPLEMENTATION ---
var mockJobs []Job
var jobIndex = 0

//go:embed mock_jobs.json
var MockJobsFS embed.FS

func LoadMockJobs() {
	if mockJobs == nil { // Only load once
		fmt.Println("[DEBUG] NexusClient: Loading mock jobs from mock_jobs.json...")
		data, err := MockJobsFS.ReadFile("mock_jobs.json")
		if err != nil {
			panic(fmt.Sprintf("failed to read embedded mock_jobs.json: %v", err))
		}
		if err := json.Unmarshal(data, &mockJobs); err != nil {
			panic(fmt.Sprintf("failed to parse mock_jobs.json: %v", err))
		}
		fmt.Printf("[DEBUG] NexusClient: Loaded %d mock jobs.\n", len(mockJobs))
	}
}

func (c *Client) GetNextJob() (*Job, error) {
	LoadMockJobs() // Ensure jobs are loaded
	if jobIndex < len(mockJobs) {
		job := mockJobs[jobIndex]
		jobIndex++
		fmt.Printf("[DEBUG] NexusClient: Serving mock job %d of %d (Type: %s).\n", jobIndex, len(mockJobs), job.Type)
		return &job, nil
	}
	// After all jobs are served, simulate an empty queue.
	return nil, nil
}

func (c *Client) UpdateJobStatus(jobID string, update JobStatusUpdate) error {
	payload, _ := json.Marshal(update)
	fmt.Printf("[DEBUG] NexusClient: Reporting status for job '%s':\n---\n%s\n---\n", jobID, string(payload))
	return nil
}
