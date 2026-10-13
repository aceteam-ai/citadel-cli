package clockstate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	commandBudget    = time.Second
	probeBudget      = 2 * time.Second
	commandWaitDelay = 100 * time.Millisecond
	outputLimit      = 4096
)

var errProbeUnavailable = errors.New("clock observation unavailable")

type commandRunner func(context.Context, string, ...string) (string, error)

func probeClock(ctx context.Context) State {
	if runtime.GOOS != "linux" {
		return Unknown
	}
	return probeWith(ctx, runCommand)
}

func probeWith(parent context.Context, run commandRunner) State {
	ctx, cancel := context.WithTimeout(parent, probeBudget)
	defer cancel()
	invoke := func(name string, args ...string) (string, error) {
		cmdCtx, stop := context.WithTimeout(ctx, commandBudget)
		defer stop()
		out, err := run(cmdCtx, name, args...)
		if cmdCtx.Err() != nil || len(out) > outputLimit {
			return "", errProbeUnavailable
		}
		return out, err
	}
	out, err := invoke("timedatectl", "show", "-p", "NTPSynchronized")
	if err == nil {
		if state, valid := parseTimedatectl(out); valid {
			return state
		}
	}
	if ctx.Err() != nil {
		return Unknown
	}
	out, err = invoke("chronyc", "tracking")
	if err != nil {
		return Unknown
	}
	return parseChronyc(out)
}

type cappedOutput struct{ bytes.Buffer }

func (b *cappedOutput) Write(p []byte) (int, error) {
	if len(p) > outputLimit-b.Len() {
		return 0, errProbeUnavailable
	}
	return b.Buffer.Write(p)
}

// Fixed directories avoid inherited PATH, DBus overrides, credentials or a
// caller-supplied command. The command's environment contains locale only.
func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	if name != "timedatectl" && name != "chronyc" {
		return "", errProbeUnavailable
	}
	var path string
	for _, dir := range []string{"/usr/bin", "/bin", "/usr/local/bin"} {
		candidate := filepath.Join(dir, name)
		if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0111 != 0 {
			path = candidate
			break
		}
	}
	if path == "" {
		return "", errProbeUnavailable
	}
	cmd := clockCommand(ctx, path, args...)
	var out cappedOutput
	cmd.Stdout = &out
	// nil stderr is os.DevNull, never surfaced in telemetry.
	if err := cmd.Run(); err != nil {
		return "", errProbeUnavailable
	}
	return out.String(), nil
}

// Construction is inert, permitting fixtures to inspect fixed environment and
// cancellation grace without running or looking up a host executable.
func clockCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"LC_ALL=C"}
	cmd.WaitDelay = commandWaitDelay
	return cmd
}

func parseTimedatectl(out string) (State, bool) {
	if len(out) > outputLimit {
		return Unknown, false
	}
	switch strings.TrimSpace(out) {
	case "NTPSynchronized=yes":
		return Synced, true
	case "NTPSynchronized=no":
		return Unsynced, true
	default:
		return Unknown, false
	}
}

func parseChronyc(out string) State {
	if len(out) > outputLimit {
		return Unknown
	}
	fields := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return Unknown
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key == "" || value == "" {
			return Unknown
		}
		if _, duplicate := fields[key]; duplicate {
			return Unknown
		}
		fields[key] = value
	}
	leap := fields["Leap status"]
	if fields["Reference ID"] == "" {
		return Unknown
	}
	if leap == "Not synchronised" {
		return Unsynced
	}
	if leap != "Normal" && leap != "Insert second" && leap != "Delete second" {
		return Unknown
	}
	ref := strings.Fields(fields["Reference ID"])
	if len(ref) < 2 || len(ref[0]) != 8 {
		return Unknown
	}
	for _, c := range ref[0] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return Unknown
		}
	}
	id := strings.ToUpper(ref[0])
	if id == "00000000" || strings.HasPrefix(id, "7F") {
		return Unknown
	}
	external := strings.TrimSpace(strings.Join(ref[1:], " "))
	if !strings.HasPrefix(external, "(") || !strings.HasSuffix(external, ")") {
		return Unknown
	}
	external = strings.Trim(external, "()")
	if external == "" || external == "0.0.0.0" || strings.Contains(strings.ToUpper(external), "LOCAL") {
		return Unknown
	}
	return Synced
}
