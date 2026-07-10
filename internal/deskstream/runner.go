package deskstream

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// encoderRunner owns a single ffmpeg subprocess that captures the X display and
// emits Annex-B H.264 on stdout. It parses the stream into wire-ready BINARY
// payloads (SPS+PPS prepended on keyframes) and pushes them to a callback.
//
// Because an ffmpeg subprocess has no stdin signal to force a keyframe, an
// on-demand keyframe (client requestKeyframe, or a brand-new viewer joining) is
// served by restarting ffmpeg: a fresh process always starts with SPS+PPS+IDR.
// This is acceptable for the relay broadcast model (a short, ~one-frame hiccup)
// and keeps the encoder selection logic encoder-independent.
type encoderRunner struct {
	cfg     EncodeConfig
	encoder EncoderKind

	onPayload func([]byte) // called with each wire-ready BINARY payload

	mu      sync.Mutex
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	running bool
}

// newEncoderRunner creates a runner. onPayload is invoked from the runner's
// read goroutine for every BINARY payload; it must be safe to call from that
// goroutine and should not block for long.
func newEncoderRunner(cfg EncodeConfig, enc EncoderKind, onPayload func([]byte)) *encoderRunner {
	return &encoderRunner{cfg: cfg, encoder: enc, onPayload: onPayload}
}

// Start launches the ffmpeg subprocess and begins streaming payloads. It
// returns an error if ffmpeg cannot be started (e.g. binary missing). Capture
// errors (e.g. X display unreachable) surface asynchronously when ffmpeg exits;
// the read goroutine simply ends.
func (r *encoderRunner) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return fmt.Errorf("encoder already running")
	}

	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("ffmpeg not found: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	args := buildFFmpegArgs(r.cfg, r.encoder)
	cmd := exec.CommandContext(runCtx, path, args...)
	cmd.Env = append(os.Environ(), "DISPLAY="+r.cfg.Display)
	if r.cfg.XAuthority != "" {
		cmd.Env = append(cmd.Env, "XAUTHORITY="+r.cfg.XAuthority)
	}
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	r.cmd = cmd
	r.cancel = cancel
	r.running = true

	go r.readLoop(stdout)
	return nil
}

// readLoop reads Annex-B bytes from ffmpeg stdout, frames them, and forwards
// each wire-ready payload via onPayload.
func (r *encoderRunner) readLoop(stdout io.ReadCloser) {
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	framer := newNALFramer()
	reader := bufio.NewReaderSize(stdout, 64*1024)
	chunk := make([]byte, 32*1024)
	for {
		n, err := reader.Read(chunk)
		if n > 0 {
			for _, payload := range framer.Push(chunk[:n]) {
				if r.onPayload != nil {
					r.onPayload(payload)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// Stop terminates the ffmpeg subprocess and waits for the read goroutine to
// drain. It is safe to call multiple times.
func (r *encoderRunner) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	cmd := r.cmd
	r.cancel = nil
	r.cmd = nil
	r.running = false
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if cmd != nil {
		_ = cmd.Wait()
	}
}

// IsRunning reports whether the ffmpeg subprocess is currently active.
func (r *encoderRunner) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}
