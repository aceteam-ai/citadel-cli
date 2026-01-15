// internal/services/native.go
package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ServiceType represents how a service is run
type ServiceType string

const (
	ServiceTypeNative  ServiceType = "native"
	ServiceTypeDocker  ServiceType = "docker"
	ServiceTypeUnknown ServiceType = "unknown"
)

// NativeService represents a service that can run natively (without Docker)
type NativeService struct {
	Name        string
	Binary      string   // Primary binary name
	AltBinaries []string // Alternative binary names
	Port        int
	StartArgs   []string
	EnvVars     map[string]string
}

// NativeServices defines the known native services
var NativeServices = map[string]NativeService{
	"ollama": {
		Name:        "ollama",
		Binary:      "ollama",
		AltBinaries: []string{},
		Port:        11434,
		StartArgs:   []string{"serve"},
		EnvVars:     map[string]string{},
	},
	"llamacpp": {
		Name:        "llamacpp",
		Binary:      "llama-server",
		AltBinaries: []string{"llama-cpp-server", "server"},
		Port:        8080,
		StartArgs:   []string{"--host", "0.0.0.0", "--port", "8080"},
		EnvVars:     map[string]string{},
	},
	"vllm": {
		Name:        "vllm",
		Binary:      "vllm",
		AltBinaries: []string{"python -m vllm.entrypoints.openai.api_server"},
		Port:        8000,
		StartArgs:   []string{"serve"},
		EnvVars:     map[string]string{},
	},
}

// DetectServiceType checks if a service is available natively or via Docker
func DetectServiceType(serviceName string) ServiceType {
	// Check native first
	if IsNativeAvailable(serviceName) {
		return ServiceTypeNative
	}

	// Check Docker
	if IsDockerAvailable() {
		return ServiceTypeDocker
	}

	return ServiceTypeUnknown
}

// IsNativeAvailable checks if a service's native binary is available
func IsNativeAvailable(serviceName string) bool {
	service, ok := NativeServices[serviceName]
	if !ok {
		return false
	}

	// Check primary binary
	if _, err := exec.LookPath(service.Binary); err == nil {
		return true
	}

	// Check alternative binaries
	for _, alt := range service.AltBinaries {
		if _, err := exec.LookPath(alt); err == nil {
			return true
		}
	}

	return false
}

// IsDockerAvailable checks if Docker is available
func IsDockerAvailable() bool {
	_, err := exec.LookPath("docker")
	if err != nil {
		return false
	}

	// Also check if Docker daemon is running
	cmd := exec.Command("docker", "info")
	return cmd.Run() == nil
}

// GetNativeBinaryPath returns the path to the native binary for a service
func GetNativeBinaryPath(serviceName string) (string, error) {
	service, ok := NativeServices[serviceName]
	if !ok {
		return "", fmt.Errorf("unknown service: %s", serviceName)
	}

	// Check primary binary
	if path, err := exec.LookPath(service.Binary); err == nil {
		return path, nil
	}

	// Check alternative binaries
	for _, alt := range service.AltBinaries {
		if path, err := exec.LookPath(alt); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("native binary not found for service: %s", serviceName)
}

// NativeProcess represents a running native service process
type NativeProcess struct {
	Name    string
	Cmd     *exec.Cmd
	LogFile *os.File
}

// StartNativeService starts a native service and returns the process
func StartNativeService(serviceName string, logDir string) (*NativeProcess, error) {
	service, ok := NativeServices[serviceName]
	if !ok {
		return nil, fmt.Errorf("unknown service: %s", serviceName)
	}

	binaryPath, err := GetNativeBinaryPath(serviceName)
	if err != nil {
		return nil, err
	}

	// Create log file
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	logPath := fmt.Sprintf("%s/%s.log", logDir, serviceName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to create log file: %w", err)
	}

	// Build command
	cmd := exec.Command(binaryPath, service.StartArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// Set environment variables
	cmd.Env = os.Environ()
	for k, v := range service.EnvVars {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	// Set platform-specific process attributes
	setSysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("failed to start service: %w", err)
	}

	return &NativeProcess{
		Name:    serviceName,
		Cmd:     cmd,
		LogFile: logFile,
	}, nil
}

// IsNativeServiceRunning checks if a native service is already running
func IsNativeServiceRunning(serviceName string) bool {
	service, ok := NativeServices[serviceName]
	if !ok {
		return false
	}

	// Check if process is running by looking for the binary in process list
	// This is a simple check using pgrep
	cmd := exec.Command("pgrep", "-f", service.Binary)
	return cmd.Run() == nil
}

// StopNativeService stops a running native service
func StopNativeService(serviceName string) error {
	service, ok := NativeServices[serviceName]
	if !ok {
		return fmt.Errorf("unknown service: %s", serviceName)
	}

	// Find and kill the process
	cmd := exec.Command("pkill", "-f", service.Binary)
	if err := cmd.Run(); err != nil {
		// Check if it's just "no process found" which is OK
		if strings.Contains(err.Error(), "no process") {
			return nil
		}
		return fmt.Errorf("failed to stop service: %w", err)
	}

	return nil
}

// WaitForServiceReady waits for a service to be ready by checking its port
func WaitForServiceReady(serviceName string, timeout time.Duration) error {
	service, ok := NativeServices[serviceName]
	if !ok {
		return fmt.Errorf("unknown service: %s", serviceName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for service %s to be ready on port %d", serviceName, service.Port)
		default:
			// Try to connect to the port
			cmd := exec.Command("nc", "-z", "localhost", fmt.Sprintf("%d", service.Port))
			if cmd.Run() == nil {
				return nil
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// GetServicePort returns the port for a service
func GetServicePort(serviceName string) (int, bool) {
	service, ok := NativeServices[serviceName]
	if !ok {
		return 0, false
	}
	return service.Port, true
}
