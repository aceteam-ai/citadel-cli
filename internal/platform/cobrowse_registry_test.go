package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeProc struct {
	start uint64
	alive bool
}

// adoptEnv installs a fake process table, cmdline reader, CDP check and killer,
// and forces adoption on. Mutates package vars: not parallel-safe.
type adoptEnv struct {
	mu     sync.Mutex
	procs  map[int]fakeProc
	cdpErr error
	killed []int
}

func newAdoptEnv(t *testing.T) *adoptEnv {
	t.Helper()
	e := &adoptEnv{procs: map[int]fakeProc{os.Getpid(): {start: 7, alive: true}}}
	e.procs[100001] = fakeProc{11, true}
	e.procs[200001] = fakeProc{12, true}
	pi, pc, pcdp, pk, pa := procIdentity, procCmdlineReader, adoptCDPCheck, pidKiller, adoptionSupported
	procIdentity = func(pid int) (uint64, bool, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		p, ok := e.procs[pid]
		if !ok {
			return 0, false, fmt.Errorf("no such process")
		}
		return p.start, p.alive, nil
	}
	procCmdlineReader = func(pid int) (string, error) {
		if pid == 200001 {
			return "Xvfb\x00:51", nil
		}
		return "/usr/bin/chromium\x00--x", nil
	}
	adoptCDPCheck = func(int, string) error { return e.cdpErr }
	pidKiller = func(pid int) error {
		e.mu.Lock()
		e.killed = append(e.killed, pid)
		e.mu.Unlock()
		return nil
	}
	adoptionSupported = true
	t.Cleanup(func() {
		procIdentity, procCmdlineReader, adoptCDPCheck, pidKiller, adoptionSupported = pi, pc, pcdp, pk, pa
		sweepProtected.Range(func(k, _ any) bool { sweepProtected.Delete(k); return true })
	})
	return e
}

func startOne(t *testing.T, base string) string {
	t.Helper()
	(&fakeLauncher{}).install(t)
	m := newCobrowseSessionManager(base, 4)
	st, err := m.StartSession("https://a.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(recordFile(m.registryDir(), st.ID)); err != nil {
		t.Fatalf("registry record not written: %v", err)
	}
	return st.ID
}

func TestRestartReadoptsSameSessionID(t *testing.T) {
	env := newAdoptEnv(t)
	base := trustedBaseDir(t)
	id := startOne(t, base)

	// "Restart": a brand new manager over the same base dir, no memory.
	m2 := newCobrowseSessionManager(base, 4)
	st, ok := m2.SessionStatus(id)
	if !ok {
		t.Fatal("session was not re-adopted")
	}
	if st.ID != id || st.State != SessionRunning || st.DebugPort != 9001 {
		t.Fatalf("unexpected status: %+v", st)
	}
	if !m2.MarkAttached(id) {
		t.Fatal("viewer could not re-attach")
	}
	if len(env.killed) != 0 {
		t.Fatalf("sweep killed an adopted session: %v", env.killed)
	}
	if _, err := os.Stat(filepath.Join(base, id)); err != nil {
		t.Fatalf("adopted session dir was removed: %v", err)
	}
	if got := m2.BusySessions(); len(got) != 1 || got[0].Why != "viewer attached" {
		t.Fatalf("busy = %+v", got)
	}
	// Re-owned: the record names this process.
	rec, err := loadRecord(m2.registryDir(), id)
	if err != nil || rec.OwnerPID != os.Getpid() || rec.OwnerStart != 7 || rec.State != RecordLive {
		t.Fatalf("record not re-owned: %+v %v", rec, err)
	}
	if rec.Generation != 2 || st.Generation != 2 {
		t.Fatalf("generation = %d / status %d, want 2", rec.Generation, st.Generation)
	}
}

func TestRestartDoesNotAdoptUnverifiable(t *testing.T) {
	cases := map[string]func(*adoptEnv){
		"dead browser":     func(e *adoptEnv) { e.procs[100001] = fakeProc{11, false} },
		"reused browser":   func(e *adoptEnv) { e.procs[100001] = fakeProc{99, true} },
		"reused xvfb":      func(e *adoptEnv) { e.procs[200001] = fakeProc{99, true} },
		"cdp mismatch":     func(e *adoptEnv) { e.cdpErr = errors.New("impostor") },
		"live other owner": nil,
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			env := newAdoptEnv(t)
			base := trustedBaseDir(t)
			id := startOne(t, base)
			if mut != nil {
				mut(env)
			} else {
				// Another live process owns the session.
				reg := newCobrowseSessionManager(base, 4).registryDir()
				rec, _ := loadRecord(reg, id)
				rec.OwnerPID = 4242
				rec.OwnerStart = 5
				env.procs[4242] = fakeProc{5, true}
				_ = saveRecord(reg, rec)
			}
			m2 := newCobrowseSessionManager(base, 4)
			if _, ok := m2.SessionStatus(id); ok {
				t.Fatal("adopted a session that failed verification")
			}
			_, err := os.Stat(filepath.Join(base, id))
			// (The sibling-owner case only asserts non-adoption: the real pidAlive
			// used by the sweep knows nothing of the fake process table.)
			if name != "live other owner" && err == nil {
				t.Fatal("unverifiable session was not swept")
			}
		})
	}
}

func TestUnadoptableLiveRecordBecomesExpiredWithReason(t *testing.T) {
	env := newAdoptEnv(t)
	base := trustedBaseDir(t)
	id := startOne(t, base)

	m1 := newCobrowseSessionManager(base, 4)
	m1.Recover() // pre-restart view, adopts the live record
	if err := m1.MarkLiveSessionsEnded(EndedReasonNodeUpdate, "to v9.9.9"); err != nil {
		t.Fatal(err)
	}
	// The restart killed the browser.
	env.procs[100001] = fakeProc{11, false}
	m2 := newCobrowseSessionManager(base, 4)
	if _, ok := m2.SessionStatus(id); ok {
		t.Fatal("dead session must not be adopted")
	}
	e, ok := m2.EndedSession(id)
	if !ok || e.Reason != "node_update" || e.Detail != "to v9.9.9" {
		t.Fatalf("ended = %+v %v", e, ok)
	}
	rec, err := loadRecord(m2.registryDir(), id)
	if err != nil || rec.State != RecordExpired {
		t.Fatalf("dead live record must be kept as expired: %+v %v", rec, err)
	}
	_, err = m2.Handoff(id)
	var se *SessionEndedError
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "ended_reason=node_update") {
		t.Fatalf("expected typed ended error, got %v", err)
	}
	if _, err := m2.Handoff("cb-0123"); err == nil || errors.As(err, &se) {
		t.Fatalf("unknown id must stay a plain error: %v", err)
	}
}

func TestDeadLiveRecordWithoutIntentIsExpiredAsGone(t *testing.T) {
	env := newAdoptEnv(t)
	base := trustedBaseDir(t)
	id := startOne(t, base)
	env.procs[100001] = fakeProc{11, false}
	m := newCobrowseSessionManager(base, 4)
	m.Recover()
	if e, ok := m.EndedSession(id); !ok || e.Reason != endedReasonLost {
		t.Fatalf("ended = %+v %v", e, ok)
	}
}

func TestSuspendedRecordSurvivesSweep(t *testing.T) {
	newAdoptEnv(t)
	base := trustedBaseDir(t)
	id := startOne(t, base)
	m := newCobrowseSessionManager(base, 4)
	reg := m.registryDir()
	rec, _ := loadRecord(reg, id)
	rec.State = RecordSuspended
	rec.OwnerPID = 999999 // owner long dead
	if err := saveRecord(reg, rec); err != nil {
		t.Fatal(err)
	}
	m.Recover()
	got, err := loadRecord(reg, id)
	if err != nil || got.State != RecordSuspended {
		t.Fatalf("suspended record must be kept untouched: %+v %v", got, err)
	}
	if _, ok := m.SessionStatus(id); ok {
		t.Fatal("suspended is not live; must not be adopted")
	}
}

func TestStopDeletesRecord(t *testing.T) {
	newAdoptEnv(t)
	base := trustedBaseDir(t)
	(&fakeLauncher{}).install(t)
	m := newCobrowseSessionManager(base, 4)
	st, _ := m.StartSession("https://a.example")
	if err := m.Stop(st.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(recordFile(m.registryDir(), st.ID)); err == nil {
		t.Fatal("record must go with a deliberate stop")
	}
	if _, ok := m.EndedSession(st.ID); ok {
		t.Fatal("a stop is not an unexpected end")
	}
}

func TestExpiredRetentionAndCap(t *testing.T) {
	newAdoptEnv(t)
	base := trustedBaseDir(t)
	m := newCobrowseSessionManager(base, 4)
	reg := m.registryDir()
	mk := func(i int, age time.Duration) {
		rec := sessionRecord{Version: 1, ID: fmt.Sprintf("cb-%04x", i), State: RecordExpired,
			EndedReason: "node_update", EndedAt: time.Now().Add(-age).UTC().Format(time.RFC3339)}
		if err := saveRecord(reg, rec); err != nil {
			t.Fatal(err)
		}
	}
	mk(0xaaaa, 25*time.Hour)
	for i := 0; i < expiredMaxRecords+5; i++ {
		mk(i, time.Duration(i)*time.Second)
	}
	m.Recover()
	n := 0
	for _, r := range listRecords(reg) {
		if r.State == RecordExpired {
			n++
		}
	}
	if n != expiredMaxRecords {
		t.Fatalf("expired kept = %d, want %d", n, expiredMaxRecords)
	}
	if _, err := os.Stat(recordFile(reg, "cb-aaaa")); err == nil {
		t.Fatal("old expired record kept past retention")
	}
	if fi, err := os.Stat(recordFile(reg, "cb-0000")); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("record not owner-only: %v %v", fi, err)
	}
}

func TestResolverChoosesRegistryDirOnlyForSingleton(t *testing.T) {
	prev := registryDirResolver
	defer func() { registryDirResolver = prev }()
	want := filepath.Join(t.TempDir(), "reg")
	SetRegistryDirResolver(func() string { return want })
	m := newCobrowseSessionManager(trustedBaseDir(t), 4)
	if m.registryDir() == want {
		t.Fatal("isolated manager must not use the node-wide resolver")
	}
	m.useResolver = true
	if m.registryDir() != want {
		t.Fatal("singleton-style manager must follow the resolver")
	}
}

func TestNoSessionsNoRecordsNoBusy(t *testing.T) {
	base := trustedBaseDir(t)
	m := newCobrowseSessionManager(base, 4)
	_ = m.MarkLiveSessionsEnded(EndedReasonNodeUpdate, "")
	if _, err := os.Stat(m.registryDir()); err == nil {
		t.Fatal("registry dir created with no live sessions")
	}
	if len(m.BusySessions()) != 0 {
		t.Fatal("no sessions cannot be busy")
	}
}

func TestBusyOnHandoff(t *testing.T) {
	newAdoptEnv(t)
	base := trustedBaseDir(t)
	id := startOne(t, base)
	m := newCobrowseSessionManager(base, 4)
	m.Recover()
	if len(m.BusySessions()) != 0 {
		t.Fatal("plain running session must not be busy")
	}
	if _, err := m.Handoff(id); err != nil {
		t.Fatal(err)
	}
	if b := m.BusySessions(); len(b) != 1 || b[0].Why != "handoff outstanding" {
		t.Fatalf("busy = %+v", b)
	}
	// Handoff survives a restart.
	m3 := newCobrowseSessionManager(base, 4)
	if b := m3.BusySessions(); len(b) != 0 {
		t.Fatal("not recovered until first use")
	}
	m3.Recover()
	if b := m3.BusySessions(); len(b) != 1 {
		t.Fatalf("handoff lost across restart: %+v", b)
	}
}

func TestParseProcStat(t *testing.T) {
	line := "123 (my (weird) comm) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 424242 23 24"
	st, alive, err := parseProcStat(line)
	if err != nil || st != 424242 || !alive {
		t.Fatalf("%d %v %v", st, alive, err)
	}
	if _, alive, _ := parseProcStat(strings.Replace(line, ") S ", ") Z ", 1)); alive {
		t.Fatal("zombie must not be alive")
	}
}
