package declaredsession

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Store pins the already-selected, trusted node config directory. Configured
// ancestors may be aliases; below this anchor every component is no-follow.
// The same owning UID is trusted not to maliciously rename private directories
// between operations. This is not a sandbox against that UID or root.
type Store struct {
	root   string
	anchor *os.File
	uid    uint32
}

type SessionStore struct {
	store *Store
	dir   *os.File
	paths Paths
	// recordWait is selected before this SessionStore is published to holder
	// goroutines. Zero retains the manager's ordinary operation deadline.
	recordWait time.Duration
}

type OperationGuard struct {
	guard *fileGuard
}

type OwnerGuard struct {
	guard *fileGuard
}

type fileGuard struct {
	file    *os.File
	session *SessionStore
	kind    string
	mu      sync.Mutex
	closed  bool
}

func (s *Store) NodeDir() string {
	return s.root
}

func (s *SessionStore) Paths() Paths {
	return s.paths
}

// DirectoryFD is borrowed until SessionStore.Close. Only fixed local socket
// operations may use it; callers must not close it or turn peer input into paths.
func (s *SessionStore) DirectoryFD() int {
	return int(s.dir.Fd())
}

func (s *Store) Close() error {
	return closePrivate(s.anchor)
}

func (s *SessionStore) Close() error {
	return closePrivate(s.dir)
}

func LoadConfig(nodeDir string) (Snapshot, error) {
	store, err := OpenStore(nodeDir)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot, readErr := store.LoadConfig()
	closeErr := store.Close()
	if readErr != nil {
		return Snapshot{}, readErr
	}
	return snapshot, closeErr
}

func (s *Store) LoadConfig() (Snapshot, error) {
	raw, present, err := readPrivateAt(s.anchor, "sessions.yaml", MaxConfigBytes, s.uid)
	if err != nil || !present {
		return Snapshot{}, err
	}
	return DecodeConfig(raw)
}

func (s *SessionStore) ReadState() (State, bool, error) {
	raw, present, err := readPrivateAt(s.dir, "state.json", MaxStateBytes, s.store.uid)
	if err != nil || !present {
		return State{}, false, err
	}
	var state State
	if decodeRecord(raw, &state) != nil || s.validateState(state) != nil {
		return State{}, false, ErrUnsafe
	}
	return state, true, nil
}

// Writes use only the short record lock, never the manager's operation lock.
// A manager may hold OperationGuard while waiting for a holder RPC without
// blocking the holder's final state/log/clean-stop-marker publication.
func (s *SessionStore) WriteState(state State) error {
	if s.validateState(state) != nil {
		return ErrUnsafe
	}
	return s.withRecord(func() error { return s.writeJSON("state.json", state, MaxStateBytes) })
}

func (s *SessionStore) ReadStopMarker() (StopMarker, bool, error) {
	raw, present, err := readPrivateAt(s.dir, "stop.json", MaxStateBytes, s.store.uid)
	if err != nil || !present {
		return StopMarker{}, false, err
	}
	var marker StopMarker
	if decodeRecord(raw, &marker) != nil || marker.Version != 1 || marker.Name != s.paths.Name || !validDigest(marker.DeclarationRevision) || !validIncarnation(marker.Incarnation) || marker.StoppedAt.IsZero() {
		return StopMarker{}, false, ErrUnsafe
	}
	return marker, true, nil
}

// A marker is a per-NAME veto, not an expiring revision cache. A later revision
// mismatch must be shown as config_conflict, never treated as permission to run.
func (s *SessionStore) WriteStopMarker(marker StopMarker) error {
	if marker.Version != 1 || marker.Name != s.paths.Name || !validDigest(marker.DeclarationRevision) || !validIncarnation(marker.Incarnation) || marker.StoppedAt.IsZero() {
		return ErrUnsafe
	}
	return s.withRecord(func() error {
		old, exists, err := s.ReadStopMarker()
		if err != nil {
			return err
		}
		if exists {
			if old.Incarnation != marker.Incarnation || old.DeclarationRevision != marker.DeclarationRevision {
				return ErrConflict
			}
			return nil
		}
		return s.writeJSON("stop.json", marker, MaxStateBytes)
	})
}

// Only an explicit local start/restart may call this, after proving the previous
// holder AND exact scope are gone. This guard serializes operators, not that
// proof: stored PIDs and possession of the guard alone are never authority.
func (s *SessionStore) ClearStopMarker(guard *OperationGuard, expected StopMarker) error {
	if guard == nil || guard.guard == nil {
		return ErrConflict
	}
	g := guard.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.session != s || g.kind != "operation.lock" {
		return ErrConflict
	}
	return s.withRecord(func() error {
		marker, present, err := s.ReadStopMarker()
		if err != nil {
			return err
		}
		if !present || marker.Version != expected.Version || marker.Name != expected.Name || marker.Incarnation != expected.Incarnation || marker.DeclarationRevision != expected.DeclarationRevision || !marker.StoppedAt.Equal(expected.StoppedAt) {
			return ErrConflict
		}
		return removePrivateAt(s.dir, "stop.json", s.store.uid)
	})
}

func (s *SessionStore) AcquireOperation(ctx context.Context) (*OperationGuard, error) {
	guard, err := s.acquire(ctx, "operation.lock")
	if err != nil {
		return nil, err
	}
	return &OperationGuard{guard: guard}, nil
}

func (s *SessionStore) AcquireOwner(ctx context.Context) (*OwnerGuard, error) {
	guard, err := s.acquire(ctx, "host.lock")
	if err != nil {
		return nil, err
	}
	return &OwnerGuard{guard: guard}, nil
}

func (g *OperationGuard) Close() error {
	return g.guard.close()
}

func (g *OwnerGuard) Close() error {
	return g.guard.close()
}

func (s *SessionStore) withRecord(write func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.recordLockWait())
	defer cancel()
	// Each acquisition opens an independent file description, so flock also
	// serializes callers sharing this SessionStore without an unbounded mutex.
	guard, err := s.acquire(ctx, "record.lock")
	if err != nil {
		return err
	}
	writeErr := write()
	closeErr := guard.close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func (s *SessionStore) recordLockWait() time.Duration {
	if s.recordWait > 0 {
		return s.recordWait
	}
	return operationWait
}

func (s *SessionStore) writeJSON(name string, value any, max int) error {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > max {
		return ErrUnsafe
	}
	return writePrivateAt(s.dir, name, raw, max, s.store.uid)
}

func decodeRecord(raw []byte, target any) error {
	// Bound duplicate-key ambiguity before typed decoding (which otherwise
	// accepts last-wins). These records contain objects/scalars, never arrays.
	check := json.NewDecoder(strings.NewReader(string(raw)))
	check.UseNumber()
	if checkJSONValue(check, 0) != nil || check.Decode(new(any)) != io.EOF {
		return ErrUnsafe
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return ErrUnsafe
	}
	if decoder.Decode(new(any)) != io.EOF {
		return ErrUnsafe
	}
	return nil
}

func checkJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 8 {
		return ErrUnsafe
	}
	token, err := decoder.Token()
	if err != nil {
		return ErrUnsafe
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	if delim != '{' {
		return ErrUnsafe
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		key, stringKey := token.(string)
		if err != nil || !stringKey || !canonicalRecordKey(key) || seen[key] || len(seen) >= 128 {
			return ErrUnsafe
		}
		seen[key] = true
		if checkJSONValue(decoder, depth+1) != nil {
			return ErrUnsafe
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return ErrUnsafe
	}
	return nil
}

// Every protocol and record schema uses ASCII lower-snake JSON keys. Reject
// aliases before encoding/json's case-insensitive field matching can turn
// differently spelled keys into last-wins versions of the same typed field.
// Canonical but unknown fields are still refused by DisallowUnknownFields.
func canonicalRecordKey(key string) bool {
	if key == "" {
		return false
	}
	for _, ch := range key {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_') {
			return false
		}
	}
	return true
}

func validDigest(value string) bool {
	return len(value) == 71 && strings.HasPrefix(value, "sha256:") && strings.Trim(value[7:], "0123456789abcdef") == ""
}

func validIncarnation(value string) bool {
	return len(value) == 32 && strings.Trim(value, "0123456789abcdef") == ""
}

func (s *SessionStore) validateState(state State) error {
	if state.Version != 1 || state.Name != s.paths.Name || !validIncarnation(state.Incarnation) || !validDigest(state.DeclarationRevision) || !oneOf(state.Phase, "starting", "running", "restart_wait", "restarting", "stopping", "stopped", "stopped_clean", "disabled", "config_drift", "config_error", "config_conflict", "holder_lost", "holder_outdated", "isolation_unavailable", "name_conflict", "cleanup_unconfirmed", "spawn_failed", "event_queue_overflow", "crashloop", "error") {
		return ErrUnsafe
	}
	if state.Scope.Name != "" && state.Scope.Name != "citadel-sess-"+s.paths.Name+".scope" {
		return ErrUnsafe
	}
	if state.Holder.PID < 0 || state.Child.PID < 0 || len(state.BinaryVersion) > 128 || len(state.Scope.ControlGroup) > 4096 || (state.ResolvedExecutable != "" && !filepath.IsAbs(state.ResolvedExecutable)) || (state.EnvironmentHash != "" && !validDigest(state.EnvironmentHash)) {
		return ErrUnsafe
	}
	return nil
}
