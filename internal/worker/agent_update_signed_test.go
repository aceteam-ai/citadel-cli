package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	if res.Status != JobStatusTerminalFailure {
		t.Fatalf("status = %v, want terminal failure", res.Status)
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

// TestAgentUpdateSignatureRefusalIsTerminal pins the classification: every
// release-signature refusal is terminal (no retry), while an ordinary
// download failure stays retryable.
func TestAgentUpdateSignatureRefusalIsTerminal(t *testing.T) {
	cases := map[string]struct {
		err  error
		want JobStatus
	}{
		"missing":   {fmt.Errorf("v2.47.0: %w", update.ErrUnsignedRelease), JobStatusTerminalFailure},
		"invalid":   {fmt.Errorf("%w: v2.47.0: bad", update.ErrReleaseSignature), JobStatusTerminalFailure},
		"platform":  {fmt.Errorf("%w: v2.47.0", update.ErrPlatformNotPublished), JobStatusTerminalFailure},
		"oversize":  {fmt.Errorf("%w (limit 1)", update.ErrArchiveTooLarge), JobStatusFailure},
		"transient": {errors.New("connection reset"), JobStatusFailure},
	}
	for name, tc := range cases {
		h := newTestHandler(t, func(c *AgentUpdateConfig) {
			c.NewAttempt = func() (*update.UpdateAttempt, error) {
				return &update.UpdateAttempt{Candidate: t.TempDir() + "/candidate"}, nil
			}
			c.Download = func(*update.Release, string) error { return tc.err }
		})
		res, err := h.Execute(context.Background(), agentUpdateJob(perNodeQueue, nil), &NoOpStreamWriter{})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if res.Status != tc.want {
			t.Errorf("%s: status = %v, want %v", name, res.Status, tc.want)
		}
	}
}

// TestRunnerAgentUpdateSignatureRefusalFailsWithoutRetry drives the refusal
// through the runner: one terminal error event and source.Fail on the first
// delivery, never Nack.
func TestRunnerAgentUpdateSignatureRefusalFailsWithoutRetry(t *testing.T) {
	h := newTestHandler(t, func(c *AgentUpdateConfig) {
		c.NewAttempt = func() (*update.UpdateAttempt, error) {
			return &update.UpdateAttempt{Candidate: t.TempDir() + "/candidate"}, nil
		}
		c.Download = func(*update.Release, string) error {
			return fmt.Errorf("v2.47.0: %w", update.ErrUnsignedRelease)
		}
	})
	source := NewMockJobSource("test", nil)
	runner := NewRunner(source, []JobHandler{h}, RunnerConfig{WorkerID: "test-worker"})
	stream := &MockStreamWriter{}
	job := agentUpdateJob(perNodeQueue, nil)
	job.Metadata = JobMetadata{Attempts: 1, MaxAttempts: 3}

	if runner.executeJob(context.Background(), job, stream, time.Now(), false, 0) {
		t.Fatal("a signature refusal must not report success")
	}
	if stream.errorCount != 1 || stream.endCount != 0 || stream.erroredRecover {
		t.Fatalf("want one non-recoverable error: errors=%d ends=%d recoverable=%v", stream.errorCount, stream.endCount, stream.erroredRecover)
	}
	if len(source.FailedJobs()) != 1 || len(source.NackedJobs()) != 0 || len(source.AckedJobs()) != 0 {
		t.Fatalf("want source.Fail only: failed=%d nacked=%d acked=%d", len(source.FailedJobs()), len(source.NackedJobs()), len(source.AckedJobs()))
	}
}
