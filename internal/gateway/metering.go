package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MeteringMiddleware intercepts OpenAI-compatible API responses to extract
// token usage and record billing transactions.
type MeteringMiddleware struct {
	next    http.Handler
	ledger  *Ledger
	acet    *ACETClient // may be nil if settlement is disabled
	tier    PricingTier

	// Accumulated stats (in-process, for the gateway's own use)
	mu           sync.Mutex
	totalIn      int
	totalOut     int
	totalCost    int
	requestCount int
}

// NewMeteringMiddleware wraps a handler with token metering.
// tier determines the ACET pricing. acet may be nil to skip settlement.
func NewMeteringMiddleware(next http.Handler, ledger *Ledger, acet *ACETClient, tier PricingTier) *MeteringMiddleware {
	return &MeteringMiddleware{
		next:   next,
		ledger: ledger,
		acet:   acet,
		tier:   tier,
	}
}

// InProcessStats returns stats accumulated in this process (not from disk).
func (m *MeteringMiddleware) InProcessStats() (totalIn, totalOut, totalCost, requestCount int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.totalIn, m.totalOut, m.totalCost, m.requestCount
}

func (m *MeteringMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only meter OpenAI-compatible endpoints
	if !isMeteredPath(r.URL.Path) {
		m.next.ServeHTTP(w, r)
		return
	}

	start := time.Now()

	// Extract consumer key from Authorization header
	consumerKey := extractConsumerKey(r)

	// Detect if client requested streaming
	isStream := false
	if r.Body != nil {
		// Peek at the body to check for "stream": true
		bodyBytes, err := io.ReadAll(r.Body)
		if err == nil {
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			isStream = detectStream(bodyBytes)
		}
	}

	if isStream {
		m.handleStreamingResponse(w, r, start, consumerKey)
	} else {
		m.handleNonStreamingResponse(w, r, start, consumerKey)
	}
}

// handleNonStreamingResponse captures the full response body to extract usage.
func (m *MeteringMiddleware) handleNonStreamingResponse(w http.ResponseWriter, r *http.Request, start time.Time, consumerKey string) {
	rec := &responseRecorder{
		ResponseWriter: w,
		body:           &bytes.Buffer{},
		statusCode:     http.StatusOK,
	}

	m.next.ServeHTTP(rec, r)

	latency := time.Since(start).Seconds() * 1000

	// Extract usage from response
	usage := extractUsageFromBody(rec.body.Bytes())
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		return // no usage data, skip metering
	}

	m.recordUsage(usage, r.URL.Path, consumerKey, latency)
}

// handleStreamingResponse tees SSE chunks to the client while accumulating
// token counts from the final usage chunk.
func (m *MeteringMiddleware) handleStreamingResponse(w http.ResponseWriter, r *http.Request, start time.Time, consumerKey string) {
	rec := &streamRecorder{
		ResponseWriter: w,
		flusher:        w.(http.Flusher),
	}

	m.next.ServeHTTP(rec, r)

	latency := time.Since(start).Seconds() * 1000

	// Parse accumulated SSE data for usage
	usage := rec.usage
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		return
	}

	m.recordUsage(usage, r.URL.Path, consumerKey, latency)
}

func (m *MeteringMiddleware) recordUsage(usage openAIUsage, path, consumerKey string, latencyMs float64) {
	cost := CalculateACETCost(m.tier, usage.PromptTokens, usage.CompletionTokens)

	// Update in-process stats
	m.mu.Lock()
	m.totalIn += usage.PromptTokens
	m.totalOut += usage.CompletionTokens
	m.totalCost += cost
	m.requestCount++
	m.mu.Unlock()

	tx := Transaction{
		Timestamp:   time.Now(),
		Model:       usage.Model,
		TokensIn:    usage.PromptTokens,
		TokensOut:   usage.CompletionTokens,
		ACETCost:    cost,
		ConsumerKey: consumerKey,
		Latency:     latencyMs,
		Path:        path,
	}

	if err := m.ledger.Record(tx); err != nil {
		log.Printf("[Gateway] ledger write error: %v", err)
	}

	// Settle with platform (async, non-blocking)
	if m.acet != nil {
		go func() {
			if err := m.acet.Settle(usage.Model, usage.PromptTokens, usage.CompletionTokens, cost, consumerKey); err != nil {
				log.Printf("[Gateway] ACET settlement queued: %v", err)
			}
		}()
	}
}

// openAIUsage holds token counts from an OpenAI-compatible response.
type openAIUsage struct {
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	Model            string `json:"-"` // extracted from the parent object
}

// openAIResponse is the minimal structure to extract usage from a response.
type openAIResponse struct {
	Model string       `json:"model"`
	Usage *openAIUsage `json:"usage,omitempty"`
}

// openAIStreamChunk is a streaming chunk that may contain usage info.
type openAIStreamChunk struct {
	Model string       `json:"model"`
	Usage *openAIUsage `json:"usage,omitempty"`
}

func extractUsageFromBody(body []byte) openAIUsage {
	var resp openAIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return openAIUsage{}
	}
	if resp.Usage == nil {
		return openAIUsage{}
	}
	usage := *resp.Usage
	usage.Model = resp.Model
	return usage
}

func extractUsageFromSSELine(line []byte) (openAIUsage, bool) {
	// SSE lines: "data: {json}" or "data: [DONE]"
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return openAIUsage{}, false
	}
	data := bytes.TrimPrefix(line, []byte("data: "))
	if bytes.Equal(data, []byte("[DONE]")) {
		return openAIUsage{}, false
	}

	var chunk openAIStreamChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return openAIUsage{}, false
	}
	if chunk.Usage == nil {
		return openAIUsage{}, false
	}
	usage := *chunk.Usage
	usage.Model = chunk.Model
	return usage, true
}

// isMeteredPath returns true for OpenAI-compatible API paths.
// Note: r.URL.Path never includes query strings, so exact match suffices.
func isMeteredPath(path string) bool {
	switch path {
	case "/v1/chat/completions", "/v1/completions", "/v1/embeddings":
		return true
	}
	return false
}

func detectStream(body []byte) bool {
	// Quick check for "stream":true in the request body
	var req struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	return req.Stream
}

func extractConsumerKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		key := strings.TrimPrefix(auth, "Bearer ")
		// Return prefix for privacy
		if len(key) > 8 {
			return key[:8] + "..."
		}
		return key
	}
	return "anonymous"
}

// responseRecorder captures the response body for non-streaming responses
// while still writing to the underlying ResponseWriter.
type responseRecorder struct {
	http.ResponseWriter
	body       *bytes.Buffer
	statusCode int
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// streamRecorder intercepts streaming responses to extract usage from SSE chunks.
type streamRecorder struct {
	http.ResponseWriter
	flusher http.Flusher
	usage   openAIUsage
}

func (s *streamRecorder) Write(b []byte) (int, error) {
	// Parse SSE lines from the chunk
	scanner := bufio.NewScanner(bytes.NewReader(b))
	for scanner.Scan() {
		if usage, ok := extractUsageFromSSELine(scanner.Bytes()); ok {
			s.usage = usage
		}
	}

	// Always write through to the client
	n, err := s.ResponseWriter.Write(b)
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return n, err
}

func (s *streamRecorder) Flush() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}
