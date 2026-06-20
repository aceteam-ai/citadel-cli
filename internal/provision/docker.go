package provision

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DockerBackend implements resource lifecycle operations using Docker.
// It shells out to the Docker CLI (compatible with CGO_ENABLED=0).
type DockerBackend struct {
	// DockerBin is the path to the docker binary. Defaults to "docker".
	DockerBin string
}

// NewDockerBackend creates a DockerBackend. It verifies that the docker
// binary is available on PATH.
func NewDockerBackend() (*DockerBackend, error) {
	bin := "docker"
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("docker not found on PATH: %w", err)
	}
	return &DockerBackend{DockerBin: bin}, nil
}

// Create pulls the image (if needed) and starts a container. It returns
// the Docker container ID.
func (b *DockerBackend) Create(ctx context.Context, id string, spec *ResourceSpec) (containerID string, err error) {
	if out, pullErr := b.run(ctx, "pull", spec.Image); pullErr != nil {
		return "", fmt.Errorf("docker pull failed: %s: %w", strings.TrimSpace(out), pullErr)
	}

	args := []string{
		"run", "-d",
		"--name", containerName(id, spec.Name),
		"--restart", "unless-stopped",
		"--label", "citadel.resource.id=" + id,
		"--label", "citadel.resource.name=" + spec.Name,
	}

	for k, v := range spec.Env {
		args = append(args, "-e", k+"="+v)
	}

	for _, p := range spec.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		args = append(args, "-p", fmt.Sprintf("%d:%d/%s", p.HostPort, p.ContainerPort, proto))
	}

	for _, v := range spec.Volumes {
		mount := v.HostPath + ":" + v.ContainerPath
		if v.ReadOnly {
			mount += ":ro"
		}
		args = append(args, "-v", mount)
	}

	if spec.CPUs != "" {
		args = append(args, "--cpus", spec.CPUs)
	}

	if spec.MemoryMB > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", spec.MemoryMB))
	}

	if spec.GPUs != "" {
		args = append(args, "--gpus", spec.GPUs)
	}

	if len(spec.Command) > 0 {
		args = append(args, spec.Image)
		args = append(args, spec.Command...)
	} else {
		args = append(args, spec.Image)
	}

	out, err := b.run(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("docker run failed: %s: %w", strings.TrimSpace(out), err)
	}

	cid := strings.TrimSpace(out)
	if len(cid) > 12 {
		cid = cid[:12]
	}
	return cid, nil
}

// Destroy stops and removes a container.
func (b *DockerBackend) Destroy(ctx context.Context, id string, spec *ResourceSpec) error {
	name := containerName(id, spec.Name)
	_, _ = b.run(ctx, "stop", name)
	out, err := b.run(ctx, "rm", "-f", name)
	if err != nil {
		return fmt.Errorf("docker rm failed: %s: %w", strings.TrimSpace(out), err)
	}
	return nil
}

// Inspect returns the running status of a container.
func (b *DockerBackend) Inspect(ctx context.Context, id string, spec *ResourceSpec) (ResourceStatus, error) {
	name := containerName(id, spec.Name)
	out, err := b.run(ctx, "inspect", "--format", "{{.State.Status}}", name)
	if err != nil {
		if strings.Contains(strings.ToLower(out), "no such") ||
			strings.Contains(strings.ToLower(err.Error()), "no such") {
			return StatusDestroyed, nil
		}
		return StatusError, fmt.Errorf("docker inspect failed: %s: %w", strings.TrimSpace(out), err)
	}

	state := strings.TrimSpace(out)
	switch state {
	case "running":
		return StatusRunning, nil
	case "exited", "dead":
		return StatusStopped, nil
	case "created", "restarting":
		return StatusCreating, nil
	default:
		return StatusError, fmt.Errorf("unknown docker state: %s", state)
	}
}

// Logs returns recent logs from a container.
func (b *DockerBackend) Logs(ctx context.Context, id string, spec *ResourceSpec, tail int) (string, error) {
	name := containerName(id, spec.Name)
	tailStr := "100"
	if tail > 0 {
		tailStr = fmt.Sprintf("%d", tail)
	}
	out, err := b.run(ctx, "logs", "--tail", tailStr, name)
	if err != nil {
		return "", fmt.Errorf("docker logs failed: %s: %w", strings.TrimSpace(out), err)
	}
	return out, nil
}

func containerName(id, specName string) string {
	return "citadel-" + specName
}

func (b *DockerBackend) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, b.DockerBin, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	return buf.String(), err
}
