package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/clockstate"
	redisclient "github.com/aceteam-ai/citadel-cli/internal/redis"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/websocket"
	goredis "github.com/redis/go-redis/v9"
)

func TestRedisStreamWriterImplementsStreamWriter(t *testing.T) {
	var _ StreamWriter = (*RedisStreamWriter)(nil)
}

func assertClockCarrier(t *testing.T, event map[string]any, eventType string) {
	t.Helper()
	if event["type"] != eventType {
		t.Fatalf("type = %v", event["type"])
	}
	data, ok := event["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %v", event["data"])
	}
	if data["t_claim_wall_ns"] != "9007199254740993" {
		t.Fatalf("claim not exact decimal: %v", data)
	}
	if eventType == "claimed" {
		if _, ok := data["t_done_wall_ns"]; ok {
			t.Fatal("premature done")
		}
		return
	}
	if data["t_done_wall_ns"] != "9007199254740999" || data["clock_state"] != "synced" {
		t.Fatalf("terminal not exact: %v", data)
	}
	if eventType == "end" && data["result"] != nil {
		t.Fatal("null result changed")
	}
	if eventType == "error" && (data["error"] != "synthetic" || data["recoverable"] != false) {
		t.Fatal("error shape changed")
	}
	if eventType == "cancelled" && data["reason"] != "synthetic cancel" {
		t.Fatal("cancel shape changed")
	}
}

func claimedClockFixture(t *testing.T, ctx context.Context, factory func(*Job) StreamWriter) StreamWriter {
	t.Helper()
	ns := int64(9007199254740993)
	r := NewRunner(NewMockJobSource("clock-fixture", nil), nil, RunnerConfig{ClockState: &fixtureClockState{clockstate.Synced}, ActivityFn: func(string, string) {}})
	r.wallNow = func() time.Time { return time.Unix(0, ns) }
	r.WithStreamWriterFactory(factory)
	proceed, writer, _ := r.claimJob(ctx, &Job{ID: "clock-fixture", RayID: "ray", Payload: map[string]any{}})
	if !proceed {
		t.Fatal("real runner claim refused")
	}
	ns = 9007199254740999
	return writer
}

func writeClockCarriers(t *testing.T, w StreamWriter) {
	t.Helper()
	for _, write := range []func() error{
		func() error { return w.WriteEnd(nil) },
		func() error { return w.WriteError(errors.New("synthetic"), false) },
		func() error { return w.WriteCancelled("synthetic cancel") },
	} {
		if err := write(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClockCarrierDirectRedisActualJSON(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client := redisclient.NewClient(redisclient.ClientConfig{})
	if err := client.Connect(ctx, "redis://"+mr.Addr(), ""); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	reader := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer reader.Close()
	sub := reader.Subscribe(ctx, "stream:v1:clock-fixture")
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	w := claimedClockFixture(t, ctx, func(job *Job) StreamWriter { return NewRedisStreamWriter(ctx, client, job.ID, job.RayID) })
	writeClockCarriers(t, w)
	for _, kind := range []string{"claimed", "end", "error", "cancelled"} {
		msg, err := sub.ReceiveMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var event map[string]any
		if err = json.Unmarshal([]byte(msg.Payload), &event); err != nil {
			t.Fatal(err)
		}
		assertClockCarrier(t, event, kind)
	}
}

func TestClockCarrierAPIAndWebSocketActualJSON(t *testing.T) {
	for _, transport := range []string{"http", "websocket", "rejected-websocket-http-fallback"} {
		t.Run(transport, func(t *testing.T) {
			events := make(chan map[string]any, 4)
			upgrader := websocket.Upgrader{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
					if transport != "websocket" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					for {
						var msg redisapi.WSMessage
						if err := conn.ReadJSON(&msg); err != nil {
							return
						}
						if msg.Type == "publish" {
							events <- msg.Message
						}
					}
				}
				if strings.HasSuffix(r.URL.Path, "/pubsub/publish") {
					var body struct {
						Message string `json:"message"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					var event map[string]any
					if err := json.Unmarshal([]byte(body.Message), &event); err != nil {
						t.Error(err)
						return
					}
					events <- event
				}
				_, _ = w.Write([]byte(`{"success":true,"published":true}`))
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			client := redisapi.NewClient(redisapi.ClientConfig{BaseURL: srv.URL, Token: "synthetic-clock-token"})
			defer client.Close()
			if transport != "http" {
				err := client.EnableWebSocket(ctx)
				if (transport == "websocket") != (err == nil) {
					t.Fatalf("WS selection: %v", err)
				}
			}
			if transport == "websocket" && !client.IsWebSocketConnected() {
				t.Fatal("not actual WS")
			}
			w := claimedClockFixture(t, ctx, func(job *Job) StreamWriter { return NewAPIStreamWriter(ctx, client, job.ID, job.RayID) })
			writeClockCarriers(t, w)
			for _, kind := range []string{"claimed", "end", "error", "cancelled"} {
				select {
				case event := <-events:
					assertClockCarrier(t, event, kind)
				case <-ctx.Done():
					t.Fatal("fixture missing carrier")
				}
			}
		})
	}
}

func TestCreateRedisStreamWriterFactory(t *testing.T) {
	ctx := context.Background()
	source := NewRedisSource(RedisSourceConfig{
		URL: "redis://localhost:6379",
	})

	factory := CreateRedisStreamWriterFactory(ctx, source)

	if factory == nil {
		t.Fatal("CreateRedisStreamWriterFactory returned nil")
	}

	// Factory should return a StreamWriter
	job := &Job{ID: "test-job-id", RayID: "ray-test-123"}
	writer := factory(job)
	if writer == nil {
		t.Error("Factory returned nil StreamWriter")
	}

	// Verify it's the right type
	rsw, ok := writer.(*RedisStreamWriter)
	if !ok {
		t.Error("Factory should return *RedisStreamWriter")
	}

	if rsw.jobID != "test-job-id" {
		t.Errorf("jobID = %v, want test-job-id", rsw.jobID)
	}
	if rsw.rayID != "ray-test-123" {
		t.Errorf("rayID = %v, want ray-test-123", rsw.rayID)
	}
}

func TestNewRedisStreamWriter(t *testing.T) {
	ctx := context.Background()
	source := NewRedisSource(RedisSourceConfig{
		URL: "redis://localhost:6379",
	})

	writer := NewRedisStreamWriter(ctx, source.Client(), "job-123", "ray-456")

	if writer == nil {
		t.Fatal("NewRedisStreamWriter returned nil")
	}
	if writer.jobID != "job-123" {
		t.Errorf("jobID = %v, want job-123", writer.jobID)
	}
	if writer.rayID != "ray-456" {
		t.Errorf("rayID = %v, want ray-456", writer.rayID)
	}
	if writer.ctx != ctx {
		t.Error("ctx not set correctly")
	}
}
