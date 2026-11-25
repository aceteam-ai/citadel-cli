package laneop

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// Exec runs one tmux command with fixed argv. stdin, when non-nil, is the
// command's standard input; it is how message bytes reach load-buffer, so a
// message never appears on a command line (/proc/*/cmdline is world-readable).
// Implementations prepend the global flags; callers pass the command only.
type Exec func(ctx context.Context, stdin []byte, args ...string) ([]byte, error)

// ErrTmuxUnavailable is any tmux failure. tmux's own stderr is never returned.
var ErrTmuxUnavailable = errors.New("tmux is unavailable")

const (
	maxTmuxOutput = 512 << 10
	tmuxTimeout   = 3 * time.Second
)

// globalFlags: -N never starts a server (so a missing server is a refusal,
// not a fresh one), and -u keeps capture output UTF-8 whatever the locale.
// -N needs tmux 3.2 or newer; Available is false on an older tmux.
var globalFlags = []string{"-N", "-u"}

// NativeExec runs bin with a minimal environment. TMUX, TMUX_PANE and
// TMUX_TMPDIR are never inherited, so the default per-user socket is used and
// an inherited value can never redirect the target to another server.
func NativeExec(bin string) Exec {
	return func(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, tmuxTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, append(append([]string{}, globalFlags...), args...)...)
		cmd.WaitDelay = 100 * time.Millisecond
		cmd.Env = minimalEnv(os.LookupEnv)
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		out := &capped{limit: maxTmuxOutput}
		cmd.Stdout = out
		cmd.Stderr = io.Discard
		if cmd.Run() != nil {
			return nil, ErrTmuxUnavailable
		}
		return out.Bytes(), nil
	}
}

func minimalEnv(lookup func(string) (string, bool)) []string {
	var env []string
	for _, key := range []string{"PATH", "HOME"} {
		if value, ok := lookup(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

type capped struct {
	bytes.Buffer
	limit int
}

func (c *capped) Write(p []byte) (int, error) {
	if c.Len()+len(p) > c.limit {
		return 0, ErrTmuxUnavailable
	}
	return c.Buffer.Write(p)
}

// Available reports whether lane operations can run in this process: a
// supported platform, an unchanged effective user (tmux's socket is per
// real user), and a tmux that accepts the global flags.
func Available(ctx context.Context, run Exec) bool {
	if runtime.GOOS == "windows" || os.Getuid() != os.Geteuid() || run == nil {
		return false
	}
	out, err := run(ctx, nil, "-V")
	return err == nil && bytes.HasPrefix(out, []byte("tmux "))
}
