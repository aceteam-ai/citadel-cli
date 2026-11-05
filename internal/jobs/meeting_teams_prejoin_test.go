package jobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Choosing Teams computer audio may recreate device controls with mic/cam on.
// The fake models that transition, rather than only counting JS calls.
type teamsPrejoinPage struct {
	neverEndingPage
	audio, mic, camera, clicked bool
}

func (p *teamsPrejoinPage) Evaluate(js string) (any, error) {
	switch js {
	case teamsIsAdmittedJS:
		return false, nil
	case teamsSelectComputerAudioJS:
		if !p.audio {
			p.audio, p.mic, p.camera = true, true, true
		}
		return true, nil
	case teamsMuteMicCamJS:
		p.mic, p.camera = false, false
		return 2, nil
	case teamsClickJoinJS:
		p.clicked = true
		return true, nil
	case clickButtonByTextOptionalJS(teamsJoinButtonLabels):
		if !p.audio || p.mic || p.camera {
			return "", nil
		}
		p.clicked = true
		return "Join now", nil
	}
	return false, nil
}
func TestTeamsSelectAudioBeforeMutingDevices(t *testing.T) {
	p := &teamsPrejoinPage{}
	err := pollForTeamsJoinClick(JobContext{Ctx: context.Background()}, p, "Synthetic recorder", "", 10*time.Millisecond, time.Millisecond)
	if err != nil || !p.clicked || !p.audio || p.mic || p.camera {
		t.Fatalf("unsafe prejoin: audio=%v mic=%v camera=%v clicked=%v err=%v", p.audio, p.mic, p.camera, p.clicked, err)
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
