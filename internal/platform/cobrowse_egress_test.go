package platform

import (
	"context"
	"errors"
	"github.com/aceteam-ai/citadel-cli/internal/browseregress"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSessionEgress struct {
	live   atomic.Bool
	closed atomic.Int32
	checks atomic.Int32
	refuse error
}

func newFakeSessionEgress() *fakeSessionEgress {
	g := &fakeSessionEgress{}
	g.live.Store(true)
	return g
}
func (*fakeSessionEgress) ProxyURL() string { return "http://127.0.0.1:12345" }
func (g *fakeSessionEgress) Active() bool   { return g.live.Load() }
func (g *fakeSessionEgress) Close() {
	if g.live.Swap(false) {
		g.closed.Add(1)
	}
}
func (g *fakeSessionEgress) CheckURL(context.Context, string) error { g.checks.Add(1); return g.refuse }
func (*fakeSessionEgress) PrivateRefused() int64                    { return 0 }

func TestNodeWideManagerSessionRequiresEgress(t *testing.T) {
	m := GetCobrowseSessionManager()
	if got := m.EgressFeatures(); len(got) != 1 || got[0] != "egress_guard" {
		t.Fatalf("production manager is unguarded: %v", got)
	}
}

func TestTargetSetRemoteEgressLaunchAndCleanup(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "throwaway", true: "persistent"}[persistent], func(t *testing.T) {
			f := newFakeDevTools(t)
			f.set([2]string{"a", "https://public.test/"})
			g := newFakeSessionEgress()
			m := newCobrowseSessionManager(trustedBaseDir(t), 2)
			m.requireEgress = true
			m.egressFactory = func() (sessionEgress, error) { return g, nil }
			m.SetLaunchOptions(LaunchOptions{ProxyServer: "http://unguarded-proxy.test:1"})
			previous := launchCobrowseProc
			t.Cleanup(func() { launchCobrowseProc = previous })
			launchCobrowseProc = func(_, _ string, lo LaunchOptions) (*cobrowseProc, error) {
				if lo.ProxyServer != g.ProxyURL() {
					t.Fatalf("guard bypass: %s", lo.ProxyServer)
				}
				if g.checks.Load() == 0 {
					t.Fatal("launch before destination check")
				}
				return &cobrowseProc{debugPort: f.port, exited: make(chan struct{}), stop: func() error {
					if !g.Active() {
						t.Error("proxy closed before browser stop")
					}
					return nil
				}}, nil
			}
			var profile SessionProfile
			if persistent {
				profile = &fakeProfile{}
			}
			st, err := m.StartSessionWithProfile("about:blank", profile)
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Features) != 1 || st.Features[0] != "egress_guard" {
				t.Fatalf("session capability: %+v", st)
			}
			if _, err = m.Navigate(st.ID, "https://public.test/"); err != nil {
				t.Fatal(err)
			}
			if g.checks.Load() < 2 {
				t.Fatal("navigate skipped guard check")
			}
			if _, err = m.Handoff(st.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = m.NavigateAsHuman(st.ID, "https://public.test/"); err != nil {
				t.Fatal(err)
			}
			if g.checks.Load() < 3 {
				t.Fatal("human navigate skipped guard check")
			}
			if err = m.Stop(st.ID); err != nil {
				t.Fatal(err)
			}
			if g.Active() || g.closed.Load() != 1 {
				t.Fatalf("guard leaked: %v %d", g.Active(), g.closed.Load())
			}
		})
	}
}

func TestTargetSetRemoteEgressFailsBeforeProfileOrLaunch(t *testing.T) {
	for _, mode := range []string{"factory", "inactive", "policy", "launcher"} {
		t.Run(mode, func(t *testing.T) {
			g := newFakeSessionEgress()
			profile := &fakeProfile{}
			m := newCobrowseSessionManager(trustedBaseDir(t), 2)
			m.requireEgress = true
			m.egressFactory = func() (sessionEgress, error) {
				if mode == "factory" {
					return nil, errors.New("fake proxy cannot bind")
				}
				return g, nil
			}
			if mode == "inactive" {
				g.Close()
			}
			if mode == "policy" {
				g.refuse = browseregress.ErrPrivateNetworkApprovalRequired
			}
			previous := launchCobrowseProc
			t.Cleanup(func() { launchCobrowseProc = previous })
			launches := 0
			launchCobrowseProc = func(_, _ string, _ LaunchOptions) (*cobrowseProc, error) {
				launches++
				return nil, errors.New("fake launch failed")
			}
			_, err := m.StartSessionWithProfile("https://public.test/", profile)
			if err == nil {
				t.Fatal("unsafe start succeeded")
			}
			mat, _, closed := profile.counts()
			if mode != "launcher" && (mat != 0 || launches != 0) {
				t.Fatal("guard refusal happened after profile/browser effects")
			}
			if closed == 0 {
				t.Fatal("profile handle leaked")
			}
			if mode != "factory" && g.Active() {
				t.Fatal("failed launch leaked guard")
			}
			if len(m.List()) != 0 {
				t.Fatal("failed session retained")
			}
		})
	}
}

func TestTargetSetRemoteEgressLossRefusesAllActions(t *testing.T) {
	f := newFakeDevTools(t)
	f.set([2]string{"a", "https://public.test/"})
	installFixedPortLauncher(t, f.port)
	g := newFakeSessionEgress()
	m := newCobrowseSessionManager(trustedBaseDir(t), 2)
	m.requireEgress = true
	m.egressFactory = func() (sessionEgress, error) { return g, nil }
	st, err := m.StartSession("about:blank")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(st.ID)
	g.Close()
	for _, action := range []func() error{
		func() error { _, e := m.Navigate(st.ID, "https://public.test/"); return e },
		func() error { _, e := m.Type(st.ID, "x"); return e },
		func() error { _, e := m.Screenshot(st.ID); return e },
		func() error { _, e := m.Extract(st.ID, "body", nil); return e },
		func() error { _, e := m.InspectTarget(st.ID, TargetProbe{Kind: "click", Selector: "button"}); return e },
		func() error { _, e := m.Handoff(st.ID); return e },
		func() error { _, e := m.Resume(st.ID); return e },
	} {
		if e := action(); !errors.Is(e, ErrEgressGuardUnavailable) {
			t.Fatalf("action after guard loss: %v", e)
		}
	}
	current, _ := m.SessionStatus(st.ID)
	if current.State != SessionExited || len(current.Features) != 0 {
		t.Fatalf("inactive guard still attachable: %+v", current)
	}
}

func TestTargetSetRemoteEgressStopDuringLaunch(t *testing.T) {
	g := newFakeSessionEgress()
	m := newCobrowseSessionManager(trustedBaseDir(t), 1)
	m.requireEgress = true
	entered, release := make(chan struct{}), make(chan struct{})
	m.egressFactory = func() (sessionEgress, error) { close(entered); <-release; return g, nil }
	f := &fakeLauncher{}
	f.install(t)
	done := make(chan error, 1)
	go func() { _, err := m.StartSession("about:blank"); done <- err }()
	<-entered
	if err := m.StopAll(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("cancelled launch succeeded")
	}
	if g.Active() {
		t.Fatal("late proxy publication leaked after stop")
	}
}

func TestTargetSetRemoteEgressRestartEndsSession(t *testing.T) {
	f := newFakeDevTools(t)
	f.set([2]string{"a", "https://public.test/"})
	installFixedPortLauncher(t, f.port)
	g := newFakeSessionEgress()
	m := newCobrowseSessionManager(trustedBaseDir(t), 1)
	m.requireEgress = true
	m.egressFactory = func() (sessionEgress, error) { return g, nil }
	st, err := m.StartSession("about:blank")
	if err != nil {
		t.Fatal(err)
	}
	m.prepareSelfRestart("node_update", "fake version")
	if _, ok := m.SessionStatus(st.ID); ok {
		t.Fatal("worker restart retained guarded browser without a proxy owner")
	}
	if g.Active() {
		t.Fatal("worker restart retained proxy")
	}
}

func TestTargetSetRemoteEgressRefusesLegacyAdoption(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-adoption-os", true: "adoption-os"}[supported], func(t *testing.T) {
			env := newAdoptEnv(t)
			adoptionSupported = supported
			base := trustedBaseDir(t)
			id := "cb-0011223344556677"
			dir := filepath.Join(base, id)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			m := adoptMgr(base)
			m.requireEgress = true
			rec := sessionRecord{Version: sessionRecordVersion, ID: id, Profile: dir, State: RecordLive, BrowserPID: 100001, BrowserStart: 11, XvfbPID: 200001, XvfbStart: 12, OwnerPID: os.Getpid(), OwnerStart: 7, StartedAt: time.Now().UTC().Format(time.RFC3339)}
			if err := saveRecord(m.registryDir(), rec); err != nil {
				t.Fatal(err)
			}
			m.Recover()
			if _, ok := m.SessionStatus(id); ok {
				t.Fatal("unguarded legacy browser was adopted")
			}
			ended, ok := m.EndedSession(id)
			if !ok || ended.Reason != endedReasonEgressRestart {
				t.Fatalf("missing restart explanation: %+v %v", ended, ok)
			}
			env.mu.Lock()
			kills := append([]int(nil), env.killed...)
			env.mu.Unlock()
			if len(kills) != 2 {
				t.Fatalf("verified legacy children not stopped: %v", kills)
			}
		})
	}
}

func TestTargetSetRemoteEgressNeverKillsUnverifiedProcess(t *testing.T) {
	for _, mode := range []string{"reused", "unknown", "different-role", "different-owner"} {
		t.Run(mode, func(t *testing.T) {
			env := newAdoptEnv(t)
			rec := sessionRecord{ID: "cb-0011223344556677", BrowserPID: 100001, BrowserStart: 11}
			switch mode {
			case "reused":
				env.procs[100001] = fakeProc{99, true}
			case "unknown":
				delete(env.procs, 100001)
			case "different-role":
				procCmdlineReader = func(int) (string, error) { return "/usr/bin/ssh", nil }
			case "different-owner":
				uid := os.Getuid() + 1
				rec.OwnerUID = &uid
			}
			stopUnguardedRecord(rec)
			if len(env.killed) != 0 {
				t.Fatalf("signalled unverified process: %v", env.killed)
			}
		})
	}
}

func TestTargetSetRemoteEgressLeavesLiveSiblingRecord(t *testing.T) {
	env := newAdoptEnv(t)
	env.procs[4242] = fakeProc{55, true}
	base := trustedBaseDir(t)
	id := "cb-0011223344556677"
	dir := filepath.Join(base, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	m := adoptMgr(base)
	m.requireEgress = true
	rec := sessionRecord{Version: sessionRecordVersion, ID: id, Profile: dir, State: RecordLive, OwnerPID: 4242, OwnerStart: 55, BrowserPID: 100001, BrowserStart: 11}
	if err := saveRecord(m.registryDir(), rec); err != nil {
		t.Fatal(err)
	}
	m.Recover()
	current, err := loadRecord(m.registryDir(), id)
	if err != nil || current.State != RecordLive {
		t.Fatalf("changed sibling record: %+v %v", current, err)
	}
	if len(env.killed) != 0 {
		t.Fatalf("stopped live sibling: %v", env.killed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("removed live sibling profile: %v", err)
	}
}

func TestTargetSetRemoteEgressLossDuringPreparation(t *testing.T) {
	f := newFakeDevTools(t)
	f.set([2]string{"a", "https://public.test/"})
	installFixedPortLauncher(t, f.port)
	g := newFakeSessionEgress()
	m := newCobrowseSessionManager(trustedBaseDir(t), 1)
	m.requireEgress = true
	m.egressFactory = func() (sessionEgress, error) { return g, nil }
	st, err := m.StartSession("about:blank")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(st.ID)
	f.mu.Lock()
	f.onCall = func(_, method string) {
		if method == "Runtime.evaluate" {
			g.Close()
		}
	}
	f.mu.Unlock()
	_, err = m.Click(st.ID, "button", nil, nil)
	if !errors.Is(err, ErrEgressGuardUnavailable) {
		t.Fatalf("input after guard loss: %v", err)
	}
	for _, call := range f.callsOn("a") {
		if strings.HasPrefix(call, "Input.") {
			t.Fatalf("input reached browser after proxy failure: %s", call)
		}
	}
}

func TestTargetSetRemoteEgressSweepCannotKillReusedBrowserPID(t *testing.T) {
	env := newAdoptEnv(t)
	env.procs[100001] = fakeProc{99, true}
	base := trustedBaseDir(t)
	id := "cb-0011223344556677"
	dir := filepath.Join(base, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeSessionPidfile(dir, os.Getpid(), 100001, 0, currentProcessStartToken())
	m := adoptMgr(base)
	m.requireEgress = true
	rec := sessionRecord{Version: sessionRecordVersion, ID: id, Profile: dir, State: RecordLive, OwnerPID: os.Getpid(), OwnerStart: 7, BrowserPID: 100001, BrowserStart: 11}
	if err := saveRecord(m.registryDir(), rec); err != nil {
		t.Fatal(err)
	}
	m.Recover()
	if len(env.killed) != 0 {
		t.Fatalf("fallback sweep signalled a reused browser PID: %v", env.killed)
	}
}
