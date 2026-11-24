//go:build darwin

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/update"
)

// NewUserLaunchAgent is the real agent for the invoking user: their home, their
// own uid (never SUDO_UID, so a sudo run refuses instead of reaching into the
// invoking user's session), and the real launchctl.
func NewUserLaunchAgent() (*LaunchAgent, error) {
	home, err := platform.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}
	return &LaunchAgent{
		Home:    home,
		UID:     os.Getuid(),
		Euid:    os.Geteuid(),
		Run:     runLaunchctl,
		StatExe: statExe,
	}, nil
}

// runLaunchctl runs launchctl with a deadline and a capped output.
func runLaunchctl(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, "launchctl", args...)
	cmd.Stdout = &limitedBuffer{w: &buf, max: launchctlOutputMax}
	cmd.Stderr = cmd.Stdout
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return buf.Bytes(), &LaunchctlExit{Code: ee.ExitCode(), Output: buf.String()}
	}
	return buf.Bytes(), err
}

type limitedBuffer struct {
	w   *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.w.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		l.w.Write(p[:room])
	}
	return len(p), nil
}

func statExe(path string) (ExeInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return ExeInfo{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ExeInfo{}, errors.New("no owner information")
	}
	return ExeInfo{UID: int(st.Uid), Mode: fi.Mode()}, nil
}

// stableExe resolves exe to the path the plist bakes: symlinks resolved, then a
// Homebrew Cellar path replaced by the stable <prefix>/bin symlink.
func stableExe(exe string) string {
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe, _ = update.StableDarwinExecPath(real)
	}
	return exe
}

// EnableUserAutostart installs the per-user LaunchAgent plist for exe. It does
// not start the worker; StartUserWorker does.
func EnableUserAutostart(exe string) error {
	a, err := NewUserLaunchAgent()
	if err != nil {
		return err
	}
	return a.Enable(stableExe(exe))
}

// DisableUserAutostart removes the LaunchAgent plist. A loaded job keeps
// running until StopUserWorker.
func DisableUserAutostart() error {
	a, err := NewUserLaunchAgent()
	if err != nil {
		return err
	}
	return a.Remove()
}

// StartUserWorker loads the LaunchAgent, which starts the worker.
func StartUserWorker() error {
	a, err := NewUserLaunchAgent()
	if err != nil {
		return err
	}
	return a.Start()
}

// StopUserWorker boots the LaunchAgent out of launchd.
func StopUserWorker() error {
	a, err := NewUserLaunchAgent()
	if err != nil {
		return err
	}
	return a.Stop()
}

// ReadUserAgentState reads the agent for thisExe.
func ReadUserAgentState(thisExe string) (LaunchAgentState, error) {
	a, err := NewUserLaunchAgent()
	if err != nil {
		return LaunchAgentState{}, err
	}
	return a.State(stableExe(thisExe)), nil
}

// UserAutostart reports the plist as the autostart status.
func UserAutostart(thisExe string) (UserAutostartStatus, error) {
	st, err := ReadUserAgentState(thisExe)
	if err != nil {
		return UserAutostartStatus{}, err
	}
	return AutostartStatusFromAgent(st), nil
}
