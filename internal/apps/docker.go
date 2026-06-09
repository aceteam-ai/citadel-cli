package apps

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// CommandRunner abstracts command execution for testability.
type CommandRunner interface {
	// Run executes a command and returns combined stdout+stderr and error.
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner is the default CommandRunner that shells out to the OS.
type ExecRunner struct{}

// Run executes a command and returns combined output.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// DockerAvailable checks whether Docker is installed and the daemon is responsive.
func DockerAvailable(ctx context.Context, runner CommandRunner) error {
	_, err := runner.Run(ctx, "docker", "info")
	if err != nil {
		return fmt.Errorf("Docker is not available (is Docker installed and running?): %w", err)
	}
	return nil
}

// ContainerName returns the standardised container name for an app.
func ContainerName(appName string) string {
	return "citadel-app-" + appName
}

// Install pulls the image and starts a container for the given app manifest.
// It returns the container ID on success.
func Install(ctx context.Context, runner CommandRunner, manifest AppManifest, hostPort int, dataDir string) (string, error) {
	containerName := ContainerName(manifest.Name)

	// Build docker run arguments.
	args := []string{
		"run", "-d",
		"--name", containerName,
		"--restart", "unless-stopped",
	}

	// Port mapping: use a single host port mapped to the first container port.
	for containerPort := range manifest.Ports {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", hostPort, containerPort))
		break // only map the first port
	}

	// Volume mounts.
	for _, vol := range manifest.Volumes {
		hostPath := dataDir
		if vol.HostPath != "" {
			hostPath = dataDir + "/" + vol.HostPath
		}
		args = append(args, "-v", fmt.Sprintf("%s:%s", hostPath, vol.ContainerPath))
	}

	// Environment variables.
	for k, v := range manifest.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}

	// Add --add-host for host.docker.internal on Linux.
	args = append(args, "--add-host", "host.docker.internal:host-gateway")

	args = append(args, manifest.Image)

	output, err := runner.Run(ctx, "docker", args...)
	if err != nil {
		return "", fmt.Errorf("docker run failed: %s: %w", output, err)
	}

	// output is the container ID (first 12 chars from docker).
	containerID := output
	if len(containerID) > 12 {
		containerID = containerID[:12]
	}
	return containerID, nil
}

// Stop stops a running app container.
func Stop(ctx context.Context, runner CommandRunner, appName string) error {
	containerName := ContainerName(appName)
	output, err := runner.Run(ctx, "docker", "stop", containerName)
	if err != nil {
		return fmt.Errorf("docker stop failed: %s: %w", output, err)
	}
	return nil
}

// Start starts a previously stopped app container.
func Start(ctx context.Context, runner CommandRunner, appName string) error {
	containerName := ContainerName(appName)
	output, err := runner.Run(ctx, "docker", "start", containerName)
	if err != nil {
		return fmt.Errorf("docker start failed: %s: %w", output, err)
	}
	return nil
}

// Uninstall stops and removes the container, then optionally removes data.
func Uninstall(ctx context.Context, runner CommandRunner, appName string) error {
	containerName := ContainerName(appName)
	// Force remove (handles both running and stopped containers).
	output, err := runner.Run(ctx, "docker", "rm", "-f", containerName)
	if err != nil {
		// Ignore "no such container" errors.
		if !strings.Contains(output, "No such container") &&
			!strings.Contains(strings.ToLower(output), "no such container") {
			return fmt.Errorf("docker rm failed: %s: %w", output, err)
		}
	}
	return nil
}

// ContainerStatus returns "running", "exited", or "not_found" for a container.
func ContainerStatus(ctx context.Context, runner CommandRunner, appName string) string {
	containerName := ContainerName(appName)
	output, err := runner.Run(ctx, "docker", "inspect",
		"--format", "{{.State.Status}}", containerName)
	if err != nil {
		return "not_found"
	}
	status := strings.TrimSpace(output)
	if status == "" {
		return "not_found"
	}
	return status
}

// WaitHealthy polls the app's health check endpoint until it responds or times out.
func WaitHealthy(ctx context.Context, manifest AppManifest, hostPort int) error {
	if manifest.HealthCheck == nil {
		// No health check configured; wait a fixed 3 seconds.
		select {
		case <-time.After(3 * time.Second):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	hc := manifest.HealthCheck
	interval := time.Duration(hc.IntervalSeconds) * time.Second
	timeout := time.Duration(hc.TimeoutSeconds) * time.Second

	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://localhost:%d%s", hostPort, hc.HTTPPath)

	client := &http.Client{Timeout: 5 * time.Second}

	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return nil
			}
		}

		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return fmt.Errorf("app %s did not become healthy within %s", manifest.Name, timeout)
}
