package terminal

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platformhttp"
)

func TestTokenRefreshContributesRealHTTPRecoveryEvidence(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		w.Write([]byte("{\"tokens\":[],\"error\":\"synthetic\"}"))
	}))
	defer srv.Close()
	var health platformhttp.Health
	v := NewCachingTokenValidator(srv.URL, "synthetic-org", "synthetic-device", time.Hour, health.Observe)
	defer v.httpClient.CloseIdleConnections()
	if err := v.fetchAndCacheTokens(); err != nil || health.Snapshot().OKAt == nil {
		t.Fatal("token refresh 2xx did not record actual success")
	}
	status.Store(http.StatusServiceUnavailable)
	if err := v.fetchAndCacheTokens(); err == nil || health.Snapshot().FailingSince == nil {
		t.Fatal("token refresh wrong answer lacked provenance")
	}
	status.Store(http.StatusUnauthorized)
	if err := v.fetchAndCacheTokens(); err == nil || health.Snapshot().FailingSince != nil {
		t.Fatal("authorization refusal armed process recovery")
	}
}
