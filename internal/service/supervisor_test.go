package service

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeClock advances only when the supervisor waits on it or a child "runs".
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
	// block makes After return a channel that never fires.
	block bool
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	ch := make(chan time.Time, 1)
	if !c.block {
		c.now = c.now.Add(d)
		ch <- c.now
	}
	return ch
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type fakeChild struct {
	done        chan struct{}
	code        int
	interrupted bool
	killed      bool
	// exitOnInterrupt closes done when Interrupt is called.
	exitOnInterrupt bool
	once            sync.Once
}

func newFakeChild(code int, exited bool) *fakeChild {
	c := &fakeChild{done: make(chan struct{}), code: code}
	if exited {
		close(c.done)
	}
	return c
}

func (c *fakeChild) Done() <-chan struct{} { return c.done }
func (c *fakeChild) ExitCode() int         { return c.code }
func (c *fakeChild) Interrupt() error {
	c.interrupted = true
	if c.exitOnInterrupt {
		c.once.Do(func() { close(c.done) })
	}
	return nil
}
func (c *fakeChild) Kill() error {
	c.killed = true
	c.once.Do(func() { close(c.done) })
	return nil
}

// run is one scripted child: it "runs" for uptime, then exits with code (or
// fails to start when startErr is set).
type run struct {
	uptime   time.Duration
	code     int
	startErr error
}

func superviseScript(t *testing.T, script []run) (starts int, sleeps []time.Duration) {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	i := 0
	err := RunSupervisor(context.Background(), SupervisorConfig{
		Clock:           clock,
		RestartExitCode: 75,
		Start: func() (SupervisedChild, error) {
			if i >= len(script) {
				t.Fatalf("supervisor started a child after the script ended (%d starts)", i)
			}
			r := script[i]
			i++
			if r.startErr != nil {
				return nil, r.startErr
			}
			clock.advance(r.uptime)
			return newFakeChild(r.code, true), nil
		},
	})
	if err != nil {
		t.Fatalf("RunSupervisor: %v", err)
	}
	return i, clock.sleeps
}

func TestSupervisorBackoffDoublesAndCaps(t *testing.T) {
	var script []run
	for range 10 {
		script = append(script, run{uptime: time.Second, code: 1})
	}
	script = append(script, run{code: 0})
	starts, sleeps := superviseScript(t, script)
	if starts != 11 {
		t.Fatalf("starts = %d, want 11", starts)
	}
	want := []time.Duration{
		2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second,
		64 * time.Second, 128 * time.Second, 256 * time.Second, 5 * time.Minute, 5 * time.Minute,
	}
	if !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
}

func TestSupervisorResetsBackoffAfterStableUptime(t *testing.T) {
	starts, sleeps := superviseScript(t, []run{
		{uptime: time.Second, code: 1},
		{uptime: time.Second, code: 1},
		{uptime: time.Second, code: 1},
		{uptime: 3 * 24 * time.Hour, code: 1}, // a self-heal exit after days up
		{uptime: time.Second, code: 0},
	})
	if starts != 5 {
		t.Fatalf("starts = %d, want 5", starts)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 2 * time.Second}
	if !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
}

func TestSupervisorRestartCodeRelaunchesAtOnce(t *testing.T) {
	starts, sleeps := superviseScript(t, []run{
		{uptime: time.Second, code: 1},
		{uptime: time.Hour, code: 75}, // update restart
		{uptime: time.Second, code: 0},
	})
	if starts != 3 {
		t.Fatalf("starts = %d, want 3", starts)
	}
	if want := []time.Duration{2 * time.Second}; !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v, want %v (no delay after the restart code)", sleeps, want)
	}
}

func TestSupervisorQuickRestartCodeIsAFailure(t *testing.T) {
	_, sleeps := superviseScript(t, []run{
		{uptime: time.Second, code: 75},
		{uptime: time.Second, code: 75},
		{code: 0},
	})
	if want := []time.Duration{2 * time.Second, 4 * time.Second}; !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
}

func TestSupervisorCleanExitStops(t *testing.T) {
	starts, sleeps := superviseScript(t, []run{{uptime: time.Minute, code: 0}})
	if starts != 1 || len(sleeps) != 0 {
		t.Fatalf("starts = %d sleeps = %v, want one start and no sleep", starts, sleeps)
	}
}

func TestSupervisorStartFailureBacksOff(t *testing.T) {
	starts, sleeps := superviseScript(t, []run{
		{startErr: errors.New("binary missing mid-update")},
		{startErr: errors.New("binary missing mid-update")},
		{code: 0},
	})
	if starts != 3 {
		t.Fatalf("starts = %d, want 3", starts)
	}
	if want := []time.Duration{2 * time.Second, 4 * time.Second}; !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
}

func TestSupervisorStopDuringBackoffReturnsPromptly(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0), block: true}
	ctx, cancel := context.WithCancel(context.Background())
	starts := 0
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SupervisorConfig{
			Clock: clock,
			Start: func() (SupervisedChild, error) {
				starts++
				return newFakeChild(1, true), nil
			},
		})
	}()
	// Wait until the supervisor is in its backoff sleep, then stop it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		clock.mu.Lock()
		n := len(clock.sleeps)
		clock.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor never reached its backoff sleep")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunSupervisor: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not return after stop during backoff")
	}
	if starts != 1 {
		t.Fatalf("starts = %d, want 1", starts)
	}
}

func TestSupervisorStopInterruptsRunningWorker(t *testing.T) {
	for _, tc := range []struct {
		name     string
		graceful bool
		wantKill bool
	}{
		{"graceful", true, false},
		{"ignores interrupt", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A graceful worker must win without the grace timer firing.
			clock := &fakeClock{now: time.Unix(0, 0), block: tc.graceful}
			child := newFakeChild(0, false)
			child.exitOnInterrupt = tc.graceful
			ctx, cancel := context.WithCancel(context.Background())
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- RunSupervisor(ctx, SupervisorConfig{
					Clock: clock,
					Start: func() (SupervisedChild, error) {
						close(started)
						return child, nil
					},
				})
			}()
			<-started
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("RunSupervisor: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("supervisor did not stop")
			}
			if !child.interrupted {
				t.Fatal("worker was not interrupted")
			}
			if child.killed != tc.wantKill {
				t.Fatalf("killed = %v, want %v", child.killed, tc.wantKill)
			}
		})
	}
}

func TestSupervisorRequiresStart(t *testing.T) {
	if err := RunSupervisor(context.Background(), SupervisorConfig{}); err == nil {
		t.Fatal("RunSupervisor with no Start returned nil")
	}
}
