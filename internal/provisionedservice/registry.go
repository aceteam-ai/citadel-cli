// Package provisionedservice is the single source of truth for which provisioned
// modules are exposed on the node's tsnet gateway and under which capability.
//
// A provisioned module (e.g. the WhatsApp bridge) binds an auto-selected free
// host port that nothing on the tsnet stack listens on. It is reached only
// through a gateway route registered under /modules/<prefix>/, which the gateway
// strips before forwarding to the module's loopback port. Which modules get a
// route, under what capability, and at which port is data -- not code. This
// registry persists that data to a small JSON file in the node state dir so:
//
//   - the running gateway can register/wire every declared module at startup and
//     pick up new ones without a restart (it watches this file), and
//   - the gateway's permission layer can map any /modules/<prefix>/... request to
//     the capability the owning module declared, with no hardcoded per-module
//     switch.
//
// Adding a future module therefore requires ZERO gateway source changes: the
// module's manifest declares its gateway: block, the provision flow writes an
// Entry here, and the gateway reacts.
package provisionedservice

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// registryFileName is the JSON file (in the node state dir) that persists the
// set of exposed provisioned modules.
const registryFileName = "provisioned-services.json"

// DefaultCapability is the permission that gates a module's gateway route when
// its manifest does not declare one. Provisioning is what deploys a module, so a
// module's route defaults to being gated by the same provision capability.
const DefaultCapability = "provision"

// Entry is one exposed provisioned module. It is the persisted, on-disk shape.
type Entry struct {
	// Name is the module's catalog/service name (stable identity, the key for
	// Remove). Distinct from Prefix so two names could in principle share nothing
	// but the prefix stays the route slug.
	Name string `json:"name"`
	// Prefix is the route slug: the gateway serves the module under
	// /modules/<prefix>/. Lowercase alphanumeric + dash, no slashes (validated at
	// the manifest layer; see catalog.GatewaySpec).
	Prefix string `json:"prefix"`
	// Port is the module's chosen loopback host port. Zero means "declared but not
	// yet deployed": the route is registered but its upstream is unset (502 until
	// the module is deployed and the port is wired).
	Port int `json:"port"`
	// Capability is the permission that gates the module's route (one of the
	// config.Permissions fields). Empty is treated as DefaultCapability by readers.
	Capability string `json:"capability"`
}

// Registry is a mutex-guarded, file-backed set of exposed provisioned modules.
// It is safe for concurrent use. The file is the single source of truth; every
// operation reads/writes it so separate processes (the CLI provisioning a module
// and the `citadel work` gateway) see each other's changes.
type Registry struct {
	path string
	mu   sync.Mutex
}

// New returns a Registry backed by the given file path. The file need not exist
// yet; List returns empty until the first Register.
func New(path string) *Registry {
	return &Registry{path: path}
}

// Path returns the backing file path (useful for a file watcher).
func (r *Registry) Path() string { return r.path }

// DefaultPath returns the registry file path inside the given node state dir.
// Callers pass network.GetStateDir() so the file colocates with the node's other
// persisted state and resolves consistently across sudo/user contexts.
func DefaultPath(stateDir string) string {
	return filepath.Join(stateDir, registryFileName)
}

// load reads the file into a slice. A missing file is an empty registry (not an
// error). Caller holds r.mu.
func (r *Registry) load() ([]Entry, error) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read provisioned-service registry: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse provisioned-service registry %s: %w", r.path, err)
	}
	return entries, nil
}

// save writes the slice atomically (temp file + rename) so a concurrent reader
// never sees a half-written file. Caller holds r.mu.
func (r *Registry) save(entries []Entry) error {
	// Stable order for deterministic output and readable diffs.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encode provisioned-service registry: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0755); err != nil {
		return fmt.Errorf("create state dir for provisioned-service registry: %w", err)
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write provisioned-service registry: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return fmt.Errorf("replace provisioned-service registry: %w", err)
	}
	return nil
}

// Register adds or updates an entry keyed by Name (an existing entry with the
// same Name is replaced, so re-provisioning a module updates its port/capability
// in place). Prefix must be non-empty; an empty Capability is stored as
// DefaultCapability so readers never have to special-case it.
func (r *Registry) Register(e Entry) error {
	if e.Name == "" {
		return fmt.Errorf("provisionedservice.Register: empty name")
	}
	if e.Prefix == "" {
		return fmt.Errorf("provisionedservice.Register: empty prefix for %q", e.Name)
	}
	if e.Capability == "" {
		e.Capability = DefaultCapability
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, err := r.load()
	if err != nil {
		return err
	}
	replaced := false
	for i := range entries {
		if entries[i].Name == e.Name {
			entries[i] = e
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, e)
	}
	return r.save(entries)
}

// Remove deletes the entry with the given Name. Removing an absent name is a
// no-op (no error), so teardown is idempotent.
func (r *Registry) Remove(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, err := r.load()
	if err != nil {
		return err
	}
	out := entries[:0]
	for _, e := range entries {
		if e.Name != name {
			out = append(out, e)
		}
	}
	return r.save(out)
}

// List returns a copy of all registered entries (empty when the file is absent).
func (r *Registry) List() ([]Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, err := r.load()
	if err != nil {
		return nil, err
	}
	// Normalize the capability on read so callers always see a concrete value.
	for i := range entries {
		if entries[i].Capability == "" {
			entries[i].Capability = DefaultCapability
		}
	}
	return entries, nil
}

// CapabilityForPrefix returns the capability gating the module served under the
// given prefix, and whether such a module is registered. A registered module
// with an empty stored capability reports DefaultCapability. This is the lookup
// the gateway's permission layer uses to gate /modules/<prefix>/... requests.
func (r *Registry) CapabilityForPrefix(prefix string) (capability string, found bool) {
	entries, err := r.List()
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.Prefix == prefix {
			if e.Capability == "" {
				return DefaultCapability, true
			}
			return e.Capability, true
		}
	}
	return "", false
}
