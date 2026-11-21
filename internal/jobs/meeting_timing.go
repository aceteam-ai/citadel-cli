package jobs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/meetingpolicy"
)

var ErrMeetingTeardownUnconfirmed = errors.New("meeting teardown or owned operation remains unconfirmed")

// ErrMeetingStartRefused marks a definite media-start refusal that created no
// owned resource (profile busy, meetingd busy/not ready, a meetingd 4xx). It is
// an ordinary terminal failure, never teardown uncertainty.
var ErrMeetingStartRefused = errors.New("meeting media start refused")

// MeetingCleanup accounts one allowance across stop/flush, rolling shutdown,
// teardown, backup and terminal publication. It never replenishes on a phase
// transition or parent cancellation. A cancelled execution may still clean up.
type MeetingCleanup struct {
	mu        sync.Mutex
	remaining time.Duration
	active    bool
	now       func() time.Time
}

func NewMeetingCleanup(allowance time.Duration) *MeetingCleanup {
	return &MeetingCleanup{remaining: allowance, now: time.Now}
}

type meetingCleanupKey struct{}
type meetingExecutionStartKey struct{}

// Worker execution start is local monotonic timing, never a payload timestamp.
func WithMeetingExecutionStart(ctx context.Context, start time.Time) context.Context {
	return context.WithValue(ctx, meetingExecutionStartKey{}, start)
}
func meetingExecutionStart(ctx context.Context) time.Time {
	if t, ok := ctx.Value(meetingExecutionStartKey{}).(time.Time); ok {
		return t
	}
	return time.Now()
}

func WithMeetingCleanup(ctx context.Context, cleanup *MeetingCleanup) context.Context {
	return context.WithValue(ctx, meetingCleanupKey{}, cleanup)
}
func MeetingCleanupFrom(ctx context.Context) *MeetingCleanup {
	c, _ := ctx.Value(meetingCleanupKey{}).(*MeetingCleanup)
	return c
}

// Begin permits one cleanup phase. Finish observes actual return, not a timeout
// wrapper; an uncooperative native call retains the handler/backstop's custody.
func (c *MeetingCleanup) Begin(parent context.Context) (context.Context, func() error, error) {
	c.mu.Lock()
	if c.active || c.remaining <= 0 {
		c.mu.Unlock()
		return nil, nil, ErrMeetingTeardownUnconfirmed
	}
	c.active = true
	started := c.now()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), c.remaining)
	c.mu.Unlock()
	var once sync.Once
	var result error
	finish := func() error {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			elapsed := c.now().Sub(started)
			if elapsed < 0 {
				c.remaining = 0
			} else if elapsed >= c.remaining {
				c.remaining = 0
			} else {
				c.remaining -= elapsed
			}
			c.active = false
			if ctx.Err() != nil || c.remaining <= 0 {
				result = ErrMeetingTeardownUnconfirmed
			}
			cancel()
		})
		return result
	}
	return ctx, finish, nil
}

func (c *MeetingCleanup) Run(parent context.Context, fn func(context.Context) error) error {
	ctx, finish, err := c.Begin(parent)
	if err != nil {
		return err
	}
	defer finish()
	operationErr := fn(ctx)
	return errors.Join(operationErr, finish())
}

func checkMeetingPhase(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := fn()
	if expired := ctx.Err(); expired != nil {
		return errors.Join(expired, err)
	}
	return err
}

func meetingSleep(ctx context.Context, delay time.Duration) error {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type phaseMeetingBrowser struct {
	meetingBrowser
	ctx context.Context
}

func (b phaseMeetingBrowser) Navigate(url string) error {
	return checkMeetingPhase(b.ctx, func() error {
		if native, ok := b.meetingBrowser.(interface {
			NavigateContext(context.Context, string) error
		}); ok {
			return native.NavigateContext(b.ctx, url)
		}
		return b.meetingBrowser.Navigate(url)
	})
}
func (b phaseMeetingBrowser) Evaluate(js string) (value any, err error) {
	err = checkMeetingPhase(b.ctx, func() error {
		var e error
		if native, ok := b.meetingBrowser.(interface {
			EvaluateContext(context.Context, string) (any, error)
		}); ok {
			value, e = native.EvaluateContext(b.ctx, js)
		} else {
			value, e = b.meetingBrowser.Evaluate(js)
		}
		return e
	})
	if err != nil {
		value = nil
	}
	return
}
func (b phaseMeetingBrowser) CurrentURL() (value string, err error) {
	err = checkMeetingPhase(b.ctx, func() error {
		var e error
		if native, ok := b.meetingBrowser.(interface {
			CurrentURLContext(context.Context) (string, error)
		}); ok {
			value, e = native.CurrentURLContext(b.ctx)
		} else {
			value, e = b.meetingBrowser.CurrentURL()
		}
		return e
	})
	if err != nil {
		value = ""
	}
	return
}
func (b phaseMeetingBrowser) Type(selector, text string) error {
	return checkMeetingPhase(b.ctx, func() error {
		if native, ok := b.meetingBrowser.(interface {
			TypeContext(context.Context, string, string) error
		}); ok {
			return native.TypeContext(b.ctx, selector, text)
		}
		return b.meetingBrowser.Type(selector, text)
	})
}

func setupMeetingContext(ctx JobContext, p meetingJoinParams) (JobContext, context.CancelFunc) {
	deadline := p.setupDeadline
	if deadline.IsZero() {
		deadline = time.Now().Add(time.Duration(p.timing.Profile().Setup) * time.Second)
		if p.timing.Profile().Setup == 0 {
			deadline = time.Now().Add(time.Duration(meetingpolicy.Default().Setup) * time.Second)
		}
	}
	c, cancel := context.WithDeadline(ctx.Context(), deadline)
	ctx.Ctx = c
	return ctx, cancel
}

func meetingPollRemaining(deadline, now time.Time, interval time.Duration) time.Duration {
	remaining := deadline.Sub(now)
	if remaining < 0 {
		return 0
	}
	if remaining < interval {
		return remaining
	}
	return interval
}

// meetingd's existing max_duration_seconds is a session age, beginning before
// the lobby. It must not consume the promised recording window. Transcription
// follows confirmed browser teardown and is not part of this media lifetime.
func (p meetingJoinParams) mediaLifetime() time.Duration {
	profile := p.timing.Profile()
	if profile.Setup == 0 {
		profile = meetingpolicy.Default()
	}
	return time.Duration(profile.Setup+profile.Cleanup)*time.Second + p.admissionTimeout() + p.maxDuration()
}

func (h *MeetingJoinHandler) lockMeetingTranscriber(ctx context.Context) error {
	for !h.transcribeMu.TryLock() {
		if err := meetingSleep(ctx, 10*time.Millisecond); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		h.transcribeMu.Unlock()
		return err
	}
	return nil
}
