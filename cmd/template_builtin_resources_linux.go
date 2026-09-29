//go:build linux

package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

const (
	renderChildMemoryMax  = 2 << 30
	renderChildCPUQuota   = "200%"
	renderChildTasksMax   = 256
	renderChildRuntimeMax = 2 * time.Hour
)

var (
	lookupRenderSystemdRun = exec.LookPath
	renderEffectiveUID     = os.Geteuid
)

// newRenderLimitedCommand puts every media-render child in its own transient
// cgroup. Limits are inherited by Chromium's renderer/helper processes and by
// ffmpeg's threads, so a child cannot escape by forking. Two render children
// may overlap (Chromium + ffmpeg), making the job-wide maxima 4 GiB, 400% CPU,
// and 512 tasks. RuntimeMaxSec is the OS-owned wall-clock backstop; the
// enclosing context independently applies the same deadline in-process.
//
// There is deliberately no unconfined fallback: a render-capable Linux node
// must have systemd-run and an available system/user manager.
func newRenderLimitedCommand(ctx context.Context, binary string, args ...string) (*exec.Cmd, error) {
	systemdRun, err := lookupRenderSystemdRun("systemd-run")
	if err != nil {
		return nil, fmt.Errorf("render resource isolation unavailable: systemd-run not found: %w", err)
	}
	scopeArgs := []string{}
	if renderEffectiveUID() != 0 {
		scopeArgs = append(scopeArgs, "--user")
	}
	scopeArgs = append(scopeArgs,
		"--scope", "--quiet", "--collect",
		fmt.Sprintf("--property=MemoryMax=%d", renderChildMemoryMax),
		"--property=MemorySwapMax=0",
		"--property=CPUQuota="+renderChildCPUQuota,
		fmt.Sprintf("--property=TasksMax=%d", renderChildTasksMax),
		fmt.Sprintf("--property=RuntimeMaxSec=%ds", int64(renderChildRuntimeMax/time.Second)),
		"--", binary,
	)
	scopeArgs = append(scopeArgs, args...)
	return exec.CommandContext(ctx, systemdRun, scopeArgs...), nil
}
