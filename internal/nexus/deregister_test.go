package nexus

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeregisterRequiresExplicitServerConfirmation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		wantOK bool
	}{
		{name: "confirmed", status: http.StatusOK, body: `{"success":true}`, wantOK: true},
		{name: "already absent", status: http.StatusNotFound, body: `{}`, wantOK: true},
		{name: "explicit refusal", status: http.StatusOK, body: `{"success":false}`},
		{name: "malformed success", status: http.StatusOK, body: `not-json`},
		{name: "empty success", status: http.StatusOK},
		{name: "unexpected 2xx", status: http.StatusNoContent},
		{name: "oversized", status: http.StatusOK, body: `{"success":true,"message":"` + strings.Repeat("x", maxDeregisterResponseBytes) + `"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer test-api-key" {
					t.Errorf("authorization = %q", got)
				}
				w.WriteHeader(tt.status)
				_, _ = fmt.Fprint(w, tt.body)
			}))
			defer server.Close()

			err := NewDeregisterClient(server.URL, "test-api-key").Deregister(
				context.Background(), DeregisterRequest{NodeName: "disposable-node"},
			)
			if (err == nil) != tt.wantOK {
				t.Fatalf("error = %v, wantOK %v", err, tt.wantOK)
			}
			if err != nil && strings.Contains(err.Error(), tt.body) && tt.body != "" {
				t.Fatalf("error leaked response body: %v", err)
			}
		})
	}
}
