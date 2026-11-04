package cmd

import (
	"context"
	"errors"
	"time"
)

// New, proposed limits for the inactive video-compose path only. These are not
// inherited audio-mix policy or native compatibility acceptance (#1437).
const (
	videoComposeMaxStdout = 1 << 20
	videoComposeMaxStderr = 64 << 10
)

var errVideoComposeStdoutLimit = errors.New("video-compose command stdout limit exceeded")

type videoComposeCommandOutput struct {
	stdout []byte
	stderr []byte
}

// Each writer belongs to one exec stream. Inspect it only after Run has joined
// the copy goroutines. Full writes are consumed without unbounded allocation.
type videoComposeOutputCapture struct {
	data     []byte
	limit    int
	tail     bool
	overflow bool
	cancel   context.CancelFunc
}

func (w *videoComposeOutputCapture) Write(p []byte) (int, error) {
	n := len(p)
	if w.tail {
		if n >= w.limit {
			w.data = append(w.data[:0], p[n-w.limit:]...)
		} else {
			if excess := len(w.data) + n - w.limit; excess > 0 {
				copy(w.data, w.data[excess:])
				w.data = w.data[:len(w.data)-excess]
			}
			w.data = append(w.data, p...)
		}
		return n, nil
	}
	remaining := w.limit - len(w.data)
	if n > remaining {
		w.data = append(w.data, p[:remaining]...)
		if !w.overflow {
			w.overflow = true
			w.cancel()
		}
	} else {
		w.data = append(w.data, p...)
	}
	return n, nil
}

// Keep the existing render-command custody: minimal env, private process scope,
// bounded wait and authoritative scope cleanup, including failed/cancelled runs.
func runVideoComposeCommand(ctx context.Context, bin string, args []string) (videoComposeCommandOutput, error) {
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	limited, err := renderLimitedCommand(childCtx, minimalRenderChildEnv(""), bin, args...)
	if err != nil {
		return videoComposeCommandOutput{}, err
	}
	stdout := &videoComposeOutputCapture{limit: videoComposeMaxStdout, cancel: cancel}
	stderr := &videoComposeOutputCapture{limit: videoComposeMaxStderr, tail: true}
	cmd := limited.cmd
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	processTree := configurePapercraftProcessTree(limited)
	processTree.beginWait()
	runErr := cmd.Run()
	cleanupErr := processTree.cleanupAfterWait()
	return finishVideoComposeCommand(stdout, stderr, runErr, cleanupErr)
}

func finishVideoComposeCommand(stdout, stderr *videoComposeOutputCapture, runErr, cleanupErr error) (videoComposeCommandOutput, error) {
	var overflowErr error
	if stdout.overflow {
		overflowErr = errVideoComposeStdoutLimit
	}
	return videoComposeCommandOutput{stdout: stdout.data, stderr: stderr.data}, errors.Join(runErr, cleanupErr, overflowErr)
}
