// Package harness provides test harness utilities for E2E testing
package harness

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// CitadelHarness wraps citadel-cli for E2E testing
type CitadelHarness struct {
	binaryPath string
	workDir    string
}

// NewCitadelHarness creates a new harness for testing citadel-cli
func NewCitadelHarness(binaryPath string) *CitadelHarness {
	workDir, _ := os.MkdirTemp("", "citadel-e2e-*")
	return &CitadelHarness{
		binaryPath: binaryPath,
		workDir:    workDir,
	}
}

// RunCommand executes a citadel command and returns the output
func (h *CitadelHarness) RunCommand(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, h.binaryPath, args...)
	cmd.Dir = h.workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("command failed: %w\nstdout: %s\nstderr: %s",
			err, stdout.String(), stderr.String())
	}

	return stdout.String(), nil
}

// StartWorker starts the citadel worker in the background
func (h *CitadelHarness) StartWorker(ctx context.Context, mode, redisURL, queue string) (*exec.Cmd, error) {
	args := []string{"work",
		"--mode=" + mode,
		"--redis-url=" + redisURL,
		"--queue=" + queue,
	}

	cmd := exec.CommandContext(ctx, h.binaryPath, args...)
	cmd.Dir = h.workDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start worker: %w", err)
	}

	return cmd, nil
}

// GenerateManifest creates a citadel.yaml manifest for testing
func (h *CitadelHarness) GenerateManifest(nodeName string, services []string) error {
	manifest := fmt.Sprintf(`node:
  name: %s
  tags:
    - e2e-test
services:`, nodeName)

	for _, svc := range services {
		manifest += fmt.Sprintf("\n  - name: %s", svc)
	}

	return os.WriteFile(filepath.Join(h.workDir, "citadel.yaml"), []byte(manifest), 0644)
}

// Cleanup removes the temporary work directory
func (h *CitadelHarness) Cleanup() error {
	return os.RemoveAll(h.workDir)
}

// WorkDir returns the working directory path
func (h *CitadelHarness) WorkDir() string {
	return h.workDir
}

// BuildCitadel builds the citadel binary and returns its path
func BuildCitadel(citadelDir string) (string, error) {
	outputPath := filepath.Join(citadelDir, "citadel-e2e")

	cmd := exec.Command("go", "build", "-o", outputPath, ".")
	cmd.Dir = citadelDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to build citadel: %w\noutput: %s", err, string(output))
	}

	return outputPath, nil
}
