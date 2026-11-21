//go:build linux

package declaredsession

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestHostManagerRefusesAndSkipsContainerDeclarations(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged owner")
	}
	dir := privateFixtureDir(t)
	if err := os.WriteFile(filepath.Join(dir, "sessions.yaml"), []byte(containerFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m, err := NewManager(store, "/opt/citadel", "test")
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"start":  func() error { _, e := m.Start(context.Background(), "lane1"); return e },
		"stop":   func() error { _, e := m.Stop(context.Background(), "lane1"); return e },
		"status": func() error { _, e := m.Status(context.Background(), "lane1"); return e },
	} {
		if err := call(); !errors.Is(err, ErrContainerSession) {
			t.Errorf("%s err = %v, want ErrContainerSession", name, err)
		}
	}
	inv, err := m.Inventory(context.Background())
	if err != nil || len(inv) != 0 {
		t.Errorf("inventory must not list container sessions: %v, %v", inv, err)
	}
}
