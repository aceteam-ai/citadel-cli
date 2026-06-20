package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/terminal"
	"github.com/gorilla/websocket"
)

func TestStreamerAddRemoveClient(t *testing.T) {
	s := NewStreamer()

	if s.ClientCount() != 0 {
		t.Fatalf("expected 0 clients, got %d", s.ClientCount())
	}

	// Create a test WebSocket pair
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		s.AddClient(conn)
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	// Give the server handler time to execute
	time.Sleep(50 * time.Millisecond)

	if s.ClientCount() != 1 {
		t.Fatalf("expected 1 client after add, got %d", s.ClientCount())
	}

	// The server-side conn is what we added to the streamer.
	// Remove the client by closing all.
	s.CloseAll()

	if s.ClientCount() != 0 {
		t.Fatalf("expected 0 clients after CloseAll, got %d", s.ClientCount())
	}

	client.Close()
}

func TestStreamerBroadcast(t *testing.T) {
	s := NewStreamer()
	s.SetSilent(true)

	// Set up a test server where the server-side conn is added to the streamer
	var serverConn *websocket.Conn
	done := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConn = conn
		s.AddClient(conn)
		close(done)
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer client.Close()

	// Wait for server handler to register the client
	<-done

	// Broadcast data
	testData := []byte("hello from PTY")
	s.Broadcast(testData)

	// Read from the client side
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msgBytes, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("client read failed: %v", err)
	}

	// Parse the protocol message
	msg, err := terminal.UnmarshalMessage(msgBytes)
	if err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if msg.Type != terminal.MessageTypeOutput {
		t.Fatalf("expected output message, got %s", msg.Type)
	}

	if string(msg.Payload) != string(testData) {
		t.Fatalf("expected payload %q, got %q", testData, msg.Payload)
	}

	_ = serverConn
}

func TestStreamerBroadcastEmpty(t *testing.T) {
	s := NewStreamer()
	// Should not panic
	s.Broadcast(nil)
	s.Broadcast([]byte{})
}

func TestStreamerCloseAllIdempotent(t *testing.T) {
	s := NewStreamer()
	// Should not panic on empty
	s.CloseAll()
	s.CloseAll()
}
