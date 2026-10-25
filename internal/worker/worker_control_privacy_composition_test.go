package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/workercontrol"
)

func TestWorkerControlFailureCannotFallBackToGenericStatusAfterPrivacyComposition(t *testing.T) {
	factories := map[string]func(*redisapi.Client) JobSource{
		"api": func(client *redisapi.Client) JobSource {
			source := NewAPISource(APISourceConfig{QueueName: "jobs:v1:owned-composition"})
			source.client = client
			source.control = &workerControlBinding{client: client}
			return source
		},
		"ws": func(client *redisapi.Client) JobSource {
			source := NewWSSource(WSSourceConfig{Client: client, QueueName: "jobs:v1:owned-composition"})
			source.control = &workerControlBinding{client: client}
			return source
		},
	}
	for name, makeSource := range factories {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				_, _ = fmt.Fprint(w, `{"acknowledged":true}`)
			}))
			defer server.Close()
			client := redisapi.NewClient(redisapi.ClientConfig{BaseURL: server.URL, Token: "owned-synthetic-device"})
			source := makeSource(client)
			// A malformed/unmatched control delivery has no claim/permit. Its
			// Nack must keep it pending; Fail cannot use generic status or ACK.
			job := &Job{ID: "owned-control", Type: JobTypeWorkerControl, MessageID: "1-0"}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			failure := errors.New("owned private control failure")
			if err := source.Nack(ctx, job, failure); err != nil {
				t.Fatalf("control Nack: %v", err)
			}
			if err := source.Fail(ctx, job, failure, map[string]any{"error": failure.Error()}); !errors.Is(err, workercontrol.ErrDenied) {
				t.Fatalf("unmatched control Fail = %v, want denied", err)
			}
			if requests.Load() != 0 {
				t.Fatalf("control failure reached generic HTTP status/ACK: %d requests", requests.Load())
			}
		})
	}
}
