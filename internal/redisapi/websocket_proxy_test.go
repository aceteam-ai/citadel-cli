package redisapi

import (
	"net/http"
	"testing"
)

// An ordinary node keeps a direct WebSocket dial; only a caller that opts in
// (the HPC profile) gets an environment proxy.
func TestWebSocketProxyIsOptIn(t *testing.T) {
	if c := NewClient(ClientConfig{BaseURL: "https://example.test", Token: "t"}); c.wsClient.proxy != nil {
		t.Fatal("default client must dial the WebSocket directly")
	}
	c := NewClient(ClientConfig{BaseURL: "https://example.test", Token: "t", WebSocketProxy: http.ProxyFromEnvironment})
	if c.wsClient.proxy == nil {
		t.Fatal("WebSocketProxy was not threaded to the WebSocket client")
	}
}
