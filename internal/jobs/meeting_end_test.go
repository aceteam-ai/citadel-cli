package jobs

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

type endObservationPage struct {
	ended, count       any
	endedErr, countErr error
}

func (p endObservationPage) Evaluate(expression string) (any, error) {
	if expression == meetIsEndedJS {
		return p.ended, p.endedErr
	}
	if expression == meetParticipantCountJS {
		return p.count, p.countErr
	}
	return nil, errors.New("unexpected fixture expression")
}

func TestMeetingEmptyRoomGraceBoundary(t *testing.T) {
	start := time.Unix(1000, 0)
	for _, count := range []any{0, 1, float64(1)} {
		state := meetingEndTracker{recordingStartedAt: start}
		page := endObservationPage{count: count}
		for _, elapsed := range []time.Duration{0, time.Minute, 15*time.Minute - time.Nanosecond} {
			if reason, ended := state.check(page, start.Add(elapsed)); ended || reason != "" {
				t.Fatalf("early empty room ended at %s: %q", elapsed, reason)
			}
		}
		if reason, ended := state.check(page, start.Add(15*time.Minute)); !ended || reason != "nobody_joined" {
			t.Fatalf("exact fifteen-minute boundary = %q, %v", reason, ended)
		}
	}
}

func TestMeetingEndRemembersAnotherParticipantWithinOneRun(t *testing.T) {
	start := time.Unix(1000, 0)
	state := meetingEndTracker{recordingStartedAt: start}
	page := endObservationPage{count: float64(2)}
	if _, ended := state.check(page, start.Add(time.Minute)); ended {
		t.Fatal("populated call ended")
	}
	page.count, page.countErr = nil, errors.New("toolbar unavailable")
	if _, ended := state.check(page, start.Add(2*time.Minute)); ended {
		t.Fatal("unreadable toolbar ended the call")
	}
	page.count, page.countErr = float64(1), nil
	if reason, ended := state.check(page, start.Add(3*time.Minute)); !ended || reason != "alone_in_call" {
		t.Fatalf("departed participants = %q, %v", reason, ended)
	}
	// Observations from a prior meeting cannot shorten a fresh run's grace.
	fresh := meetingEndTracker{recordingStartedAt: start.Add(3 * time.Minute)}
	if _, ended := fresh.check(page, start.Add(4*time.Minute)); ended {
		t.Fatal("prior run shortened a fresh call")
	}
}

func TestMeetingEmptyRoomNeedsReadableCountAndElapsedGrace(t *testing.T) {
	start := time.Unix(1000, 0)
	for _, page := range []endObservationPage{
		{count: nil}, {count: "1"}, {count: float64(-1)},
		{count: float64(1.5)}, {count: math.NaN()}, {count: math.Inf(1)},
		{count: float64(1), countErr: errors.New("DOM unavailable")},
	} {
		state := meetingEndTracker{recordingStartedAt: start}
		if _, ended := state.check(page, start.Add(time.Hour)); ended {
			t.Fatal("unknown count was treated as empty")
		}
		if state.seenOther {
			t.Fatal("unknown count fabricated participant history")
		}
	}
	for _, began := range []time.Time{{}, start.Add(time.Hour)} {
		state := meetingEndTracker{recordingStartedAt: began}
		if _, ended := state.check(endObservationPage{count: 1}, start); ended {
			t.Fatal("unknown/backwards elapsed time ended the call")
		}
	}
	state := meetingEndTracker{recordingStartedAt: start}
	if _, ended := state.check(endObservationPage{count: float64(2)}, start.Add(time.Hour)); ended {
		t.Fatal("populated room ended after the grace")
	}
}

func TestMeetingParticipantObservationDoesNotNarrowCounts(t *testing.T) {
	for _, value := range []any{int(2), int64(1<<32 + 1), float64(1<<32 + 1)} {
		state := meetingEndTracker{recordingStartedAt: time.Unix(1000, 0)}
		if _, ended := state.check(endObservationPage{count: value}, state.recordingStartedAt); ended {
			t.Fatal("other participant narrowed to an empty room")
		}
		if reason, ended := state.check(endObservationPage{count: 1}, state.recordingStartedAt); !ended || reason != "alone_in_call" {
			t.Fatal("valid prior participants were not remembered")
		}
	}
	state := meetingEndTracker{recordingStartedAt: time.Unix(1000, 0)}
	_, _ = state.check(endObservationPage{count: float64(2.5)}, state.recordingStartedAt)
	if _, ended := state.check(endObservationPage{count: 1}, state.recordingStartedAt); ended {
		t.Fatal("fractional count shortened the grace")
	}
}

func TestMeetingExplicitEndPrecedesEmptyRoomGrace(t *testing.T) {
	for _, page := range []endObservationPage{{ended: true, count: 1}, {ended: true, countErr: errors.New("DOM unavailable")}} {
		state := meetingEndTracker{recordingStartedAt: time.Unix(1000, 0)}
		if reason, ended := state.check(page, state.recordingStartedAt); !ended || reason != "call_ended" {
			t.Fatalf("explicit end = %q, %v", reason, ended)
		}
	}
}

// Only fake page evaluations and context signals run in these loop fixtures.
// Embedding neverEndingPage supplies the unused browser lifecycle methods.
type graceSequenceBrowser struct {
	neverEndingPage
	page      *fakeMeetPage
	counts    []float64
	reads     int
	afterLast func()
}

func (p *graceSequenceBrowser) Evaluate(expression string) (any, error) {
	if expression == meetParticipantCountJS {
		if p.reads >= len(p.counts) {
			return nil, errors.New("fixture exhausted")
		}
		n := p.counts[p.reads]
		p.reads++
		if p.reads == len(p.counts) && p.afterLast != nil {
			p.afterLast()
		}
		return n, nil
	}
	return p.page.Evaluate(expression)
}

func runGraceFixture(h *MeetingJoinHandler, interactive bool, ctx context.Context, page *graceSequenceBrowser, media deathTestMedia) (string, error) {
	if !interactive {
		return h.waitForMeetingEnd(JobContext{Ctx: ctx}, page, testParams(), media)
	}
	out, err := h.waitForMeetingEndInteractive(JobContext{Ctx: ctx}, page, testParams(), func() ([]TranscriptSegment, error) { return nil, nil }, time.Millisecond, map[string]struct{}{}, media)
	return out.endReason, err
}

func TestMeetingGraceWiredIntoBothLoopsWithoutMaskingStop(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		for _, recorderDeath := range []bool{false, true} {
			t.Run(map[bool]string{false: "plain", true: "interactive"}[interactive]+"/"+map[bool]string{false: "cancel", true: "recorder"}[recorderDeath], func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				died := make(chan struct{})
				after := cancel
				want := "cancelled"
				if recorderDeath {
					after = func() { close(died) }
					want = "recorder_died"
				}
				page := &graceSequenceBrowser{page: &fakeMeetPage{}, counts: []float64{1}, afterLast: after}
				h := &MeetingJoinHandler{StreamingInterval: time.Hour}
				reason, err := runGraceFixture(h, interactive, ctx, page, deathTestMedia{alive: died})
				if reason != want || err == nil {
					t.Fatalf("early arrival loop = %q, %v; want %s with error", reason, err, want)
				}
				if !recorderDeath && !errors.Is(err, context.Canceled) {
					t.Fatal("lost cancellation cause", err)
				}
			})
		}
	}
}

func TestMeetingBothLoopsRetainParticipantHistory(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		page := &graceSequenceBrowser{page: &fakeMeetPage{}, counts: []float64{2, 1}, afterLast: cancel}
		h := &MeetingJoinHandler{StreamingInterval: time.Hour}
		reason, err := runGraceFixture(h, interactive, ctx, page, deathTestMedia{})
		cancel()
		if reason != "alone_in_call" || err != nil || page.reads != 2 {
			t.Fatalf("history lost: interactive=%v reason=%q err=%v reads=%d", interactive, reason, err, page.reads)
		}
	}
}
