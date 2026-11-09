package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/update"
)

// unsignedReleaseServer publishes checksums.txt and an archive for every tag
// but no checksums.txt.sig. It counts archive requests.
func unsignedReleaseServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var archiveHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			_, _ = w.Write([]byte(strings.Repeat("a", 64) + "  citadel_v2.300.0_linux_amd64.tar.gz\n"))
		case strings.HasSuffix(r.URL.Path, ".tar.gz"), strings.HasSuffix(r.URL.Path, ".zip"):
			archiveHits.Add(1)
			_, _ = w.Write([]byte("archive"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &archiveHits
}

// TestManualUpdateRefusesUnsignedRelease runs `citadel update install`'s
// download-and-apply step with the production download wiring.
func TestManualUpdateRefusesUnsignedRelease(t *testing.T) {
	withAutoUpdateEnv(t, "v2.299.0")
	srv, archiveHits := unsignedReleaseServer(t)
	originalCapability, originalApply := manualUpdateCapabilityFn, applyReleaseFn
	t.Cleanup(func() { manualUpdateCapabilityFn, applyReleaseFn = originalCapability, originalApply })
	manualUpdateCapabilityFn = func() update.SelfUpdateCapability {
		return update.SelfUpdateCapability{State: update.SelfUpdateAvailable}
	}
	applied := false
	applyReleaseFn = func(context.Context, string, string, string, update.ApplyIntent) (update.ApplyResult, error) {
		applied = true
		return update.ApplyResult{}, nil
	}

	client := update.NewClient("v2.299.0").WithBaseURLs(srv.URL, srv.URL)
	_, err := downloadAndApplyManual(client, &update.Release{TagName: "v2.300.0"}, nil)
	if !errors.Is(err, update.ErrUnsignedRelease) {
		t.Fatalf("err = %v, want ErrUnsignedRelease", err)
	}
	if applied {
		t.Fatal("an unsigned release reached apply")
	}
	if n := archiveHits.Load(); n != 0 {
		t.Fatalf("archive fetched %d times for an unsigned release", n)
	}
}

// TestAutoUpdateHeartbeatMirrorsSignatureResult pins that a signature refusal
// recorded by the updater reaches the heartbeat auto_update block verbatim.
func TestAutoUpdateHeartbeatMirrorsSignatureResult(t *testing.T) {
	for _, result := range []string{update.ResultSignatureMissing, update.ResultSignatureInvalid} {
		withAutoUpdateEnv(t, "v2.299.0")
		if err := update.SaveState(&update.State{AutoUpdate: true, Channel: "stable"}); err != nil {
			t.Fatal(err)
		}
		if err := update.RecordCheckResult(time.Now(), result, true, "v2.300.0"); err != nil {
			t.Fatal(err)
		}
		snap := buildAutoUpdateSnapshot(autoUpdatePolicyInputs{}, "periodic", update.SelfUpdateCapability{})
		report := autoUpdateReportFrom(&snap)
		if snap.LastResult != result || report == nil || report.LastResult != result {
			t.Fatalf("last_result = %q / %+v, want %q", snap.LastResult, report, result)
		}
	}
}
