package platform

import (
	"testing"
	"time"
)

// With no browser id known the watcher never attaches.
func TestChooserWatcherDoesNotAttachWithoutBrowserID(t *testing.T) {
	c := startChooserCDP(t, "B1", openedEvent(0))
	s := watcherSession(c.port, "")
	runWatcherBriefly(s, c.port, 1500*time.Millisecond)
	if n := c.conns.Load(); n != 0 {
		t.Fatalf("attached with an unknown browser id (%d connections)", n)
	}
	if st, _ := s.statusBase(); st.FileChooser != nil {
		t.Fatal("a chooser was recorded without a verified browser")
	}
}

// Launch learns the browser id before the watcher starts, so the watcher
// attaches (and only then) on every OS, whether or not a registry exists.
func TestLaunchRecordsBrowserIDBeforeTheWatcherAttaches(t *testing.T) {
	c := startChooserCDP(t, "B1", nil)
	installFixedPortLauncher(t, c.port)
	m := newCobrowseSessionManager(trustedBaseDir(t), 2)
	st, err := m.StartSession("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.StopAll() })
	s, _ := m.sessionForAction(st.ID)
	s.mu.Lock()
	id := s.proc.browserID
	s.mu.Unlock()
	if id != "B1" {
		t.Fatalf("browser id at launch = %q", id)
	}
	deadline := time.Now().Add(5 * time.Second)
	for c.conns.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if c.conns.Load() != 1 {
		t.Fatalf("watcher connections = %d, want 1", c.conns.Load())
	}
}
