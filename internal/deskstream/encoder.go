package deskstream

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// EncoderKind identifies which H.264 encoder ffmpeg will use.
type EncoderKind string

const (
	// EncoderVAAPI is hardware H.264 via VA-API (Intel/AMD).
	EncoderVAAPI EncoderKind = "h264_vaapi"
	// EncoderNVENC is hardware H.264 via NVIDIA NVENC.
	EncoderNVENC EncoderKind = "h264_nvenc"
	// EncoderSoftware is software H.264 via libx264.
	EncoderSoftware EncoderKind = "libx264"
)

// EncoderProbe is a function that reports whether ffmpeg lists the named
// encoder. It is a variable so tests can substitute a fake without running
// ffmpeg.
type EncoderProbe func(name string) bool

// EncodeConfig describes a capture+encode session.
type EncodeConfig struct {
	Display          string // X display, e.g. ":1"
	XAuthority       string // X cookie file; empty means none needed
	Width            int    // capture width in pixels
	Height           int    // capture height in pixels
	FPS              int    // target frame rate
	KeyframeInterval int    // forced IDR interval in frames (g)
}

// selectEncoder picks the best available H.264 encoder, preferring hardware
// (VA-API, then NVENC) and falling back to software libx264. It returns an
// error if NONE is available, so callers can advertise h264=false and degrade
// to noVNC.
//
// The preference order is fixed: VA-API first because it is the most common
// zero-config hardware path on Linux desktop nodes, then NVENC for NVIDIA hosts,
// then software.
func selectEncoder(probe EncoderProbe) (EncoderKind, error) {
	if probe == nil {
		probe = ffmpegHasEncoder
	}
	for _, enc := range []EncoderKind{EncoderVAAPI, EncoderNVENC, EncoderSoftware} {
		if probe(string(enc)) {
			return enc, nil
		}
	}
	return "", fmt.Errorf("no H.264 encoder available in ffmpeg (need h264_vaapi, h264_nvenc, or libx264)")
}

// ffmpegHasEncoder reports whether `ffmpeg -hide_banner -encoders` lists name.
// Returns false if ffmpeg is absent or the probe fails.
func ffmpegHasEncoder(name string) bool {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return false
	}
	out, err := exec.Command(path, "-hide_banner", "-encoders").CombinedOutput()
	if err != nil {
		return false
	}
	// Encoder lines look like " V....D h264_nvenc   NVIDIA NVENC H.264 encoder".
	// Match the token surrounded by whitespace to avoid substring false hits.
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		for _, fld := range fields {
			if fld == name {
				return true
			}
		}
	}
	return false
}

// buildFFmpegArgs builds the ffmpeg argument vector that captures the X display
// via x11grab and emits raw H.264 Annex-B NAL units on stdout.
//
// Common invariants for all encoders:
//   - x11grab input at the configured fps and display.
//   - Output muxed as raw h264 (Annex-B) to stdout ("-f h264 -" / "pipe:1").
//   - A forced IDR at least every 2 seconds AND at the keyframe interval, via
//     -g and -force_key_frames, so a fresh broadcast viewer never waits more
//     than ~2s for a decodable frame even without an explicit request.
//   - zerolatency-friendly settings (no B-frames, low GOP) to minimize delay.
//
// The actual on-demand keyframe (client requestKeyframe) is handled OUTSIDE
// ffmpeg by respawning the encoder, since an ffmpeg subprocess has no stdin
// signal to force a keyframe mid-stream.
func buildFFmpegArgs(cfg EncodeConfig, enc EncoderKind) []string {
	fps := cfg.FPS
	if fps <= 0 {
		fps = 15
	}
	gop := cfg.KeyframeInterval
	if gop <= 0 {
		gop = fps * 2 // ~2s keyframe interval
	}
	size := fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)

	// Common input: grab the X display.
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-f", "x11grab",
		"-framerate", strconv.Itoa(fps),
	}
	if cfg.Width > 0 && cfg.Height > 0 {
		args = append(args, "-video_size", size)
	}
	args = append(args, "-i", cfg.Display)

	// Force a keyframe at least every 2 seconds regardless of scene content.
	forceKey := "expr:gte(t,n_forced*2)"

	switch enc {
	case EncoderVAAPI:
		// VA-API needs the frames uploaded to the GPU; nv12 is the standard
		// surface format. The device defaults to /dev/dri/renderD128.
		args = append(args,
			"-vaapi_device", "/dev/dri/renderD128",
			"-vf", "format=nv12,hwupload",
			"-c:v", "h264_vaapi",
			"-g", strconv.Itoa(gop),
			"-force_key_frames", forceKey,
			"-bf", "0",
		)
	case EncoderNVENC:
		args = append(args,
			"-c:v", "h264_nvenc",
			"-preset", "p1", // fastest / low-latency
			"-tune", "ll", // low latency
			"-g", strconv.Itoa(gop),
			"-force_key_frames", forceKey,
			"-bf", "0",
		)
	default: // software
		args = append(args,
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-tune", "zerolatency",
			"-profile:v", "baseline",
			"-pix_fmt", "yuv420p",
			"-g", strconv.Itoa(gop),
			"-force_key_frames", forceKey,
			"-bf", "0",
		)
	}

	// Raw Annex-B H.264 on stdout.
	args = append(args, "-f", "h264", "pipe:1")
	return args
}

// resolveX11 returns the X display and Xauthority to capture. It shares the
// resolver used by the screenshot path so a systemd --user service (no DISPLAY
// in its env) still finds the active session's display + cookie instead of the
// old hardcoded ":0" with no auth (aceteam-ai/citadel-cli#287 family).
func resolveX11() (display, xauthority string) {
	return platform.ResolveX11Env()
}

// resolveDisplay returns just the X display (for capability checks).
func resolveDisplay() string {
	d, _ := resolveX11()
	return d
}

// H264Available reports whether this node can serve an H.264 desktop stream:
// Linux, ffmpeg present with a usable H.264 encoder, and an X display set.
// Clients use this (advertised as the "h264" capability) to decide whether to
// stream H.264 or fall back to noVNC.
func H264Available() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return false
	}
	if _, err := selectEncoder(ffmpegHasEncoder); err != nil {
		return false
	}
	// An X display must be reachable; we only check the variable here (a cheap,
	// allocation-free signal). The full reachability check happens when the
	// encoder starts and is reported via the stream's error path.
	return resolveDisplay() != ""
}
