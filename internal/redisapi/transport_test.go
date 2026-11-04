package redisapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platformhttp"
)

func TestLongPollKeepsBoundedTransportAndDoesNotSwapSharedClient(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	releasePoll := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Error("timeout copy changed authorization")
		}
		if r.URL.Path == "/api/fabric/redis/jobs/consume" {
			close(entered)
			<-release
			_, _ = w.Write([]byte("{\"messages\":[]}"))
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(server.Close)
	client := NewClient(ClientConfig{BaseURL: server.URL, Token: "synthetic-token"})
	t.Cleanup(func() { client.Close() })
	original := client.httpClient
	if _, ok := original.Transport.(*platformhttp.Transport); !ok {
		t.Fatal("platform client bypassed shared bounded transport")
	}
	result := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, err := client.ConsumeJob(context.Background(), ConsumeRequest{BlockMs: 10000})
		result <- err
	}()
	t.Cleanup(func() {
		releasePoll()
		select {
		case <-finished:
		case <-time.After(20 * time.Second):
			t.Error("owned long-poll goroutine failed to exit")
		}
	})
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("owned consume fixture was not reached")
	}
	if client.httpClient != original || client.httpClient.Timeout != 30*time.Second {
		t.Fatal("long poll changed the client's shared timeout/pool")
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	releasePoll()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if client.httpClient != original || client.LastConsumeStatus() != http.StatusOK {
		t.Fatal("timeout copy changed client identity or consume status")
	}
}
