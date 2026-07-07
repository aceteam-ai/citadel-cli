// internal/jobs/instance_store.go
//
// Persistent registry of payload-launched agent-runtime instances (BYOC, e.g.
// Claude Code on your own node -- citadel-cli#462 / aceteam#4588).
//
// These instances are launched from an inline SERVICE_START payload (image, env,
// host port, state volume) rather than a name in the node's citadel.yaml or the
// embedded ServiceMap. They are DELIBERATELY kept out of citadel.yaml: the
// desired-state reconciler (internal/reconcile, #353/#4273) treats citadel.yaml
// as a projection of the control-plane DesiredState and would read anything it
// finds there but not in DesiredState as drift-to-uninstall. Recording these
// instances in a separate store keeps them clear of that reconcile domain while
// still giving SERVICE_STOP / SERVICE_STATUS a way to find a container that
// exists in neither manifest.
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// InstanceRecord is the persisted spec of one payload-launched instance. It
// holds everything SERVICE_STOP / SERVICE_STATUS (and a future reconcile
// relaunch) need to act on the container without re-consulting the platform.
type InstanceRecord struct {
	// ServiceName is the platform-assigned service name (e.g. "ac-<shortcode>").
	// It is the key the platform uses on STOP/STATUS and the suffix of the
	// docker container name.
	ServiceName string `json:"service_name"`
	// InstanceID is the platform instance id (for cross-reference/logging).
	InstanceID string `json:"instance_id,omitempty"`
	// ContainerName is the docker container name ("citadel-<service_name>").
	ContainerName string `json:"container_name"`
	// Image is the validated container image reference.
	Image string `json:"image"`
	// HostPort is the host port the container's HTTP endpoint is published on.
	HostPort int `json:"host_port"`
	// ContainerPort is the in-container port the host port maps to.
	ContainerPort int `json:"container_port"`
	// StateVolumePath is the resolved (absolute) host path mounted for durable
	// per-instance state.
	StateVolumePath string `json:"state_volume_path"`
	// StateMountPath is the container path StateVolumePath is mounted at.
	StateMountPath string `json:"state_mount_path"`
}

// instanceStore persists the set of payload-launched instances to a JSON file.
type instanceStore struct {
	mu   sync.Mutex
	path string
}

// instanceStorePath returns ~/.citadel/instances/state.json (mirrors the
// per-app store in internal/apps). Kept under ~/.citadel (a citadel data dir),
// NOT under the citadel.yaml config dir, to keep these records out of the
// manifest/reconcile surface.
func instanceStorePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, ".citadel", "instances", "state.json"), nil
}

// newInstanceStore opens (or lazily creates) the store at the default path.
func newInstanceStore() (*instanceStore, error) {
	path, err := instanceStorePath()
	if err != nil {
		return nil, err
	}
	return &instanceStore{path: path}, nil
}

// load reads the current record set. A missing file is an empty set, not an
// error, so the first launch on a fresh node works.
func (s *instanceStore) load() (map[string]InstanceRecord, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]InstanceRecord{}, nil
		}
		return nil, fmt.Errorf("failed to read instance store %s: %w", s.path, err)
	}
	records := map[string]InstanceRecord{}
	if len(data) == 0 {
		return records, nil
	}
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("failed to parse instance store %s: %w", s.path, err)
	}
	return records, nil
}

// save writes the record set atomically (temp file + rename).
func (s *instanceStore) save(records map[string]InstanceRecord) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return fmt.Errorf("failed to create instance store dir: %w", err)
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal instance store: %w", err)
	}
	tmp := s.path + ".tmp"
	// 0600: env in the record set may reference sensitive values.
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("failed to write instance store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("failed to persist instance store: %w", err)
	}
	return nil
}

// Get returns the record for a service name, if present.
func (s *instanceStore) Get(serviceName string) (InstanceRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.load()
	if err != nil {
		return InstanceRecord{}, false, err
	}
	rec, ok := records[serviceName]
	return rec, ok, nil
}

// Put upserts a record (keyed by ServiceName) and persists.
func (s *instanceStore) Put(rec InstanceRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.load()
	if err != nil {
		return err
	}
	records[rec.ServiceName] = rec
	return s.save(records)
}

// Delete removes a record by service name and persists. Deleting a
// non-existent record is a no-op (idempotent).
func (s *instanceStore) Delete(serviceName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := records[serviceName]; !ok {
		return nil
	}
	delete(records, serviceName)
	return s.save(records)
}
