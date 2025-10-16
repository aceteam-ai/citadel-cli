// internal/nexus/client.go
package nexus

import (
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
var jobServed = false

func (c *Client) GetNextJob() (*Job, error) {
	if !jobServed {
		jobServed = true
		fmt.Println("[DEBUG] NexusClient: Serving one-time mock OLLAMA_INFERENCE job.")
		return &Job{
			ID:   "job-ollama-456",
			Type: "OLLAMA_INFERENCE",
			Payload: map[string]string{
				"model":  "llama2", // Make sure this model is available on the node
				"prompt": "In one sentence, what is the purpose of a sovereign compute fabric?",
			},
		}, nil
	}
	return nil, nil
}

func (c *Client) UpdateJobStatus(jobID string, update JobStatusUpdate) error {
	payload, _ := json.Marshal(update)
	fmt.Printf("[DEBUG] NexusClient: Reporting status for job '%s':\n---\n%s\n---\n", jobID, string(payload))
	return nil
}
