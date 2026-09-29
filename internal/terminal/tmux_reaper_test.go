package terminal

import (
	"context"
	"errors"
	"fmt"
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

func TestReapExpiredTmuxSessionsUsesConfiguredCutoffAndLogsCount(t *testing.T) {
	server := NewServer(&Config{SessionTTL: 48 * time.Hour}, NewMockTokenValidator())
	now := time.Unix(2_000_000_000, 0)
	server.now = func() time.Time { return now }
	logger := &reaperCaptureLogger{}
	server.logger = logger

	var gotCutoff time.Time
	server.reapTmuxSessions = func(ctx context.Context, cutoff time.Time) ([]string, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("reaper context has no deadline")
		}
		gotCutoff = cutoff
		return []string{"one", "two"}, nil
	}

	server.reapExpiredTmuxSessions()
	if want := now.Add(-48 * time.Hour); !gotCutoff.Equal(want) {
		t.Fatalf("cutoff = %v, want %v", gotCutoff, want)
	}
	if !logger.contains("reaped 2 detached tmux session(s) inactive for at least 48h0m0s") {
		t.Fatalf("reap log missing: %v", logger.lines)
	}
}

func TestReapExpiredTmuxSessionsDisabledDoesNothing(t *testing.T) {
	server := NewServer(&Config{SessionTTL: 0}, NewMockTokenValidator())
	server.reapTmuxSessions = func(context.Context, time.Time) ([]string, error) {
		t.Fatal("disabled reaper was called")
		return nil, nil
	}
	server.reapExpiredTmuxSessions()
}

func TestReapExpiredTmuxSessionsLogsFailure(t *testing.T) {
	server := NewServer(&Config{SessionTTL: time.Hour}, NewMockTokenValidator())
	logger := &reaperCaptureLogger{}
	server.logger = logger
	server.reapTmuxSessions = func(context.Context, time.Time) ([]string, error) {
		return nil, errors.New("tmux wedged")
	}
	server.reapExpiredTmuxSessions()
	if !logger.contains("persistent tmux session reaper failed: tmux wedged") {
		t.Fatalf("failure log missing: %v", logger.lines)
	}
}
