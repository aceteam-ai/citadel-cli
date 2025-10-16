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
var mockJobs = []Job{
	{
		ID:   "job-download-llama2", // Let's make the first job useful
		Type: "DOWNLOAD_MODEL",
		Payload: map[string]string{
			// Ollama doesn't use HF URLs, it uses its own registry.
			// So this job type isn't right for Ollama. Let's download the GGUF instead.
			// Let's re-order the jobs to be more logical.
			"repo_url":   "https://huggingface.co/TheBloke/Llama-2-7B-Chat-GGUF",
			"file_name":  "llama-2-7b-chat.Q4_K_M.gguf",
			"model_type": "llamacpp",
		},
	},
	{
		ID:   "job-infer-llama2",
		Type: "LLAMACPP_INFERENCE",
		Payload: map[string]string{
			"model_file": "llama-2-7b-chat.Q4_K_M.gguf",
			"prompt":     "In one sentence, what is the purpose of a sovereign compute fabric?",
		},
	},

	// {
	// 	ID:   "job-ollama-456",
	// 	Type: "OLLAMA_INFERENCE",
	// 	Payload: map[string]string{
	// 		"model":  "llama2",
	// 		"prompt": "In one sentence, what is the purpose of a sovereign compute fabric?",
	// 	},
	// },
	// {
	// 	ID:   "job-llamacpp-789",
	// 	Type: "LLAMACPP_INFERENCE",
	// 	Payload: map[string]string{
	// 		"prompt": "Write a short poem about servers and code.",
	// 	},
	// },
	// {
	// 	ID:   "job-vllm-101",
	// 	Type: "VLLM_INFERENCE",
	// 	Payload: map[string]string{
	// 		"model":  "facebook/opt-125m", // The default model in our vllm.yml
	// 		"prompt": "What are the three laws of robotics?",
	// 	},
	// },
	// {
	// 	ID:   "job-download-deepseek",
	// 	Type: "DOWNLOAD_MODEL",
	// 	Payload: map[string]string{
	// 		"repo_url":   "https://huggingface.co/unsloth/DeepSeek-R1-0528-Qwen3-8B-GGUF",
	// 		"file_name":  "DeepSeek-R1-0528-Qwen3-8B-Q4_K_M.gguf",
	// 		"model_type": "llamacpp",
	// 	},
	// },
	// {
	// 	ID:   "job-infer-deepseek",
	// 	Type: "LLAMACPP_INFERENCE",
	// 	Payload: map[string]string{
	// 		"model_file": "DeepSeek-R1-0528-Qwen3-8B-Q4_K_M.gguf",
	// 		"prompt":     "Write a short story about a sentient AI living in a compute cluster.",
	// 	},
	// },
}
var jobIndex = 0

func (c *Client) GetNextJob() (*Job, error) {
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
