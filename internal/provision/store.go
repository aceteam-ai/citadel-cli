package provision

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const storeFileName = "resources.json"

// Store persists provisioned resource state to disk as JSON.
// It is safe for concurrent use.
type Store struct {
	mu   sync.RWMutex
	path string

	// resources is the in-memory cache, keyed by resource ID.
	resources map[string]*Resource
}

// NewStore creates a Store backed by configDir/resources.json. It loads
// existing state from disk on creation. If the file does not exist, the
// store starts empty.
func NewStore(configDir string) (*Store, error) {
	s := &Store{
		path:      filepath.Join(configDir, storeFileName),
		resources: make(map[string]*Resource),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// List returns all resources. The caller must not mutate the returned slice.
func (s *Store) List() []*Resource {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*Resource, 0, len(s.resources))
	for _, r := range s.resources {
		out = append(out, r)
	}
	return out
}

// Get returns a resource by ID, or nil if not found.
func (s *Store) Get(id string) *Resource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resources[id]
}

// FindByName returns the first resource with the given name, or nil.
func (s *Store) FindByName(name string) *Resource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.resources {
		if r.Spec.Name == name {
			return r
		}
	}
	return nil
}

// Put adds or updates a resource and persists to disk.
func (s *Store) Put(r *Resource) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.resources[r.ID] = r
	return s.save()
}

// Delete removes a resource by ID and persists to disk.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.resources, id)
	return s.save()
}

// load reads the store file from disk into memory.
func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading resource store: %w", err)
	}

	var resources []*Resource
	if err := json.Unmarshal(data, &resources); err != nil {
		return fmt.Errorf("parsing resource store: %w", err)
	}

	for _, r := range resources {
		s.resources[r.ID] = r
	}
	return nil
}

// save writes the in-memory state to disk. Caller must hold s.mu.
func (s *Store) save() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating store directory: %w", err)
	}

	resources := make([]*Resource, 0, len(s.resources))
	for _, r := range s.resources {
		resources = append(resources, r)
	}

	data, err := json.MarshalIndent(resources, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling resource store: %w", err)
	}

	return os.WriteFile(s.path, data, 0600)
}
