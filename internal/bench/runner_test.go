package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mockSSEChunk creates a single SSE data line for a chat completion chunk.
func mockSSEChunk(content string) string {
	chunk := map[string]any{
		"choices": []map[string]any{
			{
				"delta": map[string]any{
					"content": content,
				},
			},
		},
	}
	b, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", string(b))
}

// mockSSEFinalChunk creates the final SSE chunk with usage info.
func mockSSEFinalChunk(completionTokens int) string {
	chunk := map[string]any{
		"choices": []map[string]any{
			{
				"delta":         map[string]any{},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"completion_tokens": completionTokens,
		},
	}
	b, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", string(b))
}

// newMockServer creates a test server that handles /v1/models and /v1/chat/completions.
func newMockServer(modelName string, chunkCount int, completionTokens int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": modelName},
				},
			})
		case r.URL.Path == "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming not supported", 500)
				return
			}

			for i := 0; i < chunkCount; i++ {
				fmt.Fprint(w, mockSSEChunk(fmt.Sprintf("word%d ", i)))
				flusher.Flush()
				time.Sleep(5 * time.Millisecond)
			}

			fmt.Fprint(w, mockSSEFinalChunk(completionTokens))
			flusher.Flush()

			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestAutoDetectModel(t *testing.T) {
	srv := newMockServer("test-model-7b", 3, 10)
	defer srv.Close()

	ctx := context.Background()
	model, err := AutoDetectModel(ctx, srv.URL)
	if err != nil {
		t.Fatalf("AutoDetectModel failed: %v", err)
	}
	if model != "test-model-7b" {
		t.Errorf("expected model 'test-model-7b', got '%s'", model)
	}
}

func TestAutoDetectModel_WithPathSuffix(t *testing.T) {
	srv := newMockServer("qwen2-72b", 3, 10)
	defer srv.Close()

	ctx := context.Background()
	model, err := AutoDetectModel(ctx, srv.URL+"/v1/chat/completions")
	if err != nil {
		t.Fatalf("AutoDetectModel failed: %v", err)
	}
	if model != "qwen2-72b" {
		t.Errorf("expected model 'qwen2-72b', got '%s'", model)
	}
}

func TestAutoDetectModel_NoModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer srv.Close()

	ctx := context.Background()
	_, err := AutoDetectModel(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected error for empty models list")
	}
}

func TestRunBenchmark_SingleTurn(t *testing.T) {
	srv := newMockServer("test-model", 5, 15)
	defer srv.Close()

	ctx := context.Background()
	result := RunBenchmark(ctx, srv.URL, "test-model", 50, 1, 1)

	if result.Error != "" {
		t.Fatalf("benchmark failed: %s", result.Error)
	}
	if len(result.TurnResults) != 1 {
		t.Fatalf("expected 1 turn result, got %d", len(result.TurnResults))
	}

	tr := result.TurnResults[0]
	if tr.Error != "" {
		t.Fatalf("turn failed: %s", tr.Error)
	}

	// Server-reported usage should be used
	if tr.CompletionTokens != 15 {
		t.Errorf("expected 15 completion tokens (from usage), got %d", tr.CompletionTokens)
	}

	// TTFT should be > 0 (we had content chunks)
	if tr.TTFT == 0 {
		t.Error("expected TTFT > 0")
	}

	// Tokens/sec should be positive
	if tr.TokensPerSec <= 0 {
		t.Error("expected tokens/sec > 0")
	}

	// Content should contain all chunks
	if !strings.Contains(tr.Content, "word0") {
		t.Errorf("expected content to contain 'word0', got '%s'", tr.Content)
	}

	// Aggregate should match single turn
	if result.AvgTokensPerSec != tr.TokensPerSec {
		t.Errorf("avg tokens/sec mismatch: %f != %f", result.AvgTokensPerSec, tr.TokensPerSec)
	}
}

func TestRunBenchmark_MultiTurn(t *testing.T) {
	srv := newMockServer("test-model", 3, 10)
	defer srv.Close()

	ctx := context.Background()
	result := RunBenchmark(ctx, srv.URL, "test-model", 50, 3, 1)

	if result.Error != "" {
		t.Fatalf("benchmark failed: %s", result.Error)
	}
	if len(result.TurnResults) != 3 {
		t.Fatalf("expected 3 turn results, got %d", len(result.TurnResults))
	}

	for i, tr := range result.TurnResults {
		if tr.Error != "" {
			t.Errorf("turn %d failed: %s", i+1, tr.Error)
		}
	}

	if result.TotalTokens != 30 {
		t.Errorf("expected 30 total tokens, got %d", result.TotalTokens)
	}
}

func TestRunBenchmark_Concurrent(t *testing.T) {
	srv := newMockServer("test-model", 3, 10)
	defer srv.Close()

	ctx := context.Background()
	result := RunBenchmark(ctx, srv.URL, "test-model", 50, 1, 3)

	if result.Error != "" {
		t.Fatalf("benchmark failed: %s", result.Error)
	}
	// 3 concurrent workers x 1 turn each = 3 turn results
	if len(result.TurnResults) != 3 {
		t.Fatalf("expected 3 turn results, got %d", len(result.TurnResults))
	}

	if result.TotalTokens != 30 {
		t.Errorf("expected 30 total tokens (10 per worker), got %d", result.TotalTokens)
	}
}

func TestRunBenchmark_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", 500)
	}))
	defer srv.Close()

	ctx := context.Background()
	result := RunBenchmark(ctx, srv.URL, "test-model", 50, 1, 1)

	if result.Error == "" {
		t.Fatal("expected error for 500 response")
	}
}

func TestRunBenchmark_FallbackTokenCount(t *testing.T) {
	// Server that streams chunks but does NOT include usage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		for i := 0; i < 4; i++ {
			fmt.Fprint(w, mockSSEChunk(fmt.Sprintf("tok%d ", i)))
			flusher.Flush()
		}

		// Final chunk without usage
		chunk := map[string]any{
			"choices": []map[string]any{
				{"delta": map[string]any{}, "finish_reason": "stop"},
			},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", string(b))
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	ctx := context.Background()
	result := RunBenchmark(ctx, srv.URL, "test-model", 50, 1, 1)

	tr := result.TurnResults[0]
	// Without server usage, should fall back to chunk count
	if tr.CompletionTokens != 4 {
		t.Errorf("expected 4 tokens from fallback count, got %d", tr.CompletionTokens)
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"http://localhost:8000", "http://localhost:8000/v1/chat/completions"},
		{"http://localhost:8000/", "http://localhost:8000/v1/chat/completions"},
		{"http://localhost:8000/v1", "http://localhost:8000/v1/chat/completions"},
		{"http://localhost:8000/v1/chat/completions", "http://localhost:8000/v1/chat/completions"},
	}

	for _, tt := range tests {
		result := normalizeEndpoint(tt.input)
		if result != tt.expected {
			t.Errorf("normalizeEndpoint(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestFormatReport(t *testing.T) {
	result := &BenchmarkResult{
		Endpoint:        "http://localhost:8000",
		Model:           "test-model",
		Turns:           1,
		Concurrency:     1,
		MaxTokens:       50,
		AvgTokensPerSec: 42.5,
		AvgLatency:      500 * time.Millisecond,
		AvgTTFT:         50 * time.Millisecond,
		TotalTokens:     42,
		TotalTime:       600 * time.Millisecond,
		TurnResults: []TurnResult{
			{Turn: 1, TokensPerSec: 42.5, Latency: 500 * time.Millisecond, TTFT: 50 * time.Millisecond, CompletionTokens: 42},
		},
	}

	report := FormatReport(result)
	if !strings.Contains(report, "42.5") {
		t.Error("report should contain tokens/sec value")
	}
	if !strings.Contains(report, "test-model") {
		t.Error("report should contain model name")
	}
}

func TestFormatComparison(t *testing.T) {
	a := &BenchmarkResult{
		Endpoint:        "http://a:8000",
		Model:           "model-a",
		AvgTokensPerSec: 100,
		AvgLatency:      500 * time.Millisecond,
		AvgTTFT:         50 * time.Millisecond,
		TotalTokens:     100,
		TotalTime:       1 * time.Second,
	}
	b := &BenchmarkResult{
		Endpoint:        "http://b:8000",
		Model:           "model-b",
		AvgTokensPerSec: 80,
		AvgLatency:      600 * time.Millisecond,
		AvgTTFT:         70 * time.Millisecond,
		TotalTokens:     80,
		TotalTime:       1200 * time.Millisecond,
	}

	comparison := FormatComparison(a, b)

	// A has higher tokens/sec -> A wins
	if !strings.Contains(comparison, "A wins") {
		t.Error("comparison should show A as winner for tokens/sec")
	}
}

func TestFormatJSON(t *testing.T) {
	result := &BenchmarkResult{
		Endpoint: "http://localhost:8000",
		Model:    "test-model",
	}

	jsonStr, err := FormatJSON(result)
	if err != nil {
		t.Fatalf("FormatJSON failed: %v", err)
	}
	if !strings.Contains(jsonStr, "test-model") {
		t.Error("JSON output should contain model name")
	}
}

func TestPickWinner(t *testing.T) {
	// Higher is better
	if w := pickWinner(100, 80, true); !strings.Contains(w, "A wins") {
		t.Errorf("expected A wins for higher-is-better with a=100, b=80, got %q", w)
	}
	if w := pickWinner(80, 100, true); !strings.Contains(w, "B wins") {
		t.Errorf("expected B wins for higher-is-better with a=80, b=100, got %q", w)
	}

	// Lower is better
	if w := pickWinner(50, 80, false); !strings.Contains(w, "A wins") {
		t.Errorf("expected A wins for lower-is-better with a=50, b=80, got %q", w)
	}
	if w := pickWinner(80, 50, false); !strings.Contains(w, "B wins") {
		t.Errorf("expected B wins for lower-is-better with a=80, b=50, got %q", w)
	}

	// Tie
	if w := pickWinner(50, 50, true); !strings.Contains(w, "tie") {
		t.Errorf("expected tie, got %q", w)
	}
}
