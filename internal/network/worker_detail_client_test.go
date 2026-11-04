package network

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSessionWorkerDetailClientKeepsSelectedReadRouteAndRefusesRedirects(t *testing.T) {
	for _, route := range []string{"/citadel/session/v1/status", "/citadel/session/v1/worker-detail"} {
		calls := 0
		transport := sessionStatusRoundTripper(func(request *http.Request) (*http.Response, error) {
			calls++
			if calls != 1 || request.Method != http.MethodGet || request.URL.Host != "citadel.local" || request.URL.Path != route {
				t.Fatal("read-only local request changed its selected route or followed a redirect")
			}
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"http://elsewhere.invalid/stop"}},
				Body: io.NopCloser(strings.NewReader("untrusted redirect")), Request: request}, nil
		})
		response, err := sessionControlRequestWithTransport(context.Background(), transport, http.MethodGet, route, true)
		if err != nil || response == nil || response.StatusCode != http.StatusFound || calls != 1 {
			t.Fatalf("selected read-only request: response=%v err=%v calls=%d", response, err, calls)
		}
		response.Body.Close()
	}
}
