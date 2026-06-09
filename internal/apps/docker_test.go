package apps

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// mockRunner records commands and returns pre-configured responses.
type mockRunner struct {
	calls   []string
	results map[string]mockResult
}

type mockResult struct {
	output string
	err    error
}

func newMockRunner() *mockRunner {
	return &mockRunner{
		results: make(map[string]mockResult),
	}
}

func (m *mockRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	m.calls = append(m.calls, cmd)

	// Match by prefix for flexible assertions.
	for prefix, result := range m.results {
		if strings.HasPrefix(cmd, prefix) {
			return result.output, result.err
		}
	}
	return "", nil
}

func (m *mockRunner) setResult(prefix, output string, err error) {
	m.results[prefix] = mockResult{output: output, err: err}
}

func (m *mockRunner) lastCall() string {
	if len(m.calls) == 0 {
		return ""
	}
	return m.calls[len(m.calls)-1]
}

func (m *mockRunner) callCount() int {
	return len(m.calls)
}

func TestDockerAvailable(t *testing.T) {
	ctx := context.Background()

	t.Run("docker available", func(t *testing.T) {
		runner := newMockRunner()
		runner.setResult("docker info", "ok", nil)
		if err := DockerAvailable(ctx, runner); err != nil {
			t.Errorf("DockerAvailable() error = %v, want nil", err)
		}
	})

	t.Run("docker not available", func(t *testing.T) {
		runner := newMockRunner()
		runner.setResult("docker info", "", fmt.Errorf("not found"))
		if err := DockerAvailable(ctx, runner); err == nil {
			t.Error("DockerAvailable() expected error when docker is missing")
		}
	})
}

func TestInstall(t *testing.T) {
	ctx := context.Background()
	runner := newMockRunner()
	runner.setResult("docker run", "abc123def456", nil)

	manifest := AppManifest{
		Name:  "test-app",
		Image: "test/image:latest",
		Ports: map[int]int{8080: 8100},
		Volumes: []VolumeMount{
			{HostPath: "data", ContainerPath: "/data"},
		},
		Env: map[string]string{"FOO": "bar"},
	}

	containerID, err := Install(ctx, runner, manifest, 8100, "/tmp/test-data")
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	if containerID != "abc123def456" {
		t.Errorf("Install() containerID = %q, want %q", containerID, "abc123def456")
	}

	// Verify the docker run command was called.
	if runner.callCount() != 1 {
		t.Fatalf("expected 1 call, got %d", runner.callCount())
	}

	call := runner.calls[0]
	if !strings.Contains(call, "docker run") {
		t.Error("expected docker run command")
	}
	if !strings.Contains(call, "--name citadel-app-test-app") {
		t.Errorf("expected container name in call: %s", call)
	}
	if !strings.Contains(call, "test/image:latest") {
		t.Errorf("expected image in call: %s", call)
	}
	if !strings.Contains(call, "-e FOO=bar") {
		t.Errorf("expected env var in call: %s", call)
	}
}

func TestInstallError(t *testing.T) {
	ctx := context.Background()
	runner := newMockRunner()
	runner.setResult("docker run", "error: something failed", fmt.Errorf("exit 1"))

	manifest := AppManifest{
		Name:  "test-app",
		Image: "test/image:latest",
		Ports: map[int]int{8080: 8100},
	}

	_, err := Install(ctx, runner, manifest, 8100, "/tmp/test-data")
	if err == nil {
		t.Fatal("Install() expected error")
	}
}

func TestStop(t *testing.T) {
	ctx := context.Background()
	runner := newMockRunner()
	runner.setResult("docker stop", "", nil)

	if err := Stop(ctx, runner, "test-app"); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	if !strings.Contains(runner.lastCall(), "docker stop citadel-app-test-app") {
		t.Errorf("unexpected command: %s", runner.lastCall())
	}
}

func TestStart(t *testing.T) {
	ctx := context.Background()
	runner := newMockRunner()
	runner.setResult("docker start", "", nil)

	if err := Start(ctx, runner, "test-app"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if !strings.Contains(runner.lastCall(), "docker start citadel-app-test-app") {
		t.Errorf("unexpected command: %s", runner.lastCall())
	}
}

func TestUninstall(t *testing.T) {
	ctx := context.Background()
	runner := newMockRunner()
	runner.setResult("docker rm", "", nil)

	if err := Uninstall(ctx, runner, "test-app"); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}

	call := runner.lastCall()
	if !strings.Contains(call, "docker rm -f citadel-app-test-app") {
		t.Errorf("unexpected command: %s", call)
	}
}

func TestUninstallIgnoresNoSuchContainer(t *testing.T) {
	ctx := context.Background()
	runner := newMockRunner()
	runner.setResult("docker rm", "Error: No such container: citadel-app-test-app", fmt.Errorf("exit 1"))

	// Should not return error for "no such container".
	if err := Uninstall(ctx, runner, "test-app"); err != nil {
		t.Fatalf("Uninstall() should not error for missing container: %v", err)
	}
}

func TestContainerStatus(t *testing.T) {
	ctx := context.Background()

	t.Run("running", func(t *testing.T) {
		runner := newMockRunner()
		runner.setResult("docker inspect", "running", nil)
		status := ContainerStatus(ctx, runner, "test-app")
		if status != "running" {
			t.Errorf("ContainerStatus() = %q, want %q", status, "running")
		}
	})

	t.Run("exited", func(t *testing.T) {
		runner := newMockRunner()
		runner.setResult("docker inspect", "exited", nil)
		status := ContainerStatus(ctx, runner, "test-app")
		if status != "exited" {
			t.Errorf("ContainerStatus() = %q, want %q", status, "exited")
		}
	})

	t.Run("not found", func(t *testing.T) {
		runner := newMockRunner()
		runner.setResult("docker inspect", "", fmt.Errorf("not found"))
		status := ContainerStatus(ctx, runner, "test-app")
		if status != "not_found" {
			t.Errorf("ContainerStatus() = %q, want %q", status, "not_found")
		}
	})
}

func TestContainerNameFormat(t *testing.T) {
	tests := []struct {
		app  string
		want string
	}{
		{"code-server", "citadel-app-code-server"},
		{"jupyter", "citadel-app-jupyter"},
		{"filebrowser", "citadel-app-filebrowser"},
		{"ollama-webui", "citadel-app-ollama-webui"},
	}

	for _, tt := range tests {
		got := ContainerName(tt.app)
		if got != tt.want {
			t.Errorf("ContainerName(%q) = %q, want %q", tt.app, got, tt.want)
		}
	}
}
