package cobrowsestream

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// MaxRecordBytes is the largest recording the backend upload path accepts.
const MaxRecordBytes int64 = 24 * 1024 * 1024

// RecordWebM captures one bounded CDP screencast directly into a WebM file.
// Frames are coalesced and sampled at ten per second, so a busy page cannot
// grow an unbounded queue or make the encoder run faster than real time.
func RecordWebM(ctx context.Context, debugPort int, outputPath string, duration time.Duration) error {
	return recordWebMWithGrace(ctx, debugPort, outputPath, duration, 5*time.Second)
}

func recordWebMWithGrace(ctx context.Context, debugPort int, outputPath string, duration, grace time.Duration) error {
	if duration < time.Second || duration > 30*time.Second {
		return fmt.Errorf("record duration must be between 1 and 30 seconds")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// This envelope includes setup, frame writes, encoder finalization and cleanup.
	// A stopped encoder cannot hold the session's driver lock indefinitely.
	encoderCtx, cancel := context.WithTimeout(ctx, duration+grace)
	defer cancel()
	cmd := exec.CommandContext(encoderCtx, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "mjpeg", "-framerate", "10", "-i", "pipe:0",
		"-an", "-c:v", "libvpx-vp9", "-deadline", "realtime",
		"-cpu-used", "8", "-b:v", "1M",
		"-fs", strconv.FormatInt(MaxRecordBytes, 10), outputPath)
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
	cdp, err := dialCDPContext(encoderCtx, debugPort, func(frame screencastFrame) { holder.set(frame.jpeg) })
	if err != nil {
		return fmt.Errorf("attach browser screencast: %w", err)
	}
	defer func() {
		// Closing the socket interrupts a concurrent CDP write before Close
		// waits for the read loop. Detaching also stops the screencast.
		_ = cdp.conn.Close()
		cdp.Close()
	}()
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
	case <-encoderCtx.Done():
		return encoderCtx.Err()
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
					if encoderCtx.Err() != nil {
						return encoderCtx.Err()
					}
					return fmt.Errorf("write video frame: %w", err)
				}
				frames++
			}
			if info, err := os.Stat(outputPath); err == nil && info.Size() > MaxRecordBytes {
				return fmt.Errorf("recording exceeds the 24 MiB export limit")
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
				if encoderCtx.Err() != nil {
					return encoderCtx.Err()
				}
				return fmt.Errorf("encode WebM: %w", err)
			}
			info, err := os.Stat(outputPath)
			if err != nil || info.Size() == 0 {
				return fmt.Errorf("video encoder produced no WebM output")
			}
			if info.Size() > MaxRecordBytes {
				return fmt.Errorf("recording exceeds the 24 MiB export limit")
			}
			return nil
		case <-cdp.Done():
			return fmt.Errorf("browser closed during recording")
		case <-encoderCtx.Done():
			return encoderCtx.Err()
		}
	}
}
