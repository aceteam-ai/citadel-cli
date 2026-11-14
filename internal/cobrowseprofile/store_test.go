package cobrowseprofile

import (
	"errors"
	"os"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
)

// TestStoreLifecycle walks local mode's store: unset, a refused weak
// passphrase, setup, a profile round trip, listing, and Forget.
func TestStoreLifecycle(t *testing.T) {
	s := NewStore(t.TempDir())
	st, err := s.State()
	if err != nil || st.Configured {
		t.Fatalf("fresh store: %+v %v", st, err)
	}
	if err := s.SetPassphrase("654321", true); !errors.Is(err, nodevault.ErrWeakSecret) {
		t.Fatalf("weak passphrase: want ErrWeakSecret, got %v", err)
	}
	if err := s.SetPassphrase(testPIN, true); err != nil {
		t.Fatalf("SetPassphrase: %v", err)
	}
	st, err = s.State()
	if err != nil || !st.Configured || !st.Strong {
		t.Fatalf("after setup: %+v %v", st, err)
	}

	h, err := s.Open("default", testPIN)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	work := t.TempDir()
	writeCookie(t, work, "Default/Cookies", "synthetic-cookie")
	if err := h.Persist(work); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := s.Forget(); !errors.Is(err, ErrProfilesInUse) {
		t.Fatalf("Forget while open: want ErrProfilesInUse, got %v", err)
	}
	_ = h.Close()

	names, err := s.Profiles()
	if err != nil || len(names) != 1 || names[0] != "default" {
		t.Fatalf("Profiles = %v %v", names, err)
	}
	if err := s.Forget(); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if names, _ := s.Profiles(); len(names) != 0 {
		t.Fatalf("profiles left after Forget: %v", names)
	}
	if st, _ := s.State(); st.Configured {
		t.Fatal("vault still configured after Forget")
	}
	if _, err := os.Stat(storePath(s.Dir(), "default")); !os.IsNotExist(err) {
		t.Fatalf("sealed blob left after Forget: %v", err)
	}
}
