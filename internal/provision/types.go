// Package provision implements resource lifecycle management for the
// Infrastructure Canvas. It provides a unified API for creating, destroying,
// and inspecting provisioned resources (Docker containers, LXC containers,
// VMs) on a Citadel node.
package provision

import "time"

// ResourceType identifies the kind of provisioned resource.
type ResourceType string

const (
	ResourceTypeDocker ResourceType = "docker"
	ResourceTypeLXC    ResourceType = "lxc"
	ResourceTypeVM     ResourceType = "vm"
)

// ResourceStatus represents the lifecycle state of a resource.
type ResourceStatus string

const (
	StatusCreating  ResourceStatus = "creating"
	StatusRunning   ResourceStatus = "running"
	StatusStopped   ResourceStatus = "stopped"
	StatusError     ResourceStatus = "error"
	StatusDestroyed ResourceStatus = "destroyed"
)

// VolumeMount describes a bind mount from the host into a container.
type VolumeMount struct {
	// HostPath is the path on the host filesystem. Must be under an allowed
	// prefix (/tmp, workspace dir, or /var/lib/citadel/volumes).
	HostPath string `json:"host_path"`

	// ContainerPath is the mount target inside the container.
	ContainerPath string `json:"container_path"`

	// ReadOnly makes the mount read-only when true.
	ReadOnly bool `json:"read_only,omitempty"`
}

// PortMapping maps a host port to a container port.
type PortMapping struct {
	// HostPort is the port exposed on the host.
	HostPort int `json:"host_port"`

	// ContainerPort is the port inside the container.
	ContainerPort int `json:"container_port"`

	// Protocol is "tcp" or "udp". Defaults to "tcp".
	Protocol string `json:"protocol,omitempty"`
}

// ResourceSpec is the declarative specification for creating a resource.
type ResourceSpec struct {
	// Name is a human-readable identifier (must be unique per node).
	Name string `json:"name"`

	// Type selects the backend: "docker" (MVP), "lxc", or "vm" (future).
	Type ResourceType `json:"type"`

	// Image is the container image reference (e.g., "nginx:latest").
	// Required for docker and lxc types.
	Image string `json:"image,omitempty"`

	// Env is a map of environment variables injected into the resource.
	Env map[string]string `json:"env,omitempty"`

	// Ports maps host ports to container ports.
	Ports []PortMapping `json:"ports,omitempty"`

	// Volumes binds host paths into the container.
	Volumes []VolumeMount `json:"volumes,omitempty"`

	// Command overrides the container entrypoint.
	Command []string `json:"command,omitempty"`

	// CPUs limits CPU allocation (e.g., "0.5" for half a core).
	CPUs string `json:"cpus,omitempty"`

	// MemoryMB limits memory in megabytes.
	MemoryMB int `json:"memory_mb,omitempty"`

	// GPUs requests GPU access. "all" passes all GPUs; a number like "1"
	// requests that many.
	GPUs string `json:"gpus,omitempty"`
}

// Resource represents a provisioned resource with its runtime state.
type Resource struct {
	// ID is a unique identifier (UUID) assigned at creation.
	ID string `json:"id"`

	// Spec is the original creation specification.
	Spec ResourceSpec `json:"spec"`

	// Status is the current lifecycle state.
	Status ResourceStatus `json:"status"`

	// ContainerID is the backend-specific identifier (e.g., Docker container ID).
	ContainerID string `json:"container_id,omitempty"`

	// Error records the last error message, if Status == "error".
	Error string `json:"error,omitempty"`

	// CreatedAt is when the resource was first created.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is when the resource state last changed.
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateResult is returned by Manager.Create.
type CreateResult struct {
	Resource *Resource `json:"resource"`
	// Reused is true when an existing resource with the same name was returned
	// instead of creating a new one (idempotency).
	Reused bool `json:"reused,omitempty"`
}
