// Package apps provides a modular app catalog for deploying developer tools
// on Citadel nodes using Docker containers.
package apps

import (
	"crypto/rand"
	"math/big"
	"sort"
)

// GeneratePassword generates a random alphanumeric password.
func GeneratePassword(length int) (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, length)
	for i := range result {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", err
		}
		result[i] = charset[idx.Int64()]
	}
	return string(result), nil
}

// NeedsPassword returns true if the app needs a generated password at install time.
func NeedsPassword(name string) bool {
	return name == "code-server" || name == "filebrowser"
}

// AppManifest describes a deployable application in the catalog.
type AppManifest struct {
	Name        string            // Unique identifier (e.g., "code-server")
	Description string            // Human-readable description
	Image       string            // Docker image reference
	Ports       map[int]int       // Container port -> suggested host port
	Volumes     []VolumeMount     // Data volumes to persist
	Env         map[string]string // Default environment variables
	HealthCheck *HealthCheck      // Optional health check configuration
}

// VolumeMount describes a bind-mount from the host into the container.
type VolumeMount struct {
	// HostPath is relative to the app's data directory (~/.citadel/apps/<name>/data/).
	// An empty string means the data directory root.
	HostPath      string
	ContainerPath string
}

// HealthCheck describes how to verify an app is ready after starting.
type HealthCheck struct {
	// HTTPPath is a path to GET on the app's first exposed port.
	// A 2xx or 3xx response indicates healthy.
	HTTPPath string
	// IntervalSeconds is the time between retries.
	IntervalSeconds int
	// TimeoutSeconds is the maximum time to wait for the app to become healthy.
	TimeoutSeconds int
}

// builtinCatalog defines the apps available out of the box.
var builtinCatalog = map[string]AppManifest{
	"code-server": {
		Name:        "code-server",
		Description: "VS Code in the browser (code-server)",
		Image:       "linuxserver/code-server:latest",
		Ports:       map[int]int{8443: 8100},
		Volumes: []VolumeMount{
			{HostPath: "config", ContainerPath: "/config"},
		},
		Env: map[string]string{
			"PUID": "1000",
			"PGID": "1000",
			"TZ":   "UTC",
		},
		HealthCheck: &HealthCheck{
			HTTPPath:        "/",
			IntervalSeconds: 2,
			TimeoutSeconds:  60,
		},
	},
	"jupyter": {
		Name:        "jupyter",
		Description: "Jupyter Notebook for Python development",
		Image:       "jupyter/minimal-notebook:latest",
		Ports:       map[int]int{8888: 8101},
		Volumes: []VolumeMount{
			{HostPath: "work", ContainerPath: "/home/jovyan/work"},
		},
		Env: map[string]string{
			"JUPYTER_ENABLE_LAB": "yes",
		},
		HealthCheck: &HealthCheck{
			HTTPPath:        "/api",
			IntervalSeconds: 2,
			TimeoutSeconds:  60,
		},
	},
	"filebrowser": {
		Name:        "filebrowser",
		Description: "Web-based file manager",
		Image:       "filebrowser/filebrowser:latest",
		Ports:       map[int]int{80: 8102},
		Volumes: []VolumeMount{
			{HostPath: "data", ContainerPath: "/srv"},
		},
		Env: map[string]string{
			"FB_DATABASE": "/srv/filebrowser.db",
		},
		HealthCheck: &HealthCheck{
			HTTPPath:        "/",
			IntervalSeconds: 2,
			TimeoutSeconds:  30,
		},
	},
	"ollama-webui": {
		Name:        "ollama-webui",
		Description: "Open WebUI for local LLM chat (connects to Ollama)",
		Image:       "ghcr.io/open-webui/open-webui:main",
		Ports:       map[int]int{8080: 8103},
		Volumes: []VolumeMount{
			{HostPath: "data", ContainerPath: "/app/backend/data"},
		},
		Env: map[string]string{
			"OLLAMA_BASE_URL": "http://host.docker.internal:11434",
		},
		HealthCheck: &HealthCheck{
			HTTPPath:        "/",
			IntervalSeconds: 3,
			TimeoutSeconds:  90,
		},
	},
}

// Lookup returns the AppManifest for the given name, or false if not found.
func Lookup(name string) (AppManifest, bool) {
	m, ok := builtinCatalog[name]
	return m, ok
}

// List returns all app names in sorted order.
func List() []string {
	names := make([]string, 0, len(builtinCatalog))
	for k := range builtinCatalog {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// All returns all app manifests keyed by name.
func All() map[string]AppManifest {
	// Return a copy to prevent mutation.
	out := make(map[string]AppManifest, len(builtinCatalog))
	for k, v := range builtinCatalog {
		out[k] = v
	}
	return out
}
