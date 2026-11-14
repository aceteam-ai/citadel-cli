package redisapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWorkerControlWebSocketPublishesAuthenticatedExactJobIdentity(t *testing.T) {
	const jobID = "55d962d8-52a0-4910-b40b-534a14a6f417"
	upgrader := websocket.Upgrader{}
	got := make(chan WSMessage, 3)
	var handlers sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		if r.URL.Path != "/api/fabric/redis/ws" || r.Header.Get("Authorization") != "Bearer test-device-token" {
			t.Errorf("unauthenticated or unexpected transport: %s", r.URL.Path)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var msg WSMessage
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			got <- msg
		}
	}))
	defer func() { srv.Close(); handlers.Wait() }()
	client := NewClient(ClientConfig{BaseURL: srv.URL, Token: "test-device-token"})
	defer client.Close()
	if err := client.EnableWebSocket(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"claimed", "end", "error"} {
		data := map[string]any{"agent_version": "test"}
		if kind == "end" {
			data = map[string]any{"result": map[string]any{"accepted": false, "restarting": false, "code": "unmanaged_worker"}}
		}
		if kind == "error" {
			data = map[string]any{"error": "test error", "recoverable": false}
		}
		if err := client.PublishStreamEvent(context.Background(), jobID, "", kind, data); err != nil {
			t.Fatal(err)
		}
		select {
		case msg := <-got:
			if msg.Type != "publish" || msg.Channel != "stream:v1:"+jobID || msg.Message["jobId"] != jobID || msg.Message["type"] != kind {
				t.Fatalf("wire=%+v", msg)
			}
			encoded, _ := json.Marshal(msg.Message)
			var event StreamEvent
			if err := json.Unmarshal(encoded, &event); err != nil {
				t.Fatal(err)
			}
			if event.Version != "1.0" || event.Data == nil {
				t.Fatalf("event=%+v", event)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("missing websocket event")
		}
	}
}
