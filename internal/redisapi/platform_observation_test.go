package redisapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platformhttp"
	"github.com/gorilla/websocket"
)

func TestFallbackAndWebSocketWriteCannotFabricatePlatformHTTPRecovery(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("{\"error\":\"not_found\"}"))
	}))
	defer srv.Close()
	var health platformhttp.Health
	client := NewClient(ClientConfig{BaseURL: srv.URL, Token: "synthetic-device", PlatformObserver: health.Observe})
	defer client.Close()
	if cfg, err := client.FetchWorkerConfig(context.Background()); err != nil || cfg != nil {
		t.Fatal("existing 404 config fallback changed")
	}
	if snap := health.Snapshot(); snap.FailingSince == nil || snap.OKAt != nil || snap.LastStatus != 404 {
		t.Fatal("nil fallback result fabricated HTTP success")
	}
	if err := client.EnableWebSocket(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Publish(context.Background(), "synthetic-channel", map[string]any{"fixture": true}); err != nil {
		t.Fatal(err)
	}
	if snap := health.Snapshot(); snap.OKAt != nil || snap.LastStatus != 404 {
		t.Fatal("queued WS publish/handshake fabricated a successful HTTP round trip")
	}
}
