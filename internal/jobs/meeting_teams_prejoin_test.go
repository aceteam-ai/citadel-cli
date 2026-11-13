package jobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Teams may recreate device controls after the audio-selection script returns.
// Model that reset after the next Evaluate has observed the old disabled controls.
type teamsPrejoinPage struct {
	neverEndingPage
	audio, mic, camera, clicked bool
	resetPending, unsafeJoin    bool
	passes                      int
	joinByLabel                 bool
	muteResult                  any
	muteErr, audioErr           error
	changedThisPass             bool
}

func (p *teamsPrejoinPage) clickJoin() {
	p.clicked = true
	p.unsafeJoin = p.mic || p.camera || p.resetPending || p.changedThisPass
}

func (p *teamsPrejoinPage) Evaluate(js string) (any, error) {
	reset := p.resetPending
	if reset {
		p.resetPending = false
		defer func() { p.mic, p.camera = true, true }()
	}
	switch js {
	case teamsIsAdmittedJS:
		p.passes++
		p.changedThisPass = false
		return false, nil
	case teamsSelectComputerAudioJS:
		if p.audioErr != nil {
			return nil, p.audioErr
		}
		if !p.audio {
			p.audio, p.resetPending, p.changedThisPass = true, true, true
			return true, nil
		}
		return false, nil
	case teamsMuteMicCamJS:
		if p.muteErr != nil || p.muteResult != nil {
			return p.muteResult, p.muteErr
		}
		n := 0
		if p.mic {
			n++
		}
		if p.camera {
			n++
		}
		p.mic, p.camera = false, false
		p.changedThisPass = p.changedThisPass || n > 0
		return float64(n), nil
	case teamsClickJoinJS:
		if p.joinByLabel {
			return false, nil
		}
		p.clickJoin()
		return true, nil
	case clickButtonByTextOptionalJS(teamsJoinButtonLabels):
		p.clickJoin()
		return "Join now", nil
	}
	return false, nil
}

func TestTeamsWaitForDevicePostureBeforeJoining(t *testing.T) {
	for _, byLabel := range []bool{false, true} {
		t.Run(map[bool]string{false: "data-tid", true: "label"}[byLabel], func(t *testing.T) {
			p := &teamsPrejoinPage{joinByLabel: byLabel}
			err := pollForTeamsJoinClick(JobContext{Ctx: context.Background()}, p, "Synthetic recorder", "", time.Second, time.Millisecond)
			if err != nil || !p.clicked || p.unsafeJoin || !p.audio || p.mic || p.camera || p.passes < 3 {
				t.Fatalf("unsafe prejoin: page=%+v err=%v", p, err)
			}
		})
	}
}

func TestTeamsUnconfirmedDevicePostureDoesNotJoin(t *testing.T) {
	for _, tc := range []struct {
		name string
		page teamsPrejoinPage
	}{
		{name: "audio error", page: teamsPrejoinPage{audioErr: errors.New("audio probe failed")}},
		{name: "mute error", page: teamsPrejoinPage{audio: true, muteErr: errors.New("mute probe failed")}},
		{name: "unknown mute result", page: teamsPrejoinPage{audio: true, muteResult: false}},
		{name: "devices still changing", page: teamsPrejoinPage{audio: true, muteResult: float64(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := pollForTeamsJoinClick(JobContext{Ctx: context.Background()}, &tc.page, "Synthetic recorder", "", 10*time.Millisecond, time.Millisecond)
			if err == nil || tc.page.clicked {
				t.Fatalf("joined without confirmed posture: page=%+v err=%v", tc.page, err)
			}
		})
	}
}

type teamsLobbyPage struct {
	neverEndingPage
	probes        int
	admittedAfter int
	denied        bool
}

func (p *teamsLobbyPage) Evaluate(js string) (any, error) {
	switch js {
	case teamsIsAdmittedJS:
		p.probes++
		return p.admittedAfter > 0 && p.probes >= p.admittedAfter, nil
	case teamsDeniedJS:
		return p.denied, nil
	}
	return false, nil
}
func TestTeamsLobbyOutcomes(t *testing.T) {
	h := &MeetingJoinHandler{}
	for _, tc := range []struct {
		name string
		page teamsLobbyPage
		want error
	}{
		{name: "admitted", page: teamsLobbyPage{admittedAfter: 1}},
		{name: "denied", page: teamsLobbyPage{denied: true}, want: ErrMeetingDenied},
		{name: "still waiting", want: errMeetingAdmitTimeout},
		{name: "admitted wins stale denial", page: teamsLobbyPage{admittedAfter: 1, denied: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := testParams()
			params.AdmitTimeout = 15 * time.Millisecond
			err := h.waitUntilTeamsAdmitted(JobContext{Ctx: context.Background()}, &tc.page, params)
			if tc.want == nil {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}
