package localcobrowse

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Local mode allows one co-browse owner per OS user (design section 2.1): if two
// harnesses each start `citadel local mcp`, the first to use co-browse owns the
// browser sessions and the second gets an OwnerError naming the first. The
// second process still serves every other local tool.

const ownerInfoFile = "local-cobrowse-owner.json"

// OwnerInfo describes the process holding the owner lock.
type OwnerInfo struct {
	PID    int    `json:"pid"`
	Client string `json:"client,omitempty"`
	Since  string `json:"since"`
}

// OwnerError is returned when another process already owns co-browse.
type OwnerError struct {
	Info  OwnerInfo
	Known bool
}

func (e *OwnerError) Error() string {
	if !e.Known {
		return "another Citadel process already owns co-browse on this computer. Close the other agent app (or its Citadel MCP server) and try again; this process still serves the other local tools"
	}
	client := e.Info.Client
	if client == "" {
		client = "an unidentified agent"
	}
	return fmt.Sprintf("another Citadel process already owns co-browse on this computer (pid %d, used by %s, since %s). "+
		"Close that agent app or use it instead; this process still serves the other local tools", e.Info.PID, client, e.Info.Since)
}

// Owner is a held owner lock.
type Owner struct {
	dir     string
	mu      sync.Mutex
	info    OwnerInfo
	release func()
}

// AcquireOwner takes the per-user owner lock, keeping its marker files in dir.
func AcquireOwner(dir string) (*Owner, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, ownerErrorFrom(dir)
		}
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	release, err := acquireOSLock(dir)
	if err != nil {
		return nil, err
	}
	o := &Owner{dir: dir, release: release, info: OwnerInfo{PID: os.Getpid(), Since: time.Now().UTC().Format(time.RFC3339)}}
	o.write()
	return o, nil
}

// SetClient records the connected MCP client name in the owner marker.
func (o *Owner) SetClient(name string) {
	o.mu.Lock()
	o.info.Client = name
	o.mu.Unlock()
	o.write()
}

// Release drops the lock. Idempotent.
func (o *Owner) Release() {
	o.mu.Lock()
	rel := o.release
	o.release = nil
	o.mu.Unlock()
	if rel != nil {
		_ = os.Remove(filepath.Join(o.dir, ownerInfoFile))
		rel()
	}
}

func (o *Owner) write() {
	o.mu.Lock()
	data, _ := json.Marshal(o.info)
	o.mu.Unlock()
	tmp := filepath.Join(o.dir, ownerInfoFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		_ = os.Rename(tmp, filepath.Join(o.dir, ownerInfoFile))
	}
}

func ownerErrorFrom(dir string) *OwnerError {
	data, err := os.ReadFile(filepath.Join(dir, ownerInfoFile))
	if err != nil {
		return &OwnerError{}
	}
	var info OwnerInfo
	if json.Unmarshal(data, &info) != nil || info.PID == 0 {
		return &OwnerError{}
	}
	return &OwnerError{Info: info, Known: true}
}
