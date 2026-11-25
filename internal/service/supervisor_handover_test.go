package service

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// TestSupervisorRestartCodeHandsOver pins the update path: a restart-code exit
// hands supervision to a fresh supervisor instead of relaunching the worker
// from this (old) image, and a failed handover falls back to relaunching here.
func TestSupervisorRestartCodeHandsOver(t *testing.T) {
	for _, tc := range []struct {
		name       string
		handedOver bool
		wantStarts int
	}{
		{"successor took over", true, 1},
		{"handover failed", false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &fakeClock{now: time.Unix(0, 0)}
			starts, handovers := 0, 0
			err := RunSupervisor(context.Background(), SupervisorConfig{
				Clock:           clock,
				RestartExitCode: 75,
				Handover:        func() bool { handovers++; return tc.handedOver },
				Start: func() (SupervisedChild, error) {
					starts++
					clock.advance(time.Hour)
					if starts == 1 {
						return newFakeChild(75, true), nil
					}
					return newFakeChild(0, true), nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if handovers != 1 || starts != tc.wantStarts {
				t.Fatalf("handovers=%d starts=%d, want 1 and %d", handovers, starts, tc.wantStarts)
			}
			if len(clock.sleeps) != 0 {
				t.Fatalf("slept %v before relaunching after a restart code", clock.sleeps)
			}
		})
	}
}

// handoverLock models the supervisor lock for SupervisorHandover tests.
type handoverLock struct {
	mu     sync.Mutex
	holder int
	events []string
}

func (l *handoverLock) set(pid int, event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holder = pid
	l.events = append(l.events, event)
}

func (l *handoverLock) get() (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holder != 0, l.holder
}

func newHandover(l *handoverLock, clock *fakeClock, spawn func() (int, error)) SupervisorHandover {
	return SupervisorHandover{
		Spawn:   spawn,
		Release: func() { l.set(0, "release") },
		Reacquire: func() (int, error) {
			if held, pid := l.get(); held {
				return pid, nil
			}
			l.set(1, "reacquire")
			return 0, nil
		},
		Holder:  l.get,
		SelfPID: 1,
		Clock:   clock,
		Timeout: time.Second,
		Poll:    100 * time.Millisecond,
	}
}

func TestSupervisorHandover(t *testing.T) {
	t.Run("successor takes the lock after the release", func(t *testing.T) {
		l := &handoverLock{holder: 1}
		clock := &fakeClock{now: time.Unix(0, 0)}
		h := newHandover(l, clock, func() (int, error) {
			l.set(1, "spawn") // still ours: the successor waits for the release
			return 2, nil
		})
		h.Holder = func() (bool, int) {
			// The successor's retry wins as soon as the lock is free.
			if held, _ := l.get(); !held {
				l.set(2, "successor acquires")
			}
			return l.get()
		}
		if !h.Run() {
			t.Fatal("handover reported failure")
		}
		want := []string{"spawn", "release", "successor acquires"}
		if !reflect.DeepEqual(l.events, want) {
			t.Fatalf("events = %v, want %v (spawn before release, never two holders)", l.events, want)
		}
	})
	t.Run("spawn fails: keep the lock", func(t *testing.T) {
		l := &handoverLock{holder: 1}
		h := newHandover(l, &fakeClock{}, func() (int, error) { return 0, errors.New("no binary") })
		if h.Run() {
			t.Fatal("handover reported success")
		}
		if held, pid := l.get(); !held || pid != 1 || len(l.events) != 0 {
			t.Fatalf("lock changed on a failed spawn: %v %d %v", held, pid, l.events)
		}
	})
	t.Run("successor never takes the lock: retake it", func(t *testing.T) {
		l := &handoverLock{holder: 1}
		clock := &fakeClock{now: time.Unix(0, 0)}
		h := newHandover(l, clock, func() (int, error) { return 2, nil })
		if h.Run() {
			t.Fatal("handover reported success")
		}
		if held, pid := l.get(); !held || pid != 1 {
			t.Fatalf("lock not retaken: %v %d", held, pid)
		}
		if l.events[len(l.events)-1] != "reacquire" {
			t.Fatalf("events = %v", l.events)
		}
	})
	t.Run("lock cannot be retaken: stop rather than run unlocked", func(t *testing.T) {
		l := &handoverLock{holder: 1}
		clock := &fakeClock{now: time.Unix(0, 0)}
		h := newHandover(l, clock, func() (int, error) { return 2, nil })
		h.Reacquire = func() (int, error) { return 0, errors.New("access denied") }
		if !h.Run() {
			t.Fatal("handover kept supervising without its lock")
		}
	})
	t.Run("another supervisor took the lock", func(t *testing.T) {
		l := &handoverLock{holder: 1}
		clock := &fakeClock{now: time.Unix(0, 0)}
		h := newHandover(l, clock, func() (int, error) { return 2, nil })
		h.Release = func() { l.set(3, "a launcher's supervisor wins the race") }
		if !h.Run() {
			t.Fatal("handover reported failure though another supervisor holds the lock")
		}
	})
}
