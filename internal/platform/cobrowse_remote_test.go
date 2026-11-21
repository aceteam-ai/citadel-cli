package platform

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestTargetSetDispatchedAssertionsAreScopedAndSerialized(t *testing.T) {
	fixture := NewDispatchFixture(t, true)
	m := fixture.Manager
	st, err := m.StartSession("")
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.sessionForAction(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("fake action failed")
	if err = m.WithTargetAssertion(st.ID, "a", func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.expectTarget != "a" {
			t.Error("assertion not installed")
		}
		return want
	}); !errors.Is(err, want) {
		t.Fatal(err)
	}
	s.mu.Lock()
	if s.expectTarget != "" {
		t.Error("failed action leaked assertion")
	}
	s.mu.Unlock()
	var executing atomic.Int32
	first := make(chan struct{})
	release := make(chan struct{})
	second := make(chan struct{})
	done := make(chan error, 2)
	go func() {
		done <- m.WithTargetAssertion(st.ID, "a", func() error { executing.Add(1); defer executing.Add(-1); close(first); <-release; return nil })
	}()
	<-first
	go func() {
		done <- m.WithTargetAssertion(st.ID, "", func() error {
			close(second)
			if executing.Load() != 0 {
				return errors.New("overlapping dispatched actions")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.expectTarget != "" {
				return errors.New("background read inherited write assertion")
			}
			return nil
		})
	}()
	select {
	case <-second:
		t.Error("second action bypassed serialization")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

func TestNodeWideManagerSessionProductionTabsPolicy(t *testing.T) {
	m := GetCobrowseSessionManager()
	m.mu.Lock()
	enabled, policy := m.trackTargets, m.targetPolicy
	m.mu.Unlock()
	if !enabled || policy == nil {
		t.Fatal("new remote sessions do not enable the page policy")
	}
	for _, raw := range []string{"http://127.0.0.1/", "http://169.254.169.254/latest/meta-data/", "http://[::1]/", "file:///etc/passwd"} {
		if err := policy(context.Background(), raw); err == nil {
			t.Fatalf("production remote policy allowed %s", raw)
		}
	}
	for _, raw := range []string{"about:blank", "https://8.8.8.8/"} {
		if err := policy(context.Background(), raw); err != nil {
			t.Fatalf("public or blank page refused: %s %v", raw, err)
		}
	}
}

func TestTargetSetDescriptorContainsEmptyAndFalseFields(t *testing.T) {
	raw, err := json.Marshal(CobrowseTarget{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "title", "url", "active", "refused"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("descriptor missing %s: %s", key, raw)
		}
	}
}

func TestTargetSetMissingSessionNeverRunsAssertionCallback(t *testing.T) {
	m := NewCobrowseSessionManager(t.TempDir(), 1)
	ran := false
	err := m.WithTargetAssertion("missing", "a", func() error { ran = true; return nil })
	if err == nil || ran {
		t.Fatalf("missing session ran=%v err=%v", ran, err)
	}
}
