package redisapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAuthorizationRefusalDebugLogsTokenFingerprintOnly(t *testing.T) {
	const token = "device-token-0123456789abcdefQRST"
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var debug []string
			client := NewClient(ClientConfig{BaseURL: "https://example.invalid", Token: token, DebugFunc: func(format string, args ...any) {
				debug = append(debug, fmt.Sprintf(format, args...))
			}})
			client.httpClient = &http.Client{Transport: authorizationRoundTrip(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("body-canary")), Request: request}, nil
			})}
			if err := client.doRequest(context.Background(), "GET", "/api/fabric/worker-config", nil, nil); err == nil {
				t.Fatal("expected authorization error")
			}
			all := strings.Join(debug, "\n")
			if strings.Contains(all, token) || strings.Contains(all, "body-canary") {
				t.Fatalf("debug leaked credential or body: %s", all)
			}
			if !strings.Contains(all, "token=…QRST") {
				t.Fatalf("debug missing token fingerprint: %s", all)
			}
		})
	}
}
