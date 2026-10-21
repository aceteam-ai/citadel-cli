package workflow

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func llmTestNode(endpoint string) *Node {
	return &Node{ID: "llm", Type: NodeTypeLLM, Params: map[string]any{"endpoint": endpoint, "model": "synthetic-model"}}
}

func TestLLMActualConstructorPinsDNSAndPreservesRequest(t *testing.T) {
	var requests, resolutions, dials atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat" || r.URL.IsAbs() ||
			r.Host != "model.example:11434" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("request did not retain the original endpoint/method/content type")
		}
		var body struct {
			Model    string              `json:"model"`
			Messages []map[string]string `json:"messages"`
			Stream   bool                `json:"stream"`
			Options  map[string]float64  `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "synthetic-model" || body.Stream ||
			len(body.Messages) != 2 || body.Messages[0]["content"] != "system" || body.Messages[1]["content"] != "prompt" ||
			body.Options["temperature"] != 0.2 || body.Options["num_predict"] != 32 {
			t.Error("guard changed the Ollama request contract")
		}
		_, _ = io.WriteString(w, `{"message":{"content":"synthetic-answer"},"done":true}`)
	}))
	defer server.Close()
	var proxies atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxies.Add(1) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	p := LLMPolicy{
		resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host != "model.example" {
				t.Error("resolver received an unexpected hostname")
			}
			if resolutions.Add(1) > 1 {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dials.Add(1)
			if address != "8.8.8.8:11434" {
				t.Error("transport did not pin the validated numeric destination")
			}
			// Test-only routing to our owned fixture; no production localhost bypass.
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	}
	executor := NewExecutor(ExecutorConfig{LLM: p})
	node := llmTestNode("http://model.example:11434/api/chat")
	node.Params["system_prompt"], node.Params["temperature"], node.Params["max_tokens"] = "system", 0.2, 32
	output, err := executor.nodeRegistry[NodeTypeLLM].Execute(context.Background(), node, map[string]any{"prompt": "prompt"})
	if err != nil || output["response"] != "synthetic-answer" || output["model"] != "synthetic-model" || output["raw"] == nil {
		t.Fatalf("guarded constructor request failed: output=%v err=%v", output, err)
	}
	if _, err := executor.nodeRegistry[NodeTypeLLM].Execute(context.Background(), node, map[string]any{"prompt": "prompt"}); err == nil {
		t.Fatal("later rebound DNS was not revalidated")
	}
	if requests.Load() != 1 || resolutions.Load() != 2 || dials.Load() != 1 || proxies.Load() != 0 {
		t.Fatalf("unexpected routing: requests=%d DNS=%d dials=%d proxy=%d", requests.Load(), resolutions.Load(), dials.Load(), proxies.Load())
	}
}

func TestLLMZeroPolicyAndPayloadGrantsCannotReachLocalService(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	node := llmTestNode(server.URL + "/api/chat")
	node.Params["allow"] = []string{server.URL}
	node.Params["policy"] = map[string]any{"allow": []string{server.URL}}
	for _, executor := range []NodeExecutor{&LLMNodeExecutor{}, NewExecutor(ExecutorConfig{}).nodeRegistry[NodeTypeLLM]} {
		output, err := executor.Execute(context.Background(), node, map[string]any{"prompt": "private", "allow": []string{server.URL}})
		if err == nil || output != nil {
			t.Fatal("zero policy or payload authority bypassed local-service denial")
		}
	}
	// Omitting endpoint does not authorize the legacy local default either.
	if _, err := (&LLMNodeExecutor{}).Execute(context.Background(), &Node{ID: "llm"}, map[string]any{"prompt": "private"}); err == nil {
		t.Fatal("legacy localhost default was implicitly allowed")
	}
	if requests.Load() != 0 {
		t.Fatal("refused LLM node contacted the private service")
	}
	httpOnly := NewExecutor(ExecutorConfig{HTTP: HTTPPolicy{prefixes: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}})
	if _, err := httpOnly.nodeRegistry[NodeTypeLLM].Execute(context.Background(), llmTestNode("http://10.0.0.1:11434/api/chat"), map[string]any{"prompt": "private"}); err == nil {
		t.Fatal("general HTTP policy granted LLM authority")
	}
}

func TestLLMExactLocalGrantRefusesRedirectAndKeepsStatusPrivate(t *testing.T) {
	var targets atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targets.Add(1) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/private-marker", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	endpoint := source.URL + "/api/chat"
	output, err := (&LLMNodeExecutor{policy: mustLLMPolicy(t, endpoint)}).Execute(context.Background(), llmTestNode(endpoint), map[string]any{"prompt": "private-marker"})
	if err == nil || output != nil || !strings.Contains(err.Error(), "307") || strings.Contains(err.Error(), "private-marker") ||
		strings.Contains(err.Error(), target.URL) || targets.Load() != 0 {
		t.Fatalf("redirect or private server body escaped: output=%v err=%v requests=%d", output, err, targets.Load())
	}
}

func TestLLMTransportKeepsTLSVerification(t *testing.T) {
	p := LLMPolicy{}
	u, ip, err := p.resolveURL(context.Background(), "https://8.8.8.8/api/chat")
	if err != nil {
		t.Fatal(err)
	}
	client := p.clientFor(u, ip)
	defer client.CloseIdleConnections()
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.RootCAs != nil ||
		transport.TLSClientConfig.ServerName != "8.8.8.8" || transport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Fatal("LLM transport weakened pinned routing or normal TLS trust")
	}
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	endpoint := server.URL + "/api/chat"
	_, err = (&LLMNodeExecutor{policy: mustLLMPolicy(t, endpoint)}).Execute(context.Background(), llmTestNode(endpoint), map[string]any{"prompt": "private-marker"})
	if err == nil || requests.Load() != 0 || strings.Contains(err.Error(), "x509") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("untrusted TLS reached application or leaked detail: %v", err)
	}
}

func TestLLMResponseSizeBoundaryAndErrorPrivacy(t *testing.T) {
	const response = `{"message":{"content":"ok"}}`
	for _, size := range []int{maxLLMResponseBytes - 1, maxLLMResponseBytes, maxLLMResponseBytes + 1} {
		t.Run(fmt.Sprintf("bytes-%d", size), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, response+strings.Repeat(" ", size-len(response)))
			}))
			defer server.Close()
			endpoint := server.URL + "/api/chat"
			output, err := (&LLMNodeExecutor{policy: mustLLMPolicy(t, endpoint)}).Execute(context.Background(), llmTestNode(endpoint), map[string]any{"prompt": "private"})
			if size > maxLLMResponseBytes {
				if !errors.Is(err, ErrLLMResponseTooLarge) || output != nil {
					t.Fatal("oversized response accepted or returned partial output")
				}
			} else if err != nil || output["response"] != "ok" {
				t.Fatalf("bounded response rejected: %v", err)
			}
		})
	}
	for _, mode := range []string{"server-error", "invalid-json", "short-body"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if mode == "server-error" {
					w.WriteHeader(http.StatusInternalServerError)
				} else if mode == "short-body" {
					w.Header().Set("Content-Length", "1000")
				}
				_, _ = io.WriteString(w, "private-marker response detail")
			}))
			defer server.Close()
			endpoint := server.URL + "/api/chat"
			output, err := (&LLMNodeExecutor{policy: mustLLMPolicy(t, endpoint)}).Execute(context.Background(), llmTestNode(endpoint), map[string]any{"prompt": "private-marker"})
			if err == nil || output != nil || strings.Contains(err.Error(), "private-marker") || strings.Contains(err.Error(), server.URL) {
				t.Fatalf("response failure exposed data: %v", err)
			}
		})
	}
}

func TestLLMRequestBudgetIncludesDNSAndCallerDeadline(t *testing.T) {
	for _, earlier := range []bool{false, true} {
		ctx := context.Background()
		var cancel context.CancelFunc = func() {}
		var expected time.Time
		if earlier {
			ctx, cancel = context.WithTimeout(ctx, time.Minute)
			expected, _ = ctx.Deadline()
		}
		start := time.Now()
		p := LLMPolicy{resolve: func(ctx context.Context, _ string) ([]netip.Addr, error) {
			deadline, ok := ctx.Deadline()
			if !ok || (earlier && !deadline.Equal(expected)) || (!earlier && (deadline.Before(start.Add(llmRequestTimeout-time.Second)) || deadline.After(start.Add(llmRequestTimeout+time.Second)))) {
				t.Error("DNS is outside the request/caller deadline")
			}
			return nil, errors.New("private-marker resolver detail")
		}}
		_, err := (&LLMNodeExecutor{policy: p}).Execute(ctx, llmTestNode("http://model.example/api/chat"), map[string]any{"prompt": "private"})
		cancel()
		if err == nil || strings.Contains(err.Error(), "private-marker") {
			t.Fatal("DNS failure escaped static error boundary")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	p := LLMPolicy{resolve: func(ctx context.Context, _ string) ([]netip.Addr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	_, err := (&LLMNodeExecutor{policy: p}).Execute(ctx, llmTestNode("http://model.example/api/chat"), map[string]any{"prompt": "private"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DNS ignored or lost caller cancellation: %v", err)
	}
}

func TestLLMCallerCancellationDoesNotWaitForDetachedDial(t *testing.T) {
	started, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := LLMPolicy{
		resolve: func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil },
		dial: func(context.Context, string, string) (net.Conn, error) {
			close(started)
			<-release // model net/http's detached connection attempt, no socket
			close(exited)
			return nil, errors.New("private-marker dial failure")
		},
	}
	done := make(chan error, 1)
	go func() {
		_, err := (&LLMNodeExecutor{policy: p}).Execute(ctx, llmTestNode("http://model.example/api/chat"), map[string]any{"prompt": "private"})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("owned dial fixture did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "private-marker") {
			t.Fatalf("cancellation lost or private dial error exposed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client waited for a detached dial after caller cancellation")
	}
	once.Do(func() { close(release) })
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("test-owned dial goroutine did not exit")
	}
}

func TestLLMBodyReadCancellationAndNetworkErrorPrivacy(t *testing.T) {
	ready := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, "{")
		w.(http.Flusher).Flush()
		close(ready)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpoint := server.URL + "/api/chat"
	p := mustLLMPolicy(t, endpoint)
	done := make(chan error, 1)
	go func() {
		_, err := (&LLMNodeExecutor{policy: p}).Execute(ctx, llmTestNode(endpoint), map[string]any{"prompt": "private-marker"})
		done <- err
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("owned body fixture did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "private-marker") {
			t.Fatalf("body cancellation identity/privacy failure: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("body read did not obey caller cancellation")
	}
	p = LLMPolicy{
		dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("private-marker socket/path/credential detail")
		},
	}
	output, err := (&LLMNodeExecutor{policy: p}).Execute(context.Background(), llmTestNode("http://8.8.8.8:11434/private-marker"), map[string]any{"prompt": "private-marker"})
	if err == nil || output != nil || strings.Contains(err.Error(), "private-marker") || strings.Contains(err.Error(), "8.8.8.8") {
		t.Fatalf("dial failure leaked details: %v", err)
	}
}
