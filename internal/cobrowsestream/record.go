package cobrowsestream

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// RecordWebM captures one bounded CDP screencast directly into a WebM file.
// Frames are coalesced and sampled at ten per second, so a busy page cannot
// grow an unbounded queue or make the encoder run faster than real time.
func RecordWebM(ctx context.Context, debugPort int, outputPath string, duration time.Duration) error {
	if duration < time.Second || duration > 30*time.Second {
		return fmt.Errorf("record duration must be between 1 and 30 seconds")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	encoderCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(encoderCtx, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "mjpeg", "-framerate", "10", "-i", "pipe:0",
		"-an", "-c:v", "libvpx-vp9", "-deadline", "realtime",
		"-cpu-used", "8", "-b:v", "1M", outputPath)
	pipe, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open video encoder input: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start video encoder: %w", err)
	}
	waited := false
	defer func() {
		if !waited {
			cancel()
			_ = pipe.Close()
			_ = cmd.Wait()
		}
	}()

	holder := newFrameHolder()
	cdp, err := dialCDP(debugPort, func(frame screencastFrame) { holder.set(frame.jpeg) })
	if err != nil {
		return fmt.Errorf("attach browser screencast: %w", err)
	}
	defer cdp.Close()
	defer cdp.stopScreencast()
	if err := cdp.startScreencast(60, 1280, 720, 1); err != nil {
		return fmt.Errorf("start browser screencast: %w", err)
	}
	first := time.NewTimer(2 * time.Second)
	defer first.Stop()
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	select {
	case <-holder.signal:
	case <-first.C:
		return fmt.Errorf("browser screencast produced no frames")
	case <-cdp.Done():
		return fmt.Errorf("browser closed before recording started")
	case <-ctx.Done():
		return ctx.Err()
	}
	last := holder.take()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	frames := 0
	for {
		select {
		case <-holder.signal:
			if frame := holder.take(); len(frame) > 0 {
				last = frame
			}
		case <-ticker.C:
			if len(last) > 0 {
				if _, err := pipe.Write(last); err != nil {
					return fmt.Errorf("write video frame: %w", err)
				}
				frames++
			}
		case <-deadline.C:
			if frames == 0 {
				return fmt.Errorf("browser screencast produced no usable frames")
			}
			if err := pipe.Close(); err != nil {
				return fmt.Errorf("close video encoder input: %w", err)
			}
			err := cmd.Wait()
			waited = true
			if err != nil {
				return fmt.Errorf("encode WebM: %w", err)
			}
			info, err := os.Stat(outputPath)
			if err != nil || info.Size() == 0 {
				return fmt.Errorf("video encoder produced no WebM output")
			}
			return nil
		case <-cdp.Done():
			return fmt.Errorf("browser closed during recording")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
