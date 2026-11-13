package cobrowsestream

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newRecordOutput(t *testing.T) (*os.File, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capture.webm")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file, path
}

func TestRecordWebMConsumesBrowserFrames(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	withFakeCDP(t, f)

	// The fake encoder copies stdin to its output. A successful recording must
	// therefore contain browser frames, not merely an empty file made by ffmpeg.
	bin := t.TempDir()
	encoder := filepath.Join(bin, "ffmpeg")
	if err := os.WriteFile(encoder, []byte("#!/bin/sh\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	file, out := newRecordOutput(t)
	if err := RecordWebM(context.Background(), 9222, file, time.Second); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < len(f.jpeg) || !strings.HasPrefix(string(data), string(f.jpeg)) {
		t.Fatalf("recording lacks browser frames: %x", data)
	}
	f.mu.Lock()
	acks := f.acks
	f.mu.Unlock()
	if acks < 2 {
		t.Fatalf("browser frame acknowledgements = %d, want at least 2", acks)
	}
}

func TestRecordWebMProducesPlayableVideo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
	f := newFakeChrome()
	defer f.close()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var frame bytes.Buffer
	if err := jpeg.Encode(&frame, img, nil); err != nil {
		t.Fatal(err)
	}
	f.jpeg = frame.Bytes()
	withFakeCDP(t, f)
	file, out := newRecordOutput(t)
	if err := RecordWebM(context.Background(), 9222, file, time.Second); err != nil {
		t.Fatal(err)
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_name", "-of", "default=noprint_wrappers=1", out).Output()
	if err != nil || !strings.Contains(string(probe), "codec_name=vp9") {
		t.Fatalf("recording is not a VP9 WebM video: probe=%q, err=%v", probe, err)
	}
}

func TestRecordWebMRejectsUnboundedDuration(t *testing.T) {
	for _, duration := range []time.Duration{0, 31 * time.Second} {
		file, _ := newRecordOutput(t)
		if err := RecordWebM(context.Background(), 9222, file, duration); err == nil {
			t.Fatalf("duration %v accepted", duration)
		}
	}
}

func TestRecordWebMDiscoveryCancellation(t *testing.T) {
	// Stop or handoff must interrupt target discovery, before the WebSocket
	// dial. The resolver's HTTP request must use the recording context.
	originalTarget := cdpTargetURL
	originalClient := cdpHTTPClient
	entered := make(chan struct{})
	cdpTargetURL = defaultCDPTargetURL
	cdpHTTPClient = &http.Client{Timeout: 2 * time.Second, Transport: discoveryRoundTrip(func(request *http.Request) (*http.Response, error) {
		close(entered)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	t.Cleanup(func() {
		cdpTargetURL = originalTarget
		cdpHTTPClient = originalClient
	})
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte("#!/bin/sh\nexec cat >/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	file, _ := newRecordOutput(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- RecordWebM(ctx, 9222, file, time.Second) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("recording did not reach target discovery")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("recording returned %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled target discovery held the recording")
	}
}

func TestRecordWebMStalledEncoderReleasesAtDeadline(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	f.jpeg = bytes.Repeat([]byte{0xff}, 2<<20)
	f.totalFrames = 1
	withFakeCDP(t, f)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	start := time.Now()
	file, _ := newRecordOutput(t)
	err := recordWebMWithGrace(ctx, 9222, file, time.Second, 200*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled encoder returned %v, want deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Fatalf("stalled encoder held session custody for %v", elapsed)
	}
}
