//go:build linux

package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

const (
	renderChildMemoryMax   = 2 << 30
	renderChildCPUQuota    = "200%"
	renderChildTasksMax    = 256
	renderChildRuntimeMax  = 2 * time.Hour
	renderScopeStopTimeout = 10 * time.Second
)

var renderLimitControllers = []string{"cpu", "memory", "pids"}

var (
	lookupRenderSystemdRun  = exec.LookPath
	lookupRenderSystemctl   = exec.LookPath
	lookupRenderEnv         = exec.LookPath
	renderEffectiveUID      = os.Geteuid
	checkRenderDelegation   = platform.CheckRootlessCgroupDelegation
	allocateRenderScopeUnit = newRenderScopeUnit
	runRenderSystemctl      = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = minimalRenderControlEnv()
		return cmd.CombinedOutput()
	}
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
func newRenderLimitedCommand(ctx context.Context, childEnv []string, binary string, args ...string) (*renderCommand, error) {
	userManager := renderEffectiveUID() != 0
	if userManager {
		health := checkRenderDelegation(renderLimitControllers)
		if !health.OK {
			message := health.String()
			if len(health.Missing) != 0 {
				message = "missing delegated controllers: " + strings.Join(health.Missing, ", ")
				if health.Path != "" {
					message += " at " + health.Path
				}
			}
			if health.Hint != "" {
				message += " (" + health.Hint + ")"
			}
			return nil, fmt.Errorf("render resource isolation unavailable: %s", message)
		}
	}
	systemdRun, err := lookupRenderSystemdRun("systemd-run")
	if err != nil {
		return nil, fmt.Errorf("render resource isolation unavailable: systemd-run not found: %w", err)
	}
	systemctl, err := lookupRenderSystemctl("systemctl")
	if err != nil {
		return nil, fmt.Errorf("render resource isolation unavailable: systemctl not found for scope cleanup: %w", err)
	}
	envBinary, err := lookupRenderEnv("env")
	if err != nil {
		return nil, fmt.Errorf("render child environment isolation unavailable: env not found: %w", err)
	}
	unit, err := allocateRenderScopeUnit()
	if err != nil {
		return nil, err
	}
	scopeArgs := []string{}
	if userManager {
		scopeArgs = append(scopeArgs, "--user")
	}
	scopeArgs = append(scopeArgs,
		"--scope", "--quiet", "--collect", "--unit="+unit,
		fmt.Sprintf("--property=MemoryMax=%d", renderChildMemoryMax),
		"--property=MemorySwapMax=0",
		"--property=CPUQuota="+renderChildCPUQuota,
		fmt.Sprintf("--property=TasksMax=%d", renderChildTasksMax),
		fmt.Sprintf("--property=RuntimeMaxSec=%ds", int64(renderChildRuntimeMax/time.Second)),
		"--", envBinary, "-i",
	)
	scopeArgs = append(scopeArgs, childEnv...)
	scopeArgs = append(scopeArgs, binary)
	scopeArgs = append(scopeArgs, args...)
	cmd := exec.CommandContext(ctx, systemdRun, scopeArgs...)
	cmd.Env = minimalRenderControlEnv()
	return &renderCommand{
		cmd: cmd,
		stopScope: func() error {
			return stopRenderSystemdScope(systemctl, unit, userManager)
		},
	}, nil
}

func newRenderScopeUnit() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("allocate render systemd scope: %w", err)
	}
	return "citadel-render-" + hex.EncodeToString(id[:]) + ".scope", nil
}

func stopRenderSystemdScope(systemctl, unit string, userManager bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), renderScopeStopTimeout)
	defer cancel()
	args := []string{}
	if userManager {
		args = append(args, "--user")
	}
	args = append(args, "stop", unit)
	out, err := runRenderSystemctl(ctx, systemctl, args...)
	if err == nil {
		return nil
	}
	// --collect removes an already-empty transient scope. systemctl uses exit 5
	// when that unit has disappeared between process exit and cleanup; that race
	// is success, not evidence that descendants survived.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 5 {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("stop render scope %s: %w", unit, ctx.Err())
	}
	detail := strings.TrimSpace(string(out))
	if detail != "" {
		return fmt.Errorf("stop render scope %s: %w: %s", unit, err, detail)
	}
	return fmt.Errorf("stop render scope %s: %w", unit, err)
}
