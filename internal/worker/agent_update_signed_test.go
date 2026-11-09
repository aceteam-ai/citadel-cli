package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/update"
)

// TestAgentUpdateRefusesUnsignedRelease drives AGENT_UPDATE through the real
// update client against a fake release server publishing no signature.
func TestAgentUpdateRefusesUnsignedRelease(t *testing.T) {
	var archiveHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			_, _ = w.Write([]byte(strings.Repeat("a", 64) + "  citadel_v2.47.0_linux_amd64.tar.gz\n"))
		case strings.HasSuffix(r.URL.Path, ".tar.gz"), strings.HasSuffix(r.URL.Path, ".zip"):
			archiveHits.Add(1)
			_, _ = w.Write([]byte("archive"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var applied, restarted atomic.Bool
	var downloadErr error
	h := newTestHandler(t, func(c *AgentUpdateConfig) {
		c.NewAttempt = func() (*update.UpdateAttempt, error) {
			return &update.UpdateAttempt{Candidate: t.TempDir() + "/candidate"}, nil
		}
		c.Download = func(release *update.Release, destPath string) error {
			downloadErr = update.NewClient(c.Version).WithBaseURLs(srv.URL, srv.URL).DownloadAndVerify(release, destPath)
			return downloadErr
		}
		c.Apply = func(string) error { applied.Store(true); return nil }
		c.Restart = func() error { restarted.Store(true); return nil }
	})
	res, err := h.Execute(context.Background(), agentUpdateJob(perNodeQueue, nil), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusFailure {
		t.Fatalf("status = %v, want failure", res.Status)
	}
	if !errors.Is(downloadErr, update.ErrUnsignedRelease) {
		t.Fatalf("download err = %v, want ErrUnsignedRelease", downloadErr)
	}
	if applied.Load() || restarted.Load() {
		t.Fatal("an unsigned release reached apply or restart")
	}
	if n := archiveHits.Load(); n != 0 {
		t.Fatalf("archive fetched %d times for an unsigned release", n)
	}
}
