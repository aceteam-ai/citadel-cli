package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWithPulseSink(t *testing.T) {
	// Replaces an inherited PULSE_SINK and preserves other vars.
	env := []string{"HOME=/root", "PULSE_SINK=old", "DISPLAY=:99"}
	got := withPulseSink(env, "citadel_meeting_abc")
	var sinkCount int
	var sawHome, sawDisplay bool
	for _, kv := range got {
		if strings.HasPrefix(kv, EnvPulseSink+"=") {
			sinkCount++
			if kv != "PULSE_SINK=citadel_meeting_abc" {
				t.Errorf("PULSE_SINK not overridden: %q", kv)
			}
		}
		if kv == "HOME=/root" {
			sawHome = true
		}
		if kv == "DISPLAY=:99" {
			sawDisplay = true
		}
	}
	if sinkCount != 1 {
		t.Errorf("expected exactly one PULSE_SINK entry, got %d", sinkCount)
	}
	if !sawHome || !sawDisplay {
		t.Errorf("withPulseSink dropped unrelated env vars: home=%v display=%v", sawHome, sawDisplay)
	}
}

func TestBuildAudioFFmpegArgs(t *testing.T) {
	args := buildAudioFFmpegArgs("citadel_meeting_x.monitor", "/w/meetings/x.wav")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-f pulse", "-i citadel_meeting_x.monitor",
		"-ac 1", "-ar 16000", "-y /w/meetings/x.wav",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("ffmpeg args missing %q; got: %s", want, joined)
		}
	}
}

func TestSanitizeSinkSuffix(t *testing.T) {
	cases := map[string]string{
		"abc123":                  "abc123",
		"meeting-2026-07-07":      "meeting_2026_07_07",
		"a/b\\c":                  "a_b_c",
		"":                        "session",
		"550e8400-e29b-41d4-a716": "550e8400_e29b_41d4_a716",
	}
	for in, want := range cases {
		if got := sanitizeSinkSuffix(in); got != want {
			t.Errorf("sanitizeSinkSuffix(%q)=%q want %q", in, got, want)
		}
	}
}

// TestNullSinkRecorder_CapturesAudio is an integration test that proves the full
// capture path on a node with a real PulseAudio/pipewire-pulse server: load a
// null sink, record its monitor, play a tone into the sink, and assert the WAV is
// non-silent. It skips where the audio stack is absent (e.g. CI), matching this
// repo's convention that hardware-dependent paths are covered by integration
// tests that no-op without the stack.
func TestNullSinkRecorder_CapturesAudio(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping audio integration test in -short mode")
	}
	if !audioStackAvailable() {
		t.Skip("no PulseAudio/pipewire-pulse server + ffmpeg/pactl on this host; skipping capture test")
	}

	rec := NewNullSinkRecorder("gotest_" + strconv.Itoa(os.Getpid()))
	// Safety net: unload the sink even if the test fails before Stop.
	defer func() {
		if id := rec.moduleID; id != "" {
			_ = exec.Command("pactl", "unload-module", id).Run()
		}
	}()

	if err := rec.LoadSink(); err != nil {
		t.Fatalf("LoadSink: %v", err)
	}
	out := filepath.Join(t.TempDir(), "capture.wav")
	if err := rec.Start(out); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Play a 2s 440 Hz tone INTO our sink (PULSE_SINK routes this player's output
	// to the sink; its monitor then carries it to the recorder).
	play := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-f", "pulse", "citadel_meeting_gotest_player")
	play.Env = withPulseSink(os.Environ(), rec.SinkName())
	if err := play.Run(); err != nil {
		t.Fatalf("play tone into sink: %v", err)
	}
	// Small settle so the tail of the tone is flushed to the monitor before stop.
	time.Sleep(300 * time.Millisecond)

	path, err := rec.Stop()
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat recording: %v", err)
	}
	if fi.Size() < 1024 {
		t.Fatalf("recording suspiciously small: %d bytes", fi.Size())
	}

	mean := meanVolumeDB(t, path)
	// Silence reads ~ -91 dB; a captured tone is far louder. -80 is a generous
	// floor that still rejects a silent (failed-routing) capture.
	if mean <= -80.0 {
		t.Fatalf("recording appears silent (mean_volume %.1f dB) — PULSE_SINK routing or monitor capture failed", mean)
	}
	t.Logf("captured %d bytes, mean_volume %.1f dB", fi.Size(), mean)
}

var meanVolRe = regexp.MustCompile(`mean_volume:\s*(-?[0-9.]+) dB`)

// meanVolumeDB runs ffmpeg volumedetect on a file and returns the mean volume in
// dB. Fails the test if ffmpeg output cannot be parsed.
func meanVolumeDB(t *testing.T, path string) float64 {
	t.Helper()
	out, _ := exec.Command("ffmpeg", "-hide_banner", "-nostats",
		"-i", path, "-af", "volumedetect", "-f", "null", "/dev/null").CombinedOutput()
	m := meanVolRe.FindSubmatch(out)
	if m == nil {
		t.Fatalf("could not parse mean_volume from ffmpeg output:\n%s", out)
	}
	v, err := strconv.ParseFloat(string(m[1]), 64)
	if err != nil {
		t.Fatalf("parse mean_volume %q: %v", m[1], err)
	}
	return v
}
