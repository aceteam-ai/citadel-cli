package status

import (
	"encoding/json"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/clockstate"
)

type fixtureClockProvider struct {
	state clockstate.State
	calls int
}

func (p *fixtureClockProvider) Snapshot() clockstate.State { p.calls++; return p.state }

func TestClockStatusProjectionWithoutHostCollection(t *testing.T) {
	for _, state := range []clockstate.State{clockstate.Synced, clockstate.Unsynced, clockstate.Unknown, clockstate.State("invalid")} {
		p := &fixtureClockProvider{state: state}
		// Avoid NewCollector's unrelated existing host/environment discovery and
		// Collect's physical metrics. This is the exact cheap projection it calls.
		c := &Collector{clockState: p}
		s := NodeStatus{NTPSynced: c.clockSynchronization()}
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err = json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		want := any(nil)
		if state == clockstate.Synced {
			want = true
		}
		if state == clockstate.Unsynced {
			want = false
		}
		got, present := out["ntp_synced"]
		if !present || got != want {
			t.Fatal("nullable projection", string(b))
		}
		if p.calls != 1 {
			t.Fatal("getter probed/looped")
		}
		if _, present := out["recordings"]; present {
			t.Fatal("fake recording measurement")
		}
	}
	if (&Collector{}).clockSynchronization() != nil {
		t.Fatal("missing provider not unknown")
	}
	b, err := json.Marshal(NodeStatus{Recordings: &RecordingsSummary{}})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.Unmarshal(b, &out)
	recordings := out["recordings"].(map[string]any)
	if recordings["count"] != float64(0) || recordings["bytes"] != float64(0) {
		t.Fatal("explicit future placeholder shape")
	}
}
