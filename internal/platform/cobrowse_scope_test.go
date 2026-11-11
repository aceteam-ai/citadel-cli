package platform

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const (
	testBrowserUnit = "citadel-cobrowse-test-browser.scope"
	testXvfbUnit    = "citadel-cobrowse-test-xvfb.scope"
)

type scopeEnv struct {
	*adoptEnv
	mu      sync.Mutex
	cgroups map[int]string
	stops   []string
}

// newScopeEnv extends the fake process table with fake cgroups and a fake
// systemctl, and makes launches report scoped processes. It also marks the
// process as the managed worker; tests clear that for the refusal cases.
func newScopeEnv(t *testing.T) *scopeEnv {
	t.Helper()
	e := &scopeEnv{adoptEnv: newAdoptEnv(t), cgroups: map[int]string{
		100001: "0::/user.slice/user-1000.slice/user@1000.service/app.slice/" + testBrowserUnit + "\n",
		200001: "0::/user.slice/user-1000.slice/user@1000.service/app.slice/" + testXvfbUnit + "\n",
	}}
	pc, ps, pd := scopeProcCgroup, scopeStopper, detachedWorker.Load()
	scopeProcCgroup = func(pid int) (string, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if c, ok := e.cgroups[pid]; ok {
			return c, nil
		}
		return "", os.ErrNotExist
	}
	scopeStopper = func(user bool, unit string) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.stops = append(e.stops, unit)
		return nil
	}
	detachedWorker.Store(true)
	t.Cleanup(func() { scopeProcCgroup, scopeStopper = pc, ps; detachedWorker.Store(pd) })
	prev := launchCobrowseProc
	launchCobrowseProc = func(profileDir, _ string, _ LaunchOptions) (*cobrowseProc, error) {
		return &cobrowseProc{
			debugPort: 9001, display: ":51", browserPID: 100001, xvfbPID: 200001,
			browserScope: testBrowserUnit, xvfbScope: testXvfbUnit, scopeUser: true,
			exited: make(chan struct{}),
			stop:   func() error { return nil },
		}, nil
	}
	t.Cleanup(func() { launchCobrowseProc = prev })
	return e
}

func (e *scopeEnv) stopped() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.stops...)
}

// orphan simulates a full restart: the owner is gone.
func orphanRecord(t *testing.T, m *CobrowseSessionManager, id string, edit func(*sessionRecord)) {
	t.Helper()
	rec, err := loadRecord(m.registryDir(), id)
	if err != nil {
		t.Fatal(err)
	}
	rec.OwnerPID, rec.OwnerStart = 4343, 9
	if edit != nil {
		edit(&rec)
	}
	if err := saveRecord(m.registryDir(), rec); err != nil {
		t.Fatal(err)
	}
}

func startScoped(t *testing.T, base string) string {
	t.Helper()
	m := adoptMgr(base)
	st, err := m.StartSession("https://a.example")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := loadRecord(m.registryDir(), st.ID)
	if err != nil || rec.BrowserScope != testBrowserUnit || rec.XvfbScope != testXvfbUnit || !rec.ScopeUser || rec.OwnerUID == nil {
		t.Fatalf("scope not recorded: %+v %v", rec, err)
	}
	return st.ID
}

func setPlanSeams(t *testing.T, env map[string]string, cgroup string, euid int, runErr error) {
	t.Helper()
	a, b, c, d, e := scopeSupported, scopeGetenv, scopeLookupRun, scopeReadCgroup, scopeEUID
	scopeSupported = true
	scopeGetenv = func(k string) string { return env[k] }
	scopeLookupRun = func(string) (string, error) { return "/usr/bin/systemd-run", runErr }
	scopeReadCgroup = func() ([]byte, error) { return []byte(cgroup), nil }
	scopeEUID = func() int { return euid }
	t.Cleanup(func() { scopeSupported, scopeGetenv, scopeLookupRun, scopeReadCgroup, scopeEUID = a, b, c, d, e })
}

func TestScopeArgv(t *testing.T) {
	setPlanSeams(t, map[string]string{"INVOCATION_ID": "x"},
		"0::/user.slice/user-1000.slice/user@1000.service/app.slice/citadel.service\n", 1000, nil)
	p, reason := planCobrowseScope()
	if p == nil {
		t.Fatalf("expected a plan: %s", reason)
	}
	u := p.unit("cb-ab12", "browser")
	if !strings.HasPrefix(u, "citadel-cobrowse-cb-ab12-browser-") || !strings.HasSuffix(u, ".scope") {
		t.Fatalf("unit = %q", u)
	}
	if u == p.unit("cb-ab12", "browser") {
		t.Fatal("scope names must be unique per launch")
	}
	got := p.wrap(u, []string{"Xvfb", ":51"})
	want := []string{"/usr/bin/systemd-run", "--user", "--scope", "--quiet", "--collect", "--unit=" + u, "--", "Xvfb", ":51"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %v", got)
	}

	// Root system unit: the system manager, no --user.
	setPlanSeams(t, map[string]string{"INVOCATION_ID": "x"}, "0::/system.slice/citadel.service\n", 0, nil)
	p, _ = planCobrowseScope()
	if p == nil || p.user || p.wrap("u.scope", []string{"c"})[1] != "--scope" {
		t.Fatalf("system plan = %+v", p)
	}
}

func TestScopeFailsOpen(t *testing.T) {
	user := "0::/user.slice/user-1000.slice/user@1000.service/app.slice/citadel.service\n"
	cases := []struct {
		name string
		env  map[string]string
		cg   string
		euid int
		err  error
	}{
		{"not in a service", nil, user, 1000, nil},
		{"no systemd-run", map[string]string{"INVOCATION_ID": "x"}, user, 1000, os.ErrNotExist},
		{"non-root system unit", map[string]string{"INVOCATION_ID": "x"}, "0::/system.slice/citadel.service\n", 1000, nil},
	}
	for _, c := range cases {
		setPlanSeams(t, c.env, c.cg, c.euid, c.err)
		if p, reason := planCobrowseScope(); p != nil || reason == "" {
			t.Errorf("%s: plan=%v reason=%q, want a direct launch with a reason", c.name, p, reason)
		}
	}
	setPlanSeams(t, map[string]string{"INVOCATION_ID": "x"}, user, 1000, nil)
	scopeSupported = false
	if p, _ := planCobrowseScope(); p != nil {
		t.Error("unsupported platform must not scope")
	}
}

func TestReadoptAfterOwnerDeathInWorker(t *testing.T) {
	env := newScopeEnv(t)
	base := trustedBaseDir(t)
	id := startScoped(t, base)
	m := adoptMgr(base)
	orphanRecord(t, m, id, nil)

	st, ok := m.SessionStatus(id)
	if !ok || st.ID != id || st.State != SessionRunning {
		t.Fatalf("not re-adopted: %+v %v", st, ok)
	}
	if len(env.killed) != 0 || len(env.stopped()) != 0 {
		t.Fatalf("adoption must not stop anything: killed=%v stops=%v", env.killed, env.stopped())
	}
	rec, _ := loadRecord(m.registryDir(), id)
	if rec.OwnerPID != os.Getpid() || rec.Generation != 2 || rec.BrowserScope != testBrowserUnit {
		t.Fatalf("record not re-owned with its scopes: %+v", rec)
	}

	// Stop goes through the scope, identity-checked, not a bare PID kill first.
	if err := m.Stop(id); err != nil {
		t.Fatal(err)
	}
	if got := env.stopped(); !reflect.DeepEqual(got, []string{testBrowserUnit, testXvfbUnit}) {
		t.Fatalf("scope stops = %v", got)
	}
}

func TestDeadOwnerRefusedOutsideManagedWorker(t *testing.T) {
	env := newScopeEnv(t)
	base := trustedBaseDir(t)
	id := startScoped(t, base)
	m := adoptMgr(base)
	orphanRecord(t, m, id, nil)
	detachedWorker.Store(false) // a CLI, monitor or interactive run

	if _, ok := m.SessionStatus(id); ok {
		t.Fatal("a non-worker process adopted a dead owner's session")
	}
	rec, _ := loadRecord(m.registryDir(), id)
	if rec.State != RecordExpired {
		t.Fatalf("state = %q", rec.State)
	}
	if got := env.stopped(); !reflect.DeepEqual(got, []string{testBrowserUnit, testXvfbUnit}) {
		t.Fatalf("sweep must stop the scopes: %v", got)
	}
	if _, err := os.Stat(filepath.Join(base, id)); err == nil {
		t.Fatal("session dir must be swept")
	}
}

func TestDetachedAdoptionRefusals(t *testing.T) {
	other := 12345
	cases := []struct {
		name  string
		edit  func(*sessionRecord)
		world func(*scopeEnv)
		stops int // scopes the sweep may still stop (identity intact)
	}{
		{"different user", func(r *sessionRecord) { r.OwnerUID = &other }, nil, 2},
		{"unscoped record", func(r *sessionRecord) { r.BrowserScope, r.XvfbScope = "", "" }, nil, 0},
		{"browser left its scope", nil, func(e *scopeEnv) { e.cgroups[100001] = "0::/system.slice/other.service\n" }, 1},
		{"xvfb left its scope", nil, func(e *scopeEnv) { delete(e.cgroups, 200001) }, 1},
		{"browser pid reused", func(r *sessionRecord) { r.BrowserStart = 999 }, nil, 1},
		{"owner still running", func(r *sessionRecord) { r.OwnerPID, r.OwnerStart = 100001, 11 }, nil, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newScopeEnv(t)
			base := trustedBaseDir(t)
			id := startScoped(t, base)
			m := adoptMgr(base)
			orphanRecord(t, m, id, c.edit)
			if c.world != nil {
				c.world(env)
			}
			if _, ok := m.SessionStatus(id); ok {
				t.Fatal("adopted despite a failed check")
			}
			if c.stops >= 0 && len(env.stopped()) > c.stops {
				t.Fatalf("stopped scopes without identity: %v", env.stopped())
			}
		})
	}
}

func TestIdentityMismatchNeverStopsScope(t *testing.T) {
	env := newScopeEnv(t)
	base := trustedBaseDir(t)
	id := startScoped(t, base)
	m := adoptMgr(base)
	orphanRecord(t, m, id, func(r *sessionRecord) { r.BrowserStart, r.XvfbStart = 999, 998 })
	if _, ok := m.SessionStatus(id); ok {
		t.Fatal("adopted a process with a different start identity")
	}
	if got := env.stopped(); len(got) != 0 {
		t.Fatalf("a recycled PID's scope was stopped: %v", got)
	}
}

func TestShutdownDetachesScopedKeepsRecord(t *testing.T) {
	env := newScopeEnv(t)
	base := trustedBaseDir(t)
	m := adoptMgr(base)
	st, err := m.StartSession("https://a.example")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ShutdownSessions(); err != nil {
		t.Fatal(err)
	}
	if len(env.stopped()) != 0 || len(env.killed) != 0 {
		t.Fatal("a detached session must be left running on shutdown")
	}
	rec, err := loadRecord(m.registryDir(), st.ID)
	if err != nil || rec.State != RecordLive || rec.PendingEnd == nil || rec.PendingEnd.Reason != EndedReasonServiceRestart {
		t.Fatalf("record = %+v %v", rec, err)
	}

	// Outside the managed worker the same call stops everything.
	detachedWorker.Store(false)
	m2 := adoptMgr(trustedBaseDir(t))
	st2, _ := m2.StartSession("https://a.example")
	_ = m2.ShutdownSessions()
	if _, err := loadRecord(m2.registryDir(), st2.ID); err == nil {
		t.Fatal("interactive shutdown must stop sessions and drop their records")
	}
}
