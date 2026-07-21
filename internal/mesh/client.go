package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
)

// ChatEndpointPath is the OpenAI-compatible chat-completions path served by
// every engine citadel routes to: vLLM, llama.cpp, the bonsai llama.cpp fork,
// and Ollama's OpenAI-compatibility shim all expose it. That uniformity is why
// routing needs only (ip, port) and not the engine type.
const ChatEndpointPath = "/v1/chat/completions"

// Client sends OpenAI chat-completion requests to a remote node's engine over
// the mesh, dialing via the injected Dialer. It is standalone (no
// internal/network import): cmd constructs it with network.Dial; tests construct
// it with a dialer targeting a local httptest server.
type Client struct {
	http *http.Client
}

// NewClient builds a mesh chat client. No overall client timeout is set so
// streaming responses are not cut off mid-stream; callers bound a request with
// the context they pass to ChatCompletion.
func NewClient(dialer Dialer) *Client {
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:       func(ctx context.Context, netw, addr string) (net.Conn, error) { return dialer(ctx, netw, addr) },
				DisableKeepAlives: true,
			},
		},
	}
}

// ChatMessage is a single OpenAI chat message.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the minimal OpenAI chat-completions request this package
// builds. Callers that need more control can marshal their own body and call
// ChatCompletion directly.
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream,omitempty"`
}

// BuildChatRequest marshals a single-user-message chat request for the given
// model — the common one-shot `citadel mesh chat` case.
func BuildChatRequest(model, prompt string, stream bool) ([]byte, error) {
	return json.Marshal(ChatRequest{
		Model:    model,
		Messages: []ChatMessage{{Role: "user", Content: prompt}},
		Stream:   stream,
	})
}

// ChatCompletion POSTs a raw OpenAI chat-completions request body to the engine
// at ip:port over the mesh and returns the HTTP response. The caller owns the
// response body: read/stream it and Close it. The body is passed through
// verbatim so the caller controls model, messages, and streaming.
func (c *Client) ChatCompletion(ctx context.Context, ip string, port int, body []byte) (*http.Response, error) {
	if ip == "" {
		return nil, fmt.Errorf("empty target ip")
	}
	if port <= 0 {
		return nil, fmt.Errorf("invalid target port %d", port)
	}
	url := fmt.Sprintf("http://%s%s", net.JoinHostPort(ip, strconv.Itoa(port)), ChatEndpointPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return c.http.Do(req)
}

// ChatCompletionTo is a convenience wrapper that routes to a discovered
// ServedModel's node/port.
func (c *Client) ChatCompletionTo(ctx context.Context, target ServedModel, body []byte) (*http.Response, error) {
	return c.ChatCompletion(ctx, target.IP, target.Port, body)
}
