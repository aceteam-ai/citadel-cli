package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

func TestAPIFineTuneControlScopedStatusProgressCancelAndAmbiguousCommit(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	state := map[string]any{"status": "queued", "progress_percent": 0, "current_epoch": 0, "current_loss": nil, "cancelled": false}
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/fabric/redis/finetune/"+id || r.Header.Get("Authorization") != "Bearer device-token" {
			http.Error(w, "wrong path or auth", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(state)
		case http.MethodPost:
			var body struct {
				Fields map[string]any `json:"fields"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode: %v", err)
			}
			posts++
			for key, value := range body.Fields {
				state[key] = value
			}
			if posts == 1 {
				// The backend committed the status before the HTTP response was lost.
				http.Error(w, "lost response", http.StatusGatewayTimeout)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"updated": true})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	source := NewAPISource(APISourceConfig{BaseURL: server.URL, Token: "device-token"})
	source.client = redisapi.NewClient(redisapi.ClientConfig{BaseURL: server.URL, Token: "device-token"})
	control := NewAPIFineTuneControl(source)
	if err := control.Update(context.Background(), id, map[string]any{"status": "running"}); err != nil {
		t.Fatalf("ambiguous committed update: %v", err)
	}
	if posts != 1 {
		t.Fatalf("ambiguous delivery retried mutation: %d posts", posts)
	}
	if err := control.Update(context.Background(), id, map[string]any{
		"progress_percent": 12.5, "current_epoch": 1, "current_loss": 0.42,
	}); err != nil {
		t.Fatal(err)
	}
	if posts != 2 {
		t.Fatalf("progress update: %d posts", posts)
	}
	if cancelled, err := control.Cancelled(context.Background(), id); err != nil || cancelled {
		t.Fatalf("cancelled=%v err=%v", cancelled, err)
	}
	state["cancelled"] = true
	if cancelled, err := control.Cancelled(context.Background(), id); err != nil || !cancelled {
		t.Fatalf("cancelled=%v err=%v", cancelled, err)
	}
}

func TestAPIFineTuneControlFailsClosedWhenScopedEndpointMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no scoped route", http.StatusNotFound)
	}))
	defer server.Close()
	source := NewAPISource(APISourceConfig{BaseURL: server.URL, Token: "device-token"})
	source.client = redisapi.NewClient(redisapi.ClientConfig{BaseURL: server.URL, Token: "device-token"})
	control := NewAPIFineTuneControl(source)
	_, err := control.Cancelled(context.Background(), "11111111-1111-4111-8111-111111111111")
	if err == nil || !strings.Contains(err.Error(), "status 404") {
		t.Fatalf("expected scoped 404, got %v", err)
	}
}
