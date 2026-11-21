package platform

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSuspendOwnerBoundBeforeLaunchAndPersisted(t *testing.T) {
	newAdoptEnv(t)
	f := &fakeLauncher{}
	f.install(t)
	m := adoptMgr(trustedBaseDir(t))
	baseLaunch := launchCobrowseProc
	launchCobrowseProc = func(dir, url string, opts LaunchOptions) (*cobrowseProc, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, s := range m.sessions {
			if s.ownerUserID != "account-a" || s.profileName != "" {
				t.Fatal("session was visible before authenticated ownership was bound")
			}
		}
		proc, err := baseLaunch(dir, url, opts)
		if proc != nil {
			proc.browserID = "fake-browser"
		}
		return proc, err
	}
	st, err := m.StartSessionForOwner("about:blank", "account-a", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.StopAll() })
	r, err := loadRecord(m.registryDir(), st.ID)
	if err != nil || r.OwnerUserID != "account-a" || r.ProfileName != "" {
		t.Fatalf("start owner was not durably bound: %+v %v", r, err)
	}
	data, err := json.Marshal(st)
	if err != nil || strings.Contains(string(data), "account-a") || strings.Contains(string(data), "owner_user_id") {
		t.Fatal("account owner leaked into browser job status")
	}
}

func TestSuspendOwnerPersistentProfileBinding(t *testing.T) {
	newAdoptEnv(t)
	(&fakeLauncher{}).install(t)
	m := adoptMgr(trustedBaseDir(t))
	p := &fakeProfile{}
	st, err := m.StartSessionForOwner("about:blank", "account-a", "profile-a", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.StopAll() })
	r, err := loadRecord(m.registryDir(), st.ID)
	if err != nil || r.OwnerUserID != "account-a" || r.ProfileName != "profile-a" || !r.Persistent {
		t.Fatalf("persistent identity was not retained: %+v %v", r, err)
	}
}

func TestSuspendOwnerInvalidBindingClosesProfileBeforeLaunch(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 4)
	for _, tc := range []struct {
		owner, name string
		profile     bool
	}{
		{"", "profile-a", true},
		{"with space", "profile-a", true},
		{strings.Repeat("a", 129), "profile-a", true},
		{"account-a", "", true},
		{"account-a", "../profile", true},
		{"account-a", "profile-a", false},
	} {
		p := &fakeProfile{}
		var profile SessionProfile
		if tc.profile {
			profile = p
		}
		if _, err := m.StartSessionForOwner("about:blank", tc.owner, tc.name, profile); err == nil {
			t.Fatal("invalid owner/profile binding accepted")
		}
		if tc.profile {
			mat, per, closed := p.counts()
			if mat != 0 || per != 0 || closed != 1 {
				t.Fatal("invalid binding touched or leaked a profile")
			}
		}
	}
	if f.n != 0 || len(m.sessions) != 0 {
		t.Fatal("invalid binding launched a browser or consumed a slot")
	}
}

func TestSuspendOwnerLegacyStartRemainsUnbound(t *testing.T) {
	newAdoptEnv(t)
	(&fakeLauncher{}).install(t)
	m := adoptMgr(trustedBaseDir(t))
	st, err := m.StartSession("about:blank")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.StopAll() })
	r, err := loadRecord(m.registryDir(), st.ID)
	if err != nil || r.OwnerUserID != "" {
		t.Fatal("legacy start acquired an account identity")
	}
	if _, err := planSuspendedRecord(r, "account-a", "", 1, 100, nil, suspendTestNow); err == nil {
		t.Fatal("a later request claimed an unknown-owner legacy session")
	}
}

func TestSuspendOwnerAdoptionRetainsAccount(t *testing.T) {
	env := newAdoptEnv(t)
	(&fakeLauncher{}).install(t)
	base := trustedBaseDir(t)
	m := adoptMgr(base)
	st, err := m.StartSessionForOwner("about:blank", "account-a", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	m2 := adoptMgr(base)
	m2.Recover()
	m2.mu.Lock()
	adopted := m2.sessions[st.ID]
	m2.mu.Unlock()
	if adopted == nil || adopted.proc == nil {
		t.Fatal("session was not re-adopted")
	}
	exited := adopted.proc.exited
	t.Cleanup(func() {
		// The adopted process observer owns a background poll. Finish it while
		// this test's fake process table is still installed, before a sibling
		// test replaces the package-level procIdentity seam.
		env.mu.Lock()
		p := env.procs[100001]
		p.alive = false
		env.procs[100001] = p
		env.mu.Unlock()
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			t.Error("adopted process observer outlived its fake process table")
		}
		_ = m2.StopAll()
		_ = m.StopAll()
	})
	r, err := loadRecord(m2.registryDir(), st.ID)
	if err != nil || r.OwnerUserID != "account-a" || r.Generation != st.Generation+1 {
		t.Fatalf("re-adoption lost or replaced account identity: %+v %v", r, err)
	}
}
