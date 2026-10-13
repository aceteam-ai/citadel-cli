package worker

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/clockstate"
)

type fixtureClockState struct{ state clockstate.State }

func (p *fixtureClockState) Snapshot() clockstate.State { return p.state }

func TestJobClockLosslessImmutableAndUnclamped(t *testing.T) {
	claim := int64(9007199254740993)
	now := time.Unix(0, claim)
	provider := &fixtureClockState{clockstate.Synced}
	c := newDeliveryClock(func() time.Time { return now }, provider)
	if got := c.data(map[string]any{}, false); len(got) != 1 || got["t_claim_wall_ns"] != "9007199254740993" {
		t.Fatal(got)
	}
	now = time.Unix(0, -1) // observed backwards wall adjustment, not duration/order.
	result := &JobResult{Output: map[string]any{"clock_state": "spoof", "signed": "unchanged"}}
	data := c.data(map[string]any{"result": result.Output}, true)
	provider.state = clockstate.Unsynced
	now = time.Unix(0, 123)
	c.populate(result)
	if !reflect.DeepEqual(data, c.data(map[string]any{"result": result.Output}, true)) {
		t.Fatal("retry restamped timing")
	}
	if *result.TClaimWallNs != claim || *result.TDoneWallNs != -1 || result.ClockState != clockstate.Synced {
		t.Fatal("internal fields incorrect")
	}
	if result.Output["clock_state"] != "spoof" || data["clock_state"] != clockstate.Synced {
		t.Fatal("handler output became authority or was mutated")
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err = json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["t_claim_wall_ns"] != "9007199254740993" || wire["t_done_wall_ns"] != "-1" {
		t.Fatal("numeric ns rounding", string(b))
	}
	c2 := newDeliveryClock(func() time.Time { return now }, provider)
	if c2.claim == c.claim || c2.done != nil {
		t.Fatal("new delivery inherited old clock")
	}
}

func TestJobClockConcurrentFinalizationAndZero(t *testing.T) {
	c := newDeliveryClock(func() time.Time { return time.Unix(0, 0) }, nil)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int64) { defer wg.Done(); c.finish(time.Unix(0, n)) }(int64(i))
	}
	wg.Wait()
	first := c.data(map[string]any{"result": nil}, true)
	for i := 0; i < 10; i++ {
		if !reflect.DeepEqual(first, c.data(map[string]any{"result": nil}, true)) {
			t.Fatal("concurrent finalization was mutable")
		}
	}
	if first["t_claim_wall_ns"] != "0" || first["clock_state"] != clockstate.Unknown || first["result"] != nil {
		t.Fatal(first)
	}
	var absent *deliveryClock
	if got := absent.data(map[string]any{"result": nil}, true); len(got) != 1 {
		t.Fatal("legacy writer fabricated stamps")
	}
}
