package jobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMeetingCleanupAllowanceIsSharedAndDoesNotRenew(t *testing.T) {
	c := NewMeetingCleanup(180 * time.Second)
	now := time.Now()
	c.now = func() time.Time { return now }
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	for _, spent := range []time.Duration{70 * time.Second, 70 * time.Second} {
		if err := c.Run(parent, func(ctx context.Context) error {
			if ctx.Err() != nil {
				t.Fatal("cancelled parent blocked cleanup")
			}
			now = now.Add(spent)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Run(parent, func(context.Context) error { now = now.Add(50 * time.Second); return nil }); !errors.Is(err, ErrMeetingTeardownUnconfirmed) {
		t.Fatal("fresh cleanup budget minted", err)
	}
	invoked := false
	if err := c.Run(parent, func(context.Context) error { invoked = true; return nil }); err == nil || invoked {
		t.Fatal("exhausted cleanup ran again")
	}
}
func TestMeetingCleanupRejectsOverlapBackwardTimeAndRepeatFinish(t *testing.T) {
	c := NewMeetingCleanup(time.Minute)
	now := time.Now()
	c.now = func() time.Time { return now }
	_, finish, err := c.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Begin(context.Background()); err == nil {
		t.Fatal("overlapping cleanup")
	}
	now = now.Add(10 * time.Second)
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	remaining := c.remaining
	finish()
	if c.remaining != remaining {
		t.Fatal("finish charged twice")
	}
	if err := c.Run(context.Background(), func(context.Context) error { now = now.Add(-time.Second); return nil }); err == nil {
		t.Fatal("backwards cleanup time accepted")
	}
}
func TestMeetingTranscriberLockHonorsDeadline(t *testing.T) {
	h := &MeetingJoinHandler{}
	h.transcribeMu.Lock()
	defer h.transcribeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := h.lockMeetingTranscriber(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
func TestMeetingSetupCannotProceedAfterItsBudget(t *testing.T) {
	for _, flow := range []func(JobContext, meetingBrowser, meetingJoinParams) error{(&MeetingJoinHandler{}).runMeetJoinFlow, (&MeetingJoinHandler{}).runTeamsJoinFlow} {
		p := testParams()
		p.setupDeadline = time.Now().Add(20 * time.Millisecond)
		b := &disclosureBrowser{}
		if err := flow(JobContext{}, b, p); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("setup deadline ignored", err)
		}
		if b.posts != 0 || b.reads != 0 {
			t.Fatal("post-admission work ran after setup expiry")
		}
	}
}
