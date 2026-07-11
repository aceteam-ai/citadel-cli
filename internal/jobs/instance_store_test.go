// internal/jobs/instance_store_test.go
package jobs

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *instanceStore {
	t.Helper()
	return &instanceStore{path: filepath.Join(t.TempDir(), "instances", "state.json")}
}

func TestInstanceStore_PutGetDelete(t *testing.T) {
	s := newTestStore(t)

	// Missing file -> empty, not found, no error.
	if _, ok, err := s.Get("ac-x"); err != nil || ok {
		t.Fatalf("Get on empty store: ok=%v err=%v", ok, err)
	}

	rec := InstanceRecord{
		ServiceName:     "ac-x",
		InstanceID:      "i-1",
		ContainerName:   "citadel-ac-x",
		Image:           "ghcr.io/aceteam-ai/claudecode-service:latest",
		HostPort:        18789,
		ContainerPort:   8787,
		StateVolumePath: "/home/citadel/citadel-cache/instances/i-1",
		StateMountPath:  "/state",
	}
	if err := s.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok, err := s.Get("ac-x")
	if err != nil || !ok {
		t.Fatalf("Get after Put: ok=%v err=%v", ok, err)
	}
	if got != rec {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, rec)
	}

	// Delete is idempotent.
	if err := s.Delete("ac-x"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := s.Get("ac-x"); ok {
		t.Errorf("record still present after Delete")
	}
	if err := s.Delete("ac-x"); err != nil {
		t.Errorf("Delete of absent record should be no-op, got %v", err)
	}
}
