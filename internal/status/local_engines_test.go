package status

import (
	"context"
	"errors"
	"testing"
)

// stubModelLister answers DiscoverModels from a fixed per-engine table.
type stubModelLister struct {
	models map[string][]string
	errs   map[string]error
}

func (s stubModelLister) DiscoverModels(_ context.Context, serviceType string, _ int) ([]string, error) {
	if err, ok := s.errs[serviceType]; ok {
		return nil, err
	}
	return s.models[serviceType], nil
}

// runningFor returns a portIfRunning stub that claims the named engines are
// running (the state the pre-#649 process check reported for a dead ollama).
func runningFor(names ...string) func(map[string]bool, string) (int, bool) {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(_ map[string]bool, name string) (int, bool) {
		if set[name] {
			return 11434, true
		}
		return 0, false
	}
}

// TestDiscoverLocalEnginesDropsNonAnsweringEngine is the advertise half of #649.
//
// This list is not merely informational: cmd/work.go's nodeIsServingModels gates
// the shared jobs:v1:gpu-general subscription on it being non-empty, so an
// engine reported here that cannot actually answer makes the node claim
// inference jobs it cannot serve -- and every one of them times out at the
// caller. An engine whose API errors must not appear.
func TestDiscoverLocalEnginesDropsNonAnsweringEngine(t *testing.T) {
	md := stubModelLister{errs: map[string]error{"ollama": errors.New("connection refused")}}

	got := discoverLocalEngines(context.Background(), nil, runningFor("ollama"), md)

	for _, e := range got {
		if e.Name == "ollama" {
			t.Fatalf("a non-answering engine must not be advertised, got %+v", got)
		}
	}
}

// TestDiscoverLocalEnginesKeepsRunningButEmptyEngine pins the case the drop must
// NOT catch. A live ollama with nothing pulled answers /api/tags with an empty
// list and a nil error -- that is real, reportable state, and the package
// documents it as such.
//
// This is why the skip keys on the ERROR rather than on len(models): the two
// cases are indistinguishable by model count and only the error separates
// "running and empty" from "not there at all".
func TestDiscoverLocalEnginesKeepsRunningButEmptyEngine(t *testing.T) {
	md := stubModelLister{models: map[string][]string{"ollama": {}}}

	got := discoverLocalEngines(context.Background(), nil, runningFor("ollama"), md)

	found := false
	for _, e := range got {
		if e.Name == "ollama" {
			found = true
			if len(e.Models) != 0 {
				t.Errorf("expected no models, got %v", e.Models)
			}
		}
	}
	if !found {
		t.Error("a live engine with nothing pulled is real state and must still be reported")
	}
}

// TestDiscoverLocalEnginesReportsModels is the ordinary healthy path, so the
// two tests above cannot both pass by the function returning nothing ever.
func TestDiscoverLocalEnginesReportsModels(t *testing.T) {
	md := stubModelLister{models: map[string][]string{"ollama": {"llama3.1:8b"}}}

	got := discoverLocalEngines(context.Background(), nil, runningFor("ollama"), md)

	if len(got) != 1 || got[0].Name != "ollama" || len(got[0].Models) != 1 || got[0].Models[0] != "llama3.1:8b" {
		t.Fatalf("expected ollama serving llama3.1:8b, got %+v", got)
	}
}

// TestDiscoverLocalEnginesThreadsRunningSnapshot pins that the `running` map
// discoverLocalEngines receives is the SAME one handed to portIfRunning on every
// iteration, not silently dropped or replaced with a fresh lookup. runningFor
// (used by the other tests here) closes over its own set and ignores the map
// argument entirely, so it would keep passing even if discoverLocalEngines
// stopped threading the snapshot through -- this test's stub, unlike runningFor,
// actually inspects the map it is handed.
func TestDiscoverLocalEnginesThreadsRunningSnapshot(t *testing.T) {
	want := map[string]bool{"ollama": true}
	md := stubModelLister{models: map[string][]string{"ollama": {"llama3.1:8b"}}}

	portIfRunning := func(running map[string]bool, name string) (int, bool) {
		if running["ollama"] != want["ollama"] {
			t.Fatalf("portIfRunning got running=%v, want %v", running, want)
		}
		if name == "ollama" && running[name] {
			return 11434, true
		}
		return 0, false
	}

	got := discoverLocalEngines(context.Background(), want, portIfRunning, md)

	if len(got) != 1 || got[0].Name != "ollama" {
		t.Fatalf("expected ollama to be reported using the threaded snapshot, got %+v", got)
	}
}

// TestDiscoverLocalEnginesListsRunningContainersOnce pins the aceteam#7148
// follow-up fix: DiscoverLocalEngines must enumerate the running
// "citadel-<name>" containers ONCE per call, not once per managedProbeEngines
// entry. Before the fix, DiscoverLocalEngines injected managedEnginePortIfRunning
// -- which itself calls runningEmbeddedServices (and therefore the underlying
// `docker ps`) -- so every one of the ~5 managedProbeEngines lookups triggered
// its own fresh container-runtime enumeration. This path is hit by the gateway
// chat-route, `citadel chat`, and nodeIsServingModels, not just the 30s
// heartbeat, so the redundant work mattered on every call, not just once every
// 30s.
func TestDiscoverLocalEnginesListsRunningContainersOnce(t *testing.T) {
	prev := runningContainerNames
	calls := 0
	runningContainerNames = func(string) []string {
		calls++
		return []string{"citadel-ollama"}
	}
	t.Cleanup(func() { runningContainerNames = prev })

	_ = DiscoverLocalEngines(context.Background())

	if calls != 1 {
		t.Fatalf("expected exactly 1 container-runtime enumeration for DiscoverLocalEngines, got %d", calls)
	}
}
