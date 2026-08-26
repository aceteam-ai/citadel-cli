// internal/servicediag/docker.go
//
// The real, docker/podman-backed Inspector. Read-only: only `inspect` and
// `logs` are ever invoked, never `up`/`down`/`restart`/`rm` -- diagnose must
// never have a side effect on the container it's diagnosing.
package servicediag

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// inspectTimeout bounds each docker/podman exec so a hung engine/daemon can't
// stall the diagnose command indefinitely. Mirrors resmon.collectTimeout.
const inspectTimeout = 5 * time.Second

// DockerInspector is the real Inspector, driving the given engine binary
// ("docker" or "podman" -- see catalog.ContainerRuntime.EngineBin; callers in
// cmd/ resolve the runtime once via catalog.SelectContainerRuntime and pass
// its EngineBin here rather than this package selecting its own).
type DockerInspector struct {
	EngineBin string
}

// NewDockerInspector constructs a DockerInspector for the given engine
// binary. Panics are never a concern here -- an empty/invalid EngineBin
// simply makes every exec fail, which Inspect/LogTail surface as an error,
// not a crash.
func NewDockerInspector(engineBin string) DockerInspector {
	return DockerInspector{EngineBin: engineBin}
}

func (d DockerInspector) Inspect(containerName string) (ContainerState, error) {
	if containerName == "" {
		return ContainerState{}, fmt.Errorf("empty container name")
	}
	ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.EngineBin, "inspect", "--format",
		"{{.State.Status}}\t{{.State.ExitCode}}\t{{.State.Running}}", containerName)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if isNoSuchContainer(stderr.String()) {
			return ContainerState{Found: false}, nil
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return ContainerState{}, fmt.Errorf("%s inspect failed: %s", d.EngineBin, msg)
	}

	fields := strings.Split(strings.TrimSpace(string(out)), "\t")
	if len(fields) != 3 {
		return ContainerState{}, fmt.Errorf("%s inspect returned unexpected output: %q", d.EngineBin, string(out))
	}
	exitCode, _ := strconv.Atoi(fields[1])
	return ContainerState{
		Found:    true,
		Status:   fields[0],
		ExitCode: exitCode,
		Running:  fields[2] == "true",
	}, nil
}

func (d DockerInspector) LogTail(containerName string, maxLines int) ([]string, error) {
	if containerName == "" {
		return nil, fmt.Errorf("empty container name")
	}
	if maxLines <= 0 {
		maxLines = defaultMaxLogLines
	}
	ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.EngineBin, "logs", "--tail", strconv.Itoa(maxLines), containerName)
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		if isNoSuchContainer(text) {
			return nil, nil
		}
		msg := strings.TrimSpace(text)
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s logs failed: %s", d.EngineBin, msg)
	}
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// isNoSuchContainer reports whether an inspect/logs failure looks like "the
// container does not exist" rather than a real plumbing failure (daemon
// unreachable, engine missing). Both docker and podman phrase this as some
// variant of "No such container"/"no such object".
func isNoSuchContainer(stderrText string) bool {
	lower := strings.ToLower(stderrText)
	return strings.Contains(lower, "no such container") || strings.Contains(lower, "no such object")
}
