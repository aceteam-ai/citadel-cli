package platform

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cdpdiscovery"
)

func suspendPagesFixture(t *testing.T, n int) (*cobrowseSession, []cdpdiscovery.Target, suspendPageSource, *[]string) {
	t.Helper()
	s := &cobrowseSession{id: "cb-123", ownerUserID: "account-a", regDir: t.TempDir(), generation: 3,
		state: SessionRunning, proc: &cobrowseProc{debugPort: 9876, browserID: "browser-a", exited: make(chan struct{})}}
	var targets []cdpdiscovery.Target
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("page-%d", i)
		targets = append(targets, cdpdiscovery.Target{ID: id, Type: "page", URL: fmt.Sprintf("https://example.invalid/%d?private-sentinel", i),
			WebSocketDebuggerURL: "ws://127.0.0.1:9876/devtools/page/" + id})
	}
	var calls []string
	source := suspendPageSource{
		verify: func(ctx context.Context, port int, id string) error {
			if _, ok := ctx.Deadline(); !ok || port != 9876 || id != "browser-a" {
				t.Error("capture lost browser identity or deadline")
			}
			calls = append(calls, "verify")
			return nil
		},
		list: func(context.Context, int) ([]cdpdiscovery.Target, error) {
			calls = append(calls, "list")
			return append([]cdpdiscovery.Target(nil), targets...), nil
		},
		read: func(ctx context.Context, p pin, fence func() error) (suspendTab, error) {
			calls = append(calls, p.id)
			for i, target := range targets {
				if p.id == target.ID {
					if p.ws != target.WebSocketDebuggerURL || p.session != s || !p.background {
						t.Error("read was not pinned to its exact page")
					}
					c := suspendCaptureFixture()
					obj := c.result["result"].(map[string]any)["value"].(map[string]any)
					obj["url"], obj["y"] = target.URL, float64(i+10)
					return readSuspendTabOn(c, fence)
				}
			}
			return suspendTab{}, errors.New("private-sentinel")
		},
	}
	return s, targets, source, &calls
}

func suspendPagesContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func TestSuspendPagesCompletePinnedCapture(t *testing.T) {
	s, targets, source, calls := suspendPagesFixture(t, 3)
	s.tracking = true
	set := targetSetFor(9876)
	set.active = targets[1].ID
	t.Cleanup(func() { dropTargetSet(9876) })
	m, err := s.captureSuspendPagesWith(suspendPagesContext(t), "account-a", false, source)
	if err != nil || len(m.Tabs) != 3 || m.Generation != 3 || m.SessionID != s.id {
		t.Fatal("complete capture failed")
	}
	for i, tab := range m.Tabs {
		if tab.URL != targets[i].URL || tab.ScrollY != float64(i+10) || tab.Active != (i == 1) {
			t.Fatal("capture lost a tab, its scroll or the active page")
		}
	}
	if !reflect.DeepEqual(*calls, []string{"verify", "list", "verify", "page-0", "verify", "page-1", "verify", "page-2", "list", "verify"}) {
		t.Fatal("capture omitted discovery or browser identity fences")
	}
	if !s.actionMu.TryLock() {
		t.Fatal("capture leaked its action lock")
	}
	s.actionMu.Unlock()
}

func TestSuspendPagesAdmissionBeforeIO(t *testing.T) {
	for _, kind := range []string{"owner", "legacy", "local", "busy", "state", "handoff", "locked", "generation", "identity", "deadline", "expired"} {
		t.Run(kind, func(t *testing.T) {
			s, _, source, calls := suspendPagesFixture(t, 1)
			ctx := suspendPagesContext(t)
			owner := "account-a"
			switch kind {
			case "owner":
				owner = "foreign-account"
			case "legacy":
				s.ownerUserID = ""
			case "local":
				s.regDir = ""
			case "busy":
				s.actionMu.Lock()
				defer s.actionMu.Unlock()
			case "state":
				s.state = SessionLaunching
			case "handoff":
				s.explicitHandoff = true
			case "locked":
				s.persistent = true
			case "generation":
				s.generation = 0
			case "identity":
				s.proc.browserID = ""
			case "deadline":
				ctx = context.Background()
			case "expired":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			m, err := s.captureSuspendPagesWith(ctx, owner, false, source)
			if err == nil || len(m.Tabs) != 0 || len(*calls) != 0 {
				t.Fatal("invalid admission performed browser IO")
			}
		})
	}
}

func TestSuspendPagesFencesEachProviderAwait(t *testing.T) {
	for _, boundary := range []string{"verify", "list", "read", "policy"} {
		for _, change := range []string{"owner", "generation", "handoff", "process", "exited", "deadline"} {
			t.Run(boundary+"/"+change, func(t *testing.T) {
				s, _, source, _ := suspendPagesFixture(t, 2)
				ctx, cancel := context.WithCancel(suspendPagesContext(t))
				defer cancel()
				changeState := func() {
					s.mu.Lock()
					defer s.mu.Unlock()
					switch change {
					case "owner":
						s.ownerUserID = "foreign-account"
					case "generation":
						s.generation++
					case "handoff":
						s.explicitHandoff = true
					case "process":
						s.proc = nil
					case "exited":
						dead := make(chan struct{})
						close(dead)
						s.proc.exited = dead
					case "deadline":
						cancel()
					}
				}
				if boundary == "verify" {
					old := source.verify
					source.verify = func(c context.Context, p int, id string) error { err := old(c, p, id); changeState(); return err }
				}
				if boundary == "list" {
					old := source.list
					source.list = func(c context.Context, p int) ([]cdpdiscovery.Target, error) {
						out, err := old(c, p)
						changeState()
						return out, err
					}
				}
				if boundary == "read" {
					old := source.read
					source.read = func(c context.Context, p pin, f func() error) (suspendTab, error) {
						out, err := old(c, p, f)
						changeState()
						return out, err
					}
				}
				if boundary == "policy" {
					s.policy = func(context.Context, string) error { changeState(); return nil }
				}
				m, err := s.captureSuspendPagesWith(ctx, "account-a", false, source)
				if err == nil || len(m.Tabs) != 0 || strings.Contains(err.Error(), "private-sentinel") {
					t.Fatal("capture returned stale or private partial data")
				}
			})
		}
	}
}

func TestSuspendPagesBoundsAndChangedPopulation(t *testing.T) {
	for _, n := range []int{0, 1, 64, 65} {
		s, _, source, _ := suspendPagesFixture(t, n)
		m, err := s.captureSuspendPagesWith(suspendPagesContext(t), "account-a", false, source)
		if n == 0 || n > 64 {
			if err == nil || len(m.Tabs) != 0 {
				t.Fatal("page count bound did not fail closed")
			}
		} else if err != nil || len(m.Tabs) != n {
			t.Fatal("valid complete page population refused")
		}
	}
	for _, change := range []string{"opened", "closed", "duplicate", "navigated", "endpoint", "unknown"} {
		t.Run(change, func(t *testing.T) {
			s, _, source, _ := suspendPagesFixture(t, 2)
			list := source.list
			calls := 0
			source.list = func(ctx context.Context, port int) ([]cdpdiscovery.Target, error) {
				out, err := list(ctx, port)
				calls++
				if calls == 2 {
					switch change {
					case "opened":
						out = append(out, out[0])
					case "closed":
						out = out[:1]
					case "duplicate":
						out[1] = out[0]
					case "navigated":
						out[0].URL = "https://changed.invalid/private-sentinel"
					case "endpoint":
						out[0].WebSocketDebuggerURL = "ws://foreign.invalid/private-sentinel"
					case "unknown":
						out[0] = cdpdiscovery.Target{ID: "unknown", Type: "page"}
					}
				}
				return out, err
			}
			m, err := s.captureSuspendPagesWith(suspendPagesContext(t), "account-a", false, source)
			if err == nil || len(m.Tabs) != 0 || strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("changed population returned partial capture")
			}
		})
	}
}
