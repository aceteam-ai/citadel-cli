// Package bench provides benchmarking tools for OpenAI-compatible inference endpoints.
package bench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// BenchmarkResult holds the outcome of a single benchmark run.
type BenchmarkResult struct {
	Endpoint    string        `json:"endpoint"`
	Model       string        `json:"model"`
	Turns       int           `json:"turns"`
	Concurrency int           `json:"concurrency"`
	MaxTokens   int           `json:"max_tokens"`
	TurnResults []TurnResult  `json:"turn_results"`
	TotalTime   time.Duration `json:"total_time_ns"`
	// Aggregate metrics (across all turns and concurrent requests)
	AvgTokensPerSec float64       `json:"avg_tokens_per_sec"`
	AvgLatency      time.Duration `json:"avg_latency_ns"`
	AvgTTFT         time.Duration `json:"avg_ttft_ns"`
	TotalTokens     int           `json:"total_tokens"`
	Error           string        `json:"error,omitempty"`
}

// TurnResult holds metrics for a single conversation turn.
type TurnResult struct {
	Turn            int           `json:"turn"`
	TokensPerSec    float64       `json:"tokens_per_sec"`
	Latency         time.Duration `json:"latency_ns"`
	TTFT            time.Duration `json:"ttft_ns"`
	CompletionTokens int          `json:"completion_tokens"`
	Content         string        `json:"content"`
	Error           string        `json:"error,omitempty"`
}

// chatMessage represents a message in the OpenAI chat format.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the OpenAI-compatible chat completion request body.
type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	MaxTokens     int           `json:"max_tokens"`
	Stream        bool          `json:"stream"`
	StreamOptions *streamOpts   `json:"stream_options,omitempty"`
}

type streamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

// streamChunk represents a single chunk from SSE streaming.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// modelsResponse is the shape of GET /v1/models.
type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// AutoDetectModel queries /v1/models and returns the first model ID.
func AutoDetectModel(ctx context.Context, endpoint string) (string, error) {
	url := strings.TrimRight(endpoint, "/")
	// Strip /v1/chat/completions or /v1/* suffix to get base URL
	if idx := strings.Index(url, "/v1/"); idx != -1 {
		url = url[:idx]
	}
	url += "/v1/models"

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to query %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, string(body))
	}

	var models modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return "", fmt.Errorf("failed to parse models response: %w", err)
	}

	if len(models.Data) == 0 {
		return "", fmt.Errorf("no models available at %s", url)
	}

	return models.Data[0].ID, nil
}

// RunBenchmark executes a benchmark against the given endpoint.
// It sends `turns` sequential chat completion requests, building up
// conversation history. With concurrency > 1, multiple independent
// conversations run in parallel.
//
// Tokens/sec is computed using server-reported usage.completion_tokens
// divided by generation time (total latency minus TTFT).
func RunBenchmark(ctx context.Context, endpoint, model string, maxTokens, turns, concurrency int) *BenchmarkResult {
	result := &BenchmarkResult{
		Endpoint:    endpoint,
		Model:       model,
		Turns:       turns,
		Concurrency: concurrency,
		MaxTokens:   maxTokens,
	}

	// Ensure endpoint ends with /v1/chat/completions
	chatURL := normalizeEndpoint(endpoint)

	start := time.Now()

	if concurrency <= 1 {
		result.TurnResults = runConversation(ctx, chatURL, model, maxTokens, turns)
	} else {
		result.TurnResults = runConcurrent(ctx, chatURL, model, maxTokens, turns, concurrency)
	}

	result.TotalTime = time.Since(start)

	// Compute aggregate metrics
	computeAggregates(result)

	return result
}

// normalizeEndpoint ensures the URL ends with /v1/chat/completions.
func normalizeEndpoint(endpoint string) string {
	endpoint = strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(endpoint, "/v1/chat/completions") {
		return endpoint
	}
	if strings.HasSuffix(endpoint, "/v1") {
		return endpoint + "/chat/completions"
	}
	return endpoint + "/v1/chat/completions"
}

// runConversation runs a single multi-turn conversation and returns per-turn results.
func runConversation(ctx context.Context, chatURL, model string, maxTokens, turns int) []TurnResult {
	results := make([]TurnResult, 0, turns)
	history := []chatMessage{
		{Role: "user", Content: "Write a short story about a robot learning to paint. Be creative and descriptive."},
	}

	for turn := 0; turn < turns; turn++ {
		tr := executeTurn(ctx, chatURL, model, maxTokens, history, turn+1)
		results = append(results, tr)

		if tr.Error != "" {
			break
		}

		// Build conversation history for multi-turn
		history = append(history, chatMessage{Role: "assistant", Content: tr.Content})
		if turn < turns-1 {
			history = append(history, chatMessage{Role: "user", Content: "Continue the story. Add more detail and develop the characters further."})
		}
	}

	return results
}

// runConcurrent runs multiple independent conversations in parallel.
func runConcurrent(ctx context.Context, chatURL, model string, maxTokens, turns, concurrency int) []TurnResult {
	var mu sync.Mutex
	var wg sync.WaitGroup
	allResults := make([]TurnResult, 0, turns*concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			results := runConversation(ctx, chatURL, model, maxTokens, turns)

			// Tag results with worker ID for identification
			for j := range results {
				results[j].Turn = workerID*turns + results[j].Turn
			}

			mu.Lock()
			allResults = append(allResults, results...)
			mu.Unlock()
		}(i)
	}

	wg.Wait()
	return allResults
}

// executeTurn sends a single streaming chat completion request and measures performance.
func executeTurn(ctx context.Context, chatURL, model string, maxTokens int, history []chatMessage, turnNum int) TurnResult {
	tr := TurnResult{Turn: turnNum}

	body := chatRequest{
		Model:     model,
		Messages:  history,
		MaxTokens: maxTokens,
		Stream:    true,
		StreamOptions: &streamOpts{
			IncludeUsage: true,
		},
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		tr.Error = fmt.Sprintf("failed to marshal request: %v", err)
		return tr
	}

	req, err := http.NewRequestWithContext(ctx, "POST", chatURL, bytes.NewReader(bodyBytes))
	if err != nil {
		tr.Error = fmt.Sprintf("failed to create request: %v", err)
		return tr
	}
	req.Header.Set("Content-Type", "application/json")

	requestStart := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tr.Error = fmt.Sprintf("request failed: %v", err)
		tr.Latency = time.Since(requestStart)
		return tr
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		tr.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(errBody))
		tr.Latency = time.Since(requestStart)
		return tr
	}

	// Parse SSE stream
	scanner := bufio.NewScanner(resp.Body)
	var content strings.Builder
	firstToken := true
	completionTokens := 0
	contentChunks := 0

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		// Check for usage in the chunk (may appear in final chunk)
		if chunk.Usage != nil && chunk.Usage.CompletionTokens > 0 {
			completionTokens = chunk.Usage.CompletionTokens
		}

		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			if firstToken {
				tr.TTFT = time.Since(requestStart)
				firstToken = false
			}
			content.WriteString(chunk.Choices[0].Delta.Content)
			contentChunks++
		}
	}

	tr.Latency = time.Since(requestStart)
	tr.Content = content.String()

	// Use server-reported token count, fall back to chunk count
	if completionTokens > 0 {
		tr.CompletionTokens = completionTokens
	} else {
		tr.CompletionTokens = contentChunks
	}

	// Compute tokens/sec using generation time (latency - TTFT)
	generationTime := tr.Latency - tr.TTFT
	if generationTime > 0 && tr.CompletionTokens > 0 {
		tr.TokensPerSec = float64(tr.CompletionTokens) / generationTime.Seconds()
	}

	return tr
}

// computeAggregates fills in the aggregate fields on a BenchmarkResult.
func computeAggregates(result *BenchmarkResult) {
	if len(result.TurnResults) == 0 {
		return
	}

	var totalTokensPerSec float64
	var totalLatency time.Duration
	var totalTTFT time.Duration
	successCount := 0

	for _, tr := range result.TurnResults {
		if tr.Error != "" {
			if result.Error == "" {
				result.Error = tr.Error
			}
			continue
		}
		successCount++
		totalTokensPerSec += tr.TokensPerSec
		totalLatency += tr.Latency
		totalTTFT += tr.TTFT
		result.TotalTokens += tr.CompletionTokens
	}

	if successCount > 0 {
		result.AvgTokensPerSec = totalTokensPerSec / float64(successCount)
		result.AvgLatency = totalLatency / time.Duration(successCount)
		result.AvgTTFT = totalTTFT / time.Duration(successCount)
	}
}
