package terminal

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type reaperCaptureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *reaperCaptureLogger) Printf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *reaperCaptureLogger) Debugf(string, ...interface{}) {}

func (l *reaperCaptureLogger) contains(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func TestReapExpiredTmuxSessionsUsesCurrentTimeAndLogsLease(t *testing.T) {
	server := NewServer(&Config{SessionTTL: 48 * time.Hour}, NewMockTokenValidator())
	now := time.Unix(2_000_000_000, 0)
	server.now = func() time.Time { return now }
	logger := &reaperCaptureLogger{}
	server.logger = logger

	var gotNow time.Time
	server.reapTmuxSessions = func(ctx context.Context, now time.Time) ([]string, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("reaper context has no deadline")
		}
		gotNow = now
		return []string{"one", "two"}, nil
	}

	server.reapExpiredTmuxSessions(context.Background())
	if !gotNow.Equal(now) {
		t.Fatalf("now = %v, want %v", gotNow, now)
	}
	if !logger.contains("reaped 2 detached tmux session(s) after their 48h0m0s retention lease expired") {
		t.Fatalf("reap log missing: %v", logger.lines)
	}
}

func TestReapExpiredTmuxSessionsDisabledDoesNothing(t *testing.T) {
	server := NewServer(&Config{SessionTTL: 0}, NewMockTokenValidator())
	server.reapTmuxSessions = func(context.Context, time.Time) ([]string, error) {
		t.Fatal("disabled reaper was called")
		return nil, nil
	}
	server.reapExpiredTmuxSessions(context.Background())
}

func TestReapExpiredTmuxSessionsLogsFailure(t *testing.T) {
	server := NewServer(&Config{SessionTTL: time.Hour}, NewMockTokenValidator())
	logger := &reaperCaptureLogger{}
	server.logger = logger
	server.reapTmuxSessions = func(context.Context, time.Time) ([]string, error) {
		return nil, errors.New("tmux wedged")
	}
	server.reapExpiredTmuxSessions(context.Background())
	if !logger.contains("persistent tmux session reaper failed: tmux wedged") {
		t.Fatalf("failure log missing: %v", logger.lines)
	}
}

func validLifecycleConfig() *Config {
	config := DefaultConfig()
	config.Host = "127.0.0.1"
	config.Port = 7860
	config.OrgID = "test-org"
	config.SessionTTL = time.Minute
	return config
}

func TestStartRejectsTooShortSessionTTLBeforeListen(t *testing.T) {
	config := validLifecycleConfig()
	config.SessionTTL = time.Second
	server := NewServer(config, NewMockTokenValidator())
	listenCalls := 0
	server.listen = func(string, string) (net.Listener, error) {
		listenCalls++
		return nil, errors.New("must not listen")
	}

	err := server.Start()
	if !errors.Is(err, ErrInvalidSessionTTL) {
		t.Fatalf("Start() error = %v, want ErrInvalidSessionTTL", err)
	}
	if listenCalls != 0 {
		t.Fatalf("listen called %d times for invalid config", listenCalls)
	}
}

func TestRepeatedFailedStartNeverStartsMaintenance(t *testing.T) {
	server := NewServer(validLifecycleConfig(), NewMockTokenValidator())
	server.listen = func(string, string) (net.Listener, error) {
		return nil, errors.New("bind failed")
	}
	reaps := 0
	server.reapTmuxSessions = func(context.Context, time.Time) ([]string, error) {
		reaps++
		return nil, nil
	}

	for i := 0; i < 3; i++ {
		if err := server.Start(); err == nil {
			t.Fatal("Start() unexpectedly succeeded")
		}
	}
	time.Sleep(20 * time.Millisecond)
	if reaps != 0 {
		t.Fatalf("failed starts leaked %d reaper call(s)", reaps)
	}
}

func TestStopCancelsAndJoinsInflightReaper(t *testing.T) {
	server := NewServer(validLifecycleConfig(), NewMockTokenValidator())
	server.listen = func(string, string) (net.Listener, error) {
		return net.Listen("tcp", "127.0.0.1:0")
	}
	entered := make(chan struct{})
	canceled := make(chan struct{})
	server.reapTmuxSessions = func(ctx context.Context, _ time.Time) ([]string, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	}
	if err := server.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("reaper did not start")
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("Stop returned without canceling in-flight reaper")
	}
}

func TestTmuxLeaseRenewsOnDetachWithExplicitDeadline(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	server := NewServer(&Config{SessionTTL: 2 * time.Hour}, NewMockTokenValidator())
	server.now = func() time.Time { return now }
	server.maintenanceCtx = context.Background()
	var names []string
	var deadlines []time.Time
	server.renewTmuxSession = func(_ context.Context, name string, deadline time.Time) error {
		names = append(names, name)
		deadlines = append(deadlines, deadline)
		return nil
	}

	stop := server.startTmuxLeaseRenewal("agent", true)
	stop()
	if want := []string{"agent"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("renewed names = %v, want %v", names, want)
	}
	if len(deadlines) != 1 || !deadlines[0].Equal(now.Add(2*time.Hour)) {
		t.Fatalf("deadlines = %v, want %v", deadlines, now.Add(2*time.Hour))
	}
}

func TestTmuxLeaseRenewalStopsWhenServerContextCanceled(t *testing.T) {
	server := NewServer(&Config{SessionTTL: time.Minute}, NewMockTokenValidator())
	ctx, cancel := context.WithCancel(context.Background())
	server.maintenanceCtx = ctx
	server.renewTmuxSession = func(context.Context, string, time.Time) error {
		t.Fatal("renewal ran after server cancellation")
		return nil
	}

	stop := server.startTmuxLeaseRenewal("agent", true)
	cancel()
	stop()
}
