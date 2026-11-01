package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMeetingAdmissionTimeoutPayload(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{{"180", 3 * time.Minute}, {"600", 10 * time.Minute}, {"1", time.Second}} {
		p, err := parseMeetingJoinParams(map[string]string{"meeting_url": "https://meet.google.com/x", "meeting_id": "synthetic", "admit_timeout_seconds": tc.raw})
		if err != nil || p.admissionTimeout() != tc.want {
			t.Fatalf("admission %q: duration=%v err=%v, want %v", tc.raw, p.admissionTimeout(), err, tc.want)
		}
	}
}

func TestMeetingAdmissionTimeoutRejectsMalformedAndOverflow(t *testing.T) {
	for _, raw := range []string{"", " ", " 600 ", "601", "00", "+1", "0", "-1", "NaN", "Inf", "+Inf", "-Inf", "1.5", "600.0", "1e3", "9223372037", "9223372036854775808", "do-not-echo-this-synthetic-value"} {
		_, err := parseMeetingJoinParams(map[string]string{"meeting_url": "https://meet.google.com/x", "meeting_id": "synthetic", "admit_timeout_seconds": raw})
		if err == nil {
			t.Fatalf("admission accepted %q", raw)
		}
		if len(raw) > 3 && strings.Contains(err.Error(), raw) {
			t.Fatalf("admission error reflected raw input %q", raw)
		}
	}
}

type admissionProbeBrowser struct {
	neverEndingPage
	admitted bool
	probeErr error
	onProbe  func()
}

func (p admissionProbeBrowser) Evaluate(expression string) (any, error) {
	if expression == meetIsAdmittedJS || expression == teamsIsAdmittedJS {
		if p.onProbe != nil {
			p.onProbe()
		}
		return p.admitted, p.probeErr
	}
	if expression == meetDeniedJS || expression == teamsDeniedJS {
		return false, nil // an undecided lobby is never a denial
	}
	return true, nil // The Teams pre-admission dialog dismissal is synthetic.
}

func TestMeetingAdmissionLoopsHonorBudgetsAndCancellation(t *testing.T) {
	h := &MeetingJoinHandler{}
	for _, loop := range []struct {
		name string
		wait func(JobContext, meetingBrowser, meetingJoinParams) error
	}{{"meet", h.waitUntilAdmitted}, {"teams", h.waitUntilTeamsAdmitted}} {
		t.Run(loop.name, func(t *testing.T) {
			t.Run("shorter than poll interval", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				p := testParams()
				p.AdmitTimeout = 25 * time.Millisecond
				start := time.Now()
				err := loop.wait(JobContext{Ctx: ctx}, admissionProbeBrowser{}, p)
				if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "not admitted") {
					t.Fatalf("payload timeout did not govern: %v", err)
				}
				if elapsed := time.Since(start); elapsed > time.Second {
					t.Fatalf("admission overshot short budget: %v", elapsed)
				}
			})
			t.Run("parent deadline stays authoritative", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
				defer cancel()
				p := testParams()
				p.AdmitTimeout = time.Hour
				err := loop.wait(JobContext{Ctx: ctx}, admissionProbeBrowser{}, p)
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("lost parent deadline: %v", err)
				}
			})
			t.Run("cancelled during successful probe", func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				err := loop.wait(JobContext{Ctx: ctx}, admissionProbeBrowser{admitted: true, onProbe: cancel}, testParams())
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("late successful probe ignored cancellation: %v", err)
				}
			})
			t.Run("successful probe after budget", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				p := testParams()
				p.AdmitTimeout = 10 * time.Millisecond
				err := loop.wait(JobContext{Ctx: ctx}, admissionProbeBrowser{admitted: true, onProbe: func() { time.Sleep(20 * time.Millisecond) }}, p)
				if err == nil || !strings.Contains(err.Error(), "not admitted") {
					t.Fatalf("late probe reported admission: %v", err)
				}
			})
			t.Run("success within budget", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := loop.wait(JobContext{Ctx: ctx}, admissionProbeBrowser{admitted: true}, testParams()); err != nil {
					t.Fatal(err)
				}
			})
			t.Run("probe failure keeps bounded retry", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				p := testParams()
				p.AdmitTimeout = 25 * time.Millisecond
				err := loop.wait(JobContext{Ctx: ctx}, admissionProbeBrowser{probeErr: errors.New("synthetic unavailable DOM")}, p)
				if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "not admitted") {
					t.Fatalf("probe failure lost admission deadline: %v", err)
				}
			})
		})
	}
}
