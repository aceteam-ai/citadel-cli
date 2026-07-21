package mesh

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestChatCompletionOverInjectedDialer proves ChatCompletion routes an OpenAI
// chat request to a remote engine over the (injected) dialer end-to-end: the
// dialer points at a local httptest server standing in for the remote engine.
func TestChatCompletionOverInjectedDialer(t *testing.T) {
	var gotPath, gotModel, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		var req ChatRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"cmpl-1","choices":[{"message":{"role":"assistant","content":"hello from engine"}}]}`))
	}))
	defer srv.Close()
	srvAddr := strings.TrimPrefix(srv.URL, "http://")

	dialer := func(ctx context.Context, netw, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", srvAddr)
	}

	client := NewClient(dialer)
	body, err := BuildChatRequest("Qwen/Qwen2.5-7B", "hi", false)
	if err != nil {
		t.Fatal(err)
	}

	target := ServedModel{Model: "Qwen/Qwen2.5-7B", IP: "100.64.0.2", Port: 8201, Engine: "vllm"}
	resp, err := client.ChatCompletionTo(context.Background(), target, body)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if gotPath != ChatEndpointPath {
		t.Errorf("path = %q, want %q", gotPath, ChatEndpointPath)
	}
	if gotModel != "Qwen/Qwen2.5-7B" {
		t.Errorf("model = %q", gotModel)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q", gotContentType)
	}

	respBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(respBody), "hello from engine") {
		t.Errorf("unexpected response body: %s", respBody)
	}
}

func TestChatCompletionValidatesTarget(t *testing.T) {
	client := NewClient(func(ctx context.Context, netw, addr string) (net.Conn, error) {
		return nil, nil
	})
	if _, err := client.ChatCompletion(context.Background(), "", 8201, nil); err == nil {
		t.Error("expected error for empty ip")
	}
	if _, err := client.ChatCompletion(context.Background(), "100.64.0.2", 0, nil); err == nil {
		t.Error("expected error for zero port")
	}
}

func TestBuildChatRequest(t *testing.T) {
	body, err := BuildChatRequest("m", "hello", true)
	if err != nil {
		t.Fatal(err)
	}
	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "m" || !req.Stream || len(req.Messages) != 1 || req.Messages[0].Content != "hello" || req.Messages[0].Role != "user" {
		t.Fatalf("unexpected request: %+v", req)
	}
}
