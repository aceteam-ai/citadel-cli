// Package localchat implements the client used by `citadel chat`
// (aceteam-ai/citadel-cli#575) to hold a streaming, multi-turn conversation with
// a model served locally on THIS node over its OpenAI-compatible
// /v1/chat/completions endpoint (vllm, llamacpp, bonsai, ollama).
//
// It is deliberately separate from internal/chat, which is node-to-node peer
// messaging over the Redis API proxy (a different feature). The build-request
// and parse-response logic lives here (not in cmd/) so it is unit-testable
// without a running engine; the interactive loop lives in cmd/chat.go.
package localchat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultMaxTokens is the completion budget used when the caller does not set
// one. Kept comfortably above the issue's ">= 512" floor so a thinking model
// (Bonsai) has room for reasoning_content plus an actual answer.
const DefaultMaxTokens = 1024

// Message is one turn in the conversation, in OpenAI chat format.
//
// IMPORTANT: for a thinking model the assistant's chain-of-thought
// (reasoning_content) is per-turn scratch and MUST NOT be stored here or resent
// on the next turn — only the final answer content belongs in the history.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the JSON body POSTed to /v1/chat/completions.
type chatRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	Stream    bool      `json:"stream"`
	MaxTokens int       `json:"max_tokens,omitempty"`
}

// BuildRequestBody builds the streaming /v1/chat/completions request body for a
// conversation. maxTokens <= 0 falls back to DefaultMaxTokens. The model id
// should be the id discovered from the engine's /v1/models (llama.cpp/bonsai
// ignore it; vLLM requires an exact match).
func BuildRequestBody(model string, messages []Message, maxTokens int) ([]byte, error) {
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	return json.Marshal(chatRequest{
		Model:     model,
		Messages:  messages,
		Stream:    true,
		MaxTokens: maxTokens,
	})
}

// StreamChunk is the decoded delta from one SSE event: the answer fragment
// (Content) and/or the chain-of-thought fragment (Reasoning). For a thinking
// model such as Bonsai the reasoning arrives first (with Content empty) and the
// answer follows in a separate field — never as inline <think> tags.
type StreamChunk struct {
	Content   string
	Reasoning string
}

// sseEnvelope is the subset of the OpenAI streaming chunk schema we consume.
// content/reasoning_content decode "null" JSON values to the empty string.
type sseEnvelope struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
	} `json:"choices"`
}

// ParseSSELine parses one raw line of an SSE stream. It returns:
//   - handled=false for lines that carry no data event (blank lines, ":"
//     comment/keep-alive lines, or any non-"data:" line) — the caller skips them.
//   - done=true when the terminal "data: [DONE]" sentinel is seen.
//   - a populated StreamChunk for a "data: {json}" payload.
//
// A JSON payload that parses but carries no content/reasoning yields a zero
// chunk with handled=true (e.g. the opening role-only delta), which the caller
// can safely ignore.
func ParseSSELine(line string) (chunk StreamChunk, handled bool, done bool, err error) {
	line = strings.TrimRight(line, "\r")
	if line == "" || strings.HasPrefix(line, ":") {
		return StreamChunk{}, false, false, nil
	}
	data, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return StreamChunk{}, false, false, nil
	}
	data = strings.TrimSpace(data)
	if data == "[DONE]" {
		return StreamChunk{}, false, true, nil
	}

	var env sseEnvelope
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		return StreamChunk{}, false, false, fmt.Errorf("parse stream chunk: %w", err)
	}
	if len(env.Choices) == 0 {
		return StreamChunk{}, true, false, nil
	}
	d := env.Choices[0].Delta
	return StreamChunk{Content: d.Content, Reasoning: d.ReasoningContent}, true, false, nil
}

// Client talks to one engine's OpenAI-compatible API at a base URL such as
// "http://localhost:8210".
type Client struct {
	BaseURL string
	Model   string
	HTTP    *http.Client
}

// NewClient builds a Client for the engine at the given localhost port. The HTTP
// client has no overall timeout because a streaming completion is open-ended;
// per-request cancellation is done via the context passed to Stream.
func NewClient(port int, model string) *Client {
	return &Client{
		BaseURL: fmt.Sprintf("http://localhost:%d", port),
		Model:   model,
		HTTP:    &http.Client{},
	}
}

// Stream sends the conversation and invokes onChunk for every streamed delta as
// it arrives. It returns when the stream completes ([DONE] or EOF), the context
// is cancelled, or an error occurs. A non-200 response body is read and surfaced
// as the error (a bad request comes back as plain JSON, not an SSE stream, so a
// naive line-loop would otherwise show nothing).
func (c *Client) Stream(ctx context.Context, messages []Message, maxTokens int, onChunk func(StreamChunk)) error {
	body, err := BuildRequestBody(c.Model, messages, maxTokens)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("engine returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	scanner := bufio.NewScanner(resp.Body)
	// Allow long SSE lines (a single delta is small, but be generous).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		chunk, handled, done, perr := ParseSSELine(scanner.Text())
		if perr != nil {
			return perr
		}
		if done {
			return nil
		}
		if handled && (chunk.Content != "" || chunk.Reasoning != "") {
			onChunk(chunk)
		}
	}
	if err := scanner.Err(); err != nil {
		// A cancelled context surfaces here as a read error; report the context
		// error so the caller can distinguish a user interrupt from a real fault.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

// HealthCheck reports whether the engine's API is reachable, with a short
// bounded probe. Used before entering the chat loop to fail fast with a clear
// message rather than on the first send.
func (c *Client) HealthCheck(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/models", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("engine at %s returned %s", c.BaseURL, resp.Status)
	}
	return nil
}
