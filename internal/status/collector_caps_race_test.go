package status

import (
	"sync"
	"testing"
)

// SetCapabilities must be safe against a concurrent Collect (citadel-cli#1758:
// the meeting reconciler swaps capabilities while heartbeats collect). Run with -race.
func TestSetCapabilitiesRacesWithCollect(t *testing.T) {
	c := NewCollector(CollectorConfig{Capabilities: &NodeCapabilities{Tags: []string{"cpu:general"}}})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				c.SetCapabilities(&NodeCapabilities{Tags: []string{"cpu:general", "meeting"}})
			}
		}
	}()
	for i := 0; i < 3; i++ {
		st, err := c.Collect()
		if err != nil {
			t.Logf("collect: %v", err)
			continue
		}
		if st.Capabilities == nil {
			t.Error("collected status lost capabilities")
		}
	}
	close(stop)
	wg.Wait()
}
