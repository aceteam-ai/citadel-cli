package harness

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// K8sHarness provides Kubernetes test utilities
type K8sHarness struct {
	kubeconfig string
	namespace  string
}

// NewK8sHarness creates a new Kubernetes harness
func NewK8sHarness(kubeconfig, namespace string) *K8sHarness {
	return &K8sHarness{
		kubeconfig: kubeconfig,
		namespace:  namespace,
	}
}

// kubectl runs a kubectl command and returns the output
func (h *K8sHarness) kubectl(args ...string) (string, error) {
	fullArgs := args
	if h.kubeconfig != "" {
		fullArgs = append([]string{"--kubeconfig", h.kubeconfig}, args...)
	}
	if h.namespace != "" {
		fullArgs = append(fullArgs, "-n", h.namespace)
	}

	cmd := exec.Command("kubectl", fullArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kubectl failed: %w\nstderr: %s", err, stderr.String())
	}

	return stdout.String(), nil
}

// WaitForDeployment waits for a deployment to be ready
func (h *K8sHarness) WaitForDeployment(ctx context.Context, name string, timeout time.Duration) error {
	args := []string{
		"wait", "--for=condition=available",
		fmt.Sprintf("deployment/%s", name),
		fmt.Sprintf("--timeout=%s", timeout.String()),
	}

	_, err := h.kubectl(args...)
	return err
}

// WaitForPods waits for pods matching a label selector to be ready
func (h *K8sHarness) WaitForPods(ctx context.Context, labelSelector string, timeout time.Duration) error {
	args := []string{
		"wait", "--for=condition=ready", "pod",
		"-l", labelSelector,
		fmt.Sprintf("--timeout=%s", timeout.String()),
	}

	_, err := h.kubectl(args...)
	return err
}

// PortForwardStart starts port forwarding to a service
func (h *K8sHarness) PortForwardStart(ctx context.Context, service string, localPort, remotePort int) (*exec.Cmd, error) {
	args := []string{
		"port-forward",
		fmt.Sprintf("svc/%s", service),
		fmt.Sprintf("%d:%d", localPort, remotePort),
	}

	if h.kubeconfig != "" {
		args = append([]string{"--kubeconfig", h.kubeconfig}, args...)
	}
	if h.namespace != "" {
		args = append(args, "-n", h.namespace)
	}

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start port forward: %w", err)
	}

	// Wait a bit for port forward to establish
	time.Sleep(2 * time.Second)

	return cmd, nil
}

// GetServiceURL returns the URL to access a service
func (h *K8sHarness) GetServiceURL(service string, port int) string {
	// For local testing, we use port-forwarding
	return fmt.Sprintf("http://localhost:%d", port)
}

// GetPodLogs retrieves logs from pods matching a label selector
func (h *K8sHarness) GetPodLogs(labelSelector string, tailLines int) (string, error) {
	args := []string{
		"logs",
		"-l", labelSelector,
		fmt.Sprintf("--tail=%d", tailLines),
	}

	return h.kubectl(args...)
}

// GetPods returns pod information for a label selector
func (h *K8sHarness) GetPods(labelSelector string) ([]string, error) {
	output, err := h.kubectl("get", "pods", "-l", labelSelector, "-o", "name")
	if err != nil {
		return nil, err
	}

	var pods []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line != "" {
			pods = append(pods, strings.TrimPrefix(line, "pod/"))
		}
	}

	return pods, nil
}

// ExecInPod executes a command in a pod
func (h *K8sHarness) ExecInPod(pod string, command ...string) (string, error) {
	args := append([]string{"exec", pod, "--"}, command...)
	return h.kubectl(args...)
}

// ApplyManifest applies a YAML manifest
func (h *K8sHarness) ApplyManifest(manifest string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	if h.kubeconfig != "" {
		cmd.Args = append([]string{cmd.Args[0], "--kubeconfig", h.kubeconfig}, cmd.Args[1:]...)
	}
	if h.namespace != "" {
		cmd.Args = append(cmd.Args, "-n", h.namespace)
	}

	cmd.Stdin = strings.NewReader(manifest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to apply manifest: %w\nstderr: %s", err, stderr.String())
	}

	return nil
}

// DeleteManifest deletes resources from a YAML manifest
func (h *K8sHarness) DeleteManifest(manifest string) error {
	cmd := exec.Command("kubectl", "delete", "-f", "-", "--ignore-not-found")
	if h.kubeconfig != "" {
		cmd.Args = append([]string{cmd.Args[0], "--kubeconfig", h.kubeconfig}, cmd.Args[1:]...)
	}
	if h.namespace != "" {
		cmd.Args = append(cmd.Args, "-n", h.namespace)
	}

	cmd.Stdin = strings.NewReader(manifest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to delete manifest: %w\nstderr: %s", err, stderr.String())
	}

	return nil
}
