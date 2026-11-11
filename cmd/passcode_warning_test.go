package cmd

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPasscodeWarningReminderIntervalAndBackwardClock(t *testing.T) {
	var reminder passcodeWarningReminder
	now := time.Unix(1_000, 0)
	for _, test := range []struct {
		offset time.Duration
		want   bool
	}{
		{0, true},
		{0, false},
		{time.Minute - time.Nanosecond, false},
		{time.Minute, true},
		{time.Minute, false},
		{30 * time.Second, true},
		{30 * time.Second, false},
		{90 * time.Second, true},
	} {
		if got := reminder.shouldWarn(passcodeWarningMaster, "private-node", func() time.Time { return now.Add(test.offset) }); got != test.want {
			t.Fatalf("offset %s: warning=%v, want %v", test.offset, got, test.want)
		}
	}
}

func TestPasscodeWarningReminderKeepsOnlyCurrentContextPerMode(t *testing.T) {
	var reminder passcodeWarningReminder
	now := time.Unix(1_000, 0)
	clock := func() time.Time { return now }
	if !reminder.shouldWarn(passcodeWarningMaster, "node-a", clock) || !reminder.shouldWarn(passcodeWarningLegacy, "node-a", clock) {
		t.Fatal("one mode suppressed the other mode's first warning")
	}
	if reminder.shouldWarn(passcodeWarningMaster, "node-a", clock) || reminder.shouldWarn(passcodeWarningLegacy, "node-a", clock) {
		t.Fatal("same-context warnings were not bounded")
	}
	for i := 0; i < 1_000; i++ {
		if !reminder.shouldWarn(passcodeWarningMaster, time.Unix(int64(i), 0).String(), clock) {
			t.Fatal("changed current node context lost its immediate warning")
		}
	}
	if len(reminder.slots) != 2 || reminder.slots[passcodeWarningLegacy].nodeDir != "node-a" {
		t.Fatal("context changes grew storage or replaced the other mode")
	}
	if reminder.shouldWarn(passcodeWarningModeCount, "ignored", clock) {
		t.Fatal("invalid internal mode emitted a warning")
	}
}

func TestPasscodeWarningReminderConcurrentFirstDenials(t *testing.T) {
	var reminder passcodeWarningReminder
	now := time.Unix(1_000, 0)
	clock := func() time.Time { return now }
	start := make(chan struct{})
	var count atomic.Int32
	var callers sync.WaitGroup
	for i := 0; i < 64; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			<-start
			if reminder.shouldWarn(passcodeWarningMaster, "private-node", clock) {
				count.Add(1)
			}
		}()
	}
	close(start)
	callers.Wait()
	if got := count.Load(); got != 1 {
		t.Fatalf("concurrent first denials emitted %d warnings, want one", got)
	}
}
