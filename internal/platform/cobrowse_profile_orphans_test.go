package platform

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// identityProfile is a fakeProfile that names its sealed blob.
type identityProfile struct {
	fakeProfile
	id string
}

func (p *identityProfile) ProfileIdentity() string { return p.id }

// deadPID is far above any real pid_max, so no live process has it.
const deadPID = 2147483000

func plantOrphan(t *testing.T, base, name, identity string, owner int, start int64) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Default", "Cookies"), []byte("synthetic-plaintext-cookie"), 0o600); err != nil {
		t.Fatal(err)
	}
	if identity != "" {
		data, _ := json.Marshal(profileMarker{Identity: identity, OwnerPID: owner, OwnerStart: start})
		if err := os.WriteFile(filepath.Join(dir, profileMarkerName), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestCleanOrphanedProfileSessions: an interrupted session's decrypted copy of
// an owned profile is removed; one whose owner still runs is reported and
// kept; dirs of other profiles or without a marker are never touched.
func TestCleanOrphanedProfileSessions(t *testing.T) {
	base := trustedBaseDir(t)
	const mine, other = "/store/cobrowse-profiles/default.enc", "/node/cobrowse-profiles/work.enc"
	owns := func(id string) bool { return id == mine }

	orphan := plantOrphan(t, base, "cb-0000000000000001", mine, deadPID, 0)
	foreign := plantOrphan(t, base, "cb-0000000000000002", other, deadPID, 0)
	throwaway := plantOrphan(t, base, "cb-0000000000000003", "", deadPID, 0)
	if err := CleanOrphanedProfileSessions(base, owns); err != nil {
		t.Fatalf("clean: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("the interrupted session's plaintext profile was left behind: %v", err)
	}
	for _, d := range []string{foreign, throwaway} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("%s must not be touched: %v", d, err)
		}
	}

	live := plantOrphan(t, base, "cb-0000000000000004", mine, os.Getpid(), currentProcessStartToken())
	err := CleanOrphanedProfileSessions(base, owns)
	if !errors.Is(err, ErrProfilePlaintextRemains) || !strings.Contains(err.Error(), live) {
		t.Fatalf("a live owner's dir must be reported, got %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("a live owner's dir must be kept: %v", err)
	}
	if err := CleanOrphanedProfileSessions(filepath.Join(base, "absent"), owns); err != nil {
		t.Fatalf("a missing base dir means nothing to clean: %v", err)
	}
}

// The marker is written before any plaintext, names the profile and this
// process, and goes away with the working dir on a normal stop.
func TestPersistentSessionWritesProfileMarker(t *testing.T) {
	l := &sentinelCheckingLauncher{}
	l.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 2)
	p := &identityProfile{id: "/store/cobrowse-profiles/default.enc"}
	st, err := m.StartSessionWithProfile("", p)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(st.Profile, profileMarkerName))
	if err != nil {
		t.Fatalf("no profile marker: %v", err)
	}
	var mk profileMarker
	if json.Unmarshal(data, &mk) != nil || mk.Identity != p.id || mk.OwnerPID != os.Getpid() {
		t.Fatalf("marker = %s", data)
	}
	if err := m.Stop(st.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.Profile); !os.IsNotExist(err) {
		t.Fatalf("working dir left behind: %v", err)
	}
}
