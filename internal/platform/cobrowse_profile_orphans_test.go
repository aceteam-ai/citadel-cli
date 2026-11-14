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

// A present marker that is damaged (invalid JSON, empty identity, or not a
// regular file) is reported and its dir kept: it may hold decrypted cookies of
// one of the user's profiles. A dir with no marker is still skipped.
func TestCleanOrphanedProfileSessionsDamagedMarker(t *testing.T) {
	owns := func(string) bool { return true }
	for name, damage := range map[string]func(t *testing.T, marker string){
		"invalid json": func(t *testing.T, m string) {
			if err := os.WriteFile(m, []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"empty identity": func(t *testing.T, m string) {
			if err := os.WriteFile(m, []byte(`{"identity":"","owner_pid":2147483000}`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"not a file": func(t *testing.T, m string) {
			if err := os.Remove(m); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(m, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			base := trustedBaseDir(t)
			dir := plantOrphan(t, base, "cb-0000000000000007", "/store/cobrowse-profiles/default.enc", deadPID, 0)
			damage(t, filepath.Join(dir, profileMarkerName))
			unmarked := plantOrphan(t, base, "cb-0000000000000008", "", deadPID, 0)
			err := CleanOrphanedProfileSessions(base, owns)
			if !errors.Is(err, ErrProfilePlaintextRemains) || !strings.Contains(err.Error(), dir) {
				t.Fatalf("a damaged marker must be reported, got %v", err)
			}
			if strings.Contains(err.Error(), unmarked) {
				t.Fatalf("an unmarked dir must not be reported: %v", err)
			}
			for _, d := range []string{dir, unmarked} {
				if _, err := os.Stat(d); err != nil {
					t.Fatalf("%s must be kept: %v", d, err)
				}
			}
		})
	}
}
