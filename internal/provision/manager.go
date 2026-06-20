package provision

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Backend is the interface for a resource backend (Docker, LXC, VM).
type Backend interface {
	Create(ctx context.Context, id string, spec *ResourceSpec) (containerID string, err error)
	Destroy(ctx context.Context, id string, spec *ResourceSpec) error
	Inspect(ctx context.Context, id string, spec *ResourceSpec) (ResourceStatus, error)
	Logs(ctx context.Context, id string, spec *ResourceSpec, tail int) (string, error)
}

// Manager orchestrates resource lifecycle operations. It is safe for
// concurrent use.
type Manager struct {
	mu       sync.Mutex
	store    *Store
	backends map[ResourceType]Backend
}

// NewManager creates a Manager with the given store and backends.
func NewManager(store *Store, backends map[ResourceType]Backend) *Manager {
	return &Manager{
		store:    store,
		backends: backends,
	}
}

// Create provisions a new resource from the given spec. If a resource with
// the same name already exists and is not destroyed, the existing resource
// is returned (idempotency). The resource is persisted before and after
// the backend call so crash recovery can detect orphans.
func (m *Manager) Create(ctx context.Context, spec *ResourceSpec) (*CreateResult, error) {
	if err := ValidateSpec(spec); err != nil {
		return nil, fmt.Errorf("invalid spec: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing := m.store.FindByName(spec.Name); existing != nil {
		if existing.Status != StatusDestroyed {
			return &CreateResult{Resource: existing, Reused: true}, nil
		}
		_ = m.store.Delete(existing.ID)
	}

	backend, ok := m.backends[spec.Type]
	if !ok {
		return nil, fmt.Errorf("no backend for resource type %q", spec.Type)
	}

	now := time.Now().UTC()
	r := &Resource{
		ID:        uuid.New().String(),
		Spec:      *spec,
		Status:    StatusCreating,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := m.store.Put(r); err != nil {
		return nil, fmt.Errorf("persisting resource: %w", err)
	}

	containerID, err := backend.Create(ctx, r.ID, spec)
	if err != nil {
		r.Status = StatusError
		r.Error = err.Error()
		r.UpdatedAt = time.Now().UTC()
		_ = m.store.Put(r)
		return nil, fmt.Errorf("creating resource: %w", err)
	}

	r.ContainerID = containerID
	r.Status = StatusRunning
	r.UpdatedAt = time.Now().UTC()

	if err := m.store.Put(r); err != nil {
		return nil, fmt.Errorf("persisting resource after create: %w", err)
	}

	return &CreateResult{Resource: r}, nil
}

// Destroy stops and removes a resource.
func (m *Manager) Destroy(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	r := m.store.Get(id)
	if r == nil {
		return fmt.Errorf("resource %q not found", id)
	}

	if r.Status == StatusDestroyed {
		return nil
	}

	backend, ok := m.backends[r.Spec.Type]
	if !ok {
		return fmt.Errorf("no backend for resource type %q", r.Spec.Type)
	}

	if err := backend.Destroy(ctx, r.ID, &r.Spec); err != nil {
		r.Status = StatusError
		r.Error = err.Error()
		r.UpdatedAt = time.Now().UTC()
		_ = m.store.Put(r)
		return fmt.Errorf("destroying resource: %w", err)
	}

	r.Status = StatusDestroyed
	r.UpdatedAt = time.Now().UTC()

	return m.store.Put(r)
}

// Status returns the current status of a resource after reconciling with
// the backend.
func (m *Manager) Status(ctx context.Context, id string) (*Resource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	r := m.store.Get(id)
	if r == nil {
		return nil, fmt.Errorf("resource %q not found", id)
	}

	if r.Status == StatusDestroyed {
		return r, nil
	}

	backend, ok := m.backends[r.Spec.Type]
	if !ok {
		return r, nil
	}

	actualStatus, err := backend.Inspect(ctx, r.ID, &r.Spec)
	if err != nil {
		return r, fmt.Errorf("inspecting resource: %w", err)
	}

	if actualStatus != r.Status {
		r.Status = actualStatus
		r.UpdatedAt = time.Now().UTC()
		_ = m.store.Put(r)
	}

	return r, nil
}

// List returns all resources.
func (m *Manager) List() []*Resource {
	return m.store.List()
}

// Get returns a resource by ID without reconciling status.
func (m *Manager) Get(id string) *Resource {
	return m.store.Get(id)
}

// Logs returns recent logs for a resource.
func (m *Manager) Logs(ctx context.Context, id string, tail int) (string, error) {
	r := m.store.Get(id)
	if r == nil {
		return "", fmt.Errorf("resource %q not found", id)
	}

	backend, ok := m.backends[r.Spec.Type]
	if !ok {
		return "", fmt.Errorf("no backend for resource type %q", r.Spec.Type)
	}

	return backend.Logs(ctx, r.ID, &r.Spec, tail)
}

// ReconcileAll reconciles the status of all non-destroyed resources with
// their backends. This is used for crash recovery on startup.
func (m *Manager) ReconcileAll(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, r := range m.store.List() {
		if r.Status == StatusDestroyed {
			continue
		}

		backend, ok := m.backends[r.Spec.Type]
		if !ok {
			continue
		}

		actualStatus, err := backend.Inspect(ctx, r.ID, &r.Spec)
		if err != nil {
			log.Printf("[provision] reconcile %s (%s): %v", r.Spec.Name, r.ID, err)
			continue
		}

		if actualStatus != r.Status {
			log.Printf("[provision] reconcile %s (%s): %s -> %s", r.Spec.Name, r.ID, r.Status, actualStatus)
			r.Status = actualStatus
			r.UpdatedAt = time.Now().UTC()
			_ = m.store.Put(r)
		}
	}
}
