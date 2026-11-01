//go:build linux

package containerjobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestContainerRealAdapterAcceptsInitialNullExitOnlyBeforeStart(t *testing.T) {
	id, container := strings.Repeat("e", 64), strings.Repeat("c", 64)
	var mu sync.Mutex
	started := false
	creates, starts := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/version":
			json.NewEncoder(w).Encode(map[string]any{"ApiVersion": "1.51"})
		case "/v1.51/containers/" + testContainer + "/exec":
			creates++
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"Id": id})
		case "/v1.51/exec/" + id + "/json":
			var code any
			if started {
				code = 7
			}
			json.NewEncoder(w).Encode(map[string]any{"ID": id, "ContainerID": container, "Running": false, "ExitCode": code})
		case "/v1.51/exec/" + id + "/start":
			starts++
			started = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Error("unexpected fake Docker path")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	n := NewNative(t.TempDir(), nil)
	n.command = func(context.Context, string, ...string) (commandResult, error) {
		return commandResult{bytes: nativeState(true)}, nil
	}
	n.execControl = &dockerExecClient{base: srv.URL, client: srv.Client()}
	result, err := n.Exec(context.Background(), testContainer, []string{"fixture"})
	mu.Lock()
	defer mu.Unlock()
	if err != nil || result.ExitCode != 7 || creates != 1 || starts != 1 {
		t.Fatal("created exec was refused, restarted or assigned a fabricated exit", err, result, creates, starts)
	}
}
