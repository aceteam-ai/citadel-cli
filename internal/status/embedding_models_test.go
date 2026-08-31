package status

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"reflect"
	"testing"

	nativesvc "github.com/aceteam-ai/citadel-cli/internal/services"
)

// citadel-cli#690: an embedding server reports the model it is actually serving,
// and a stopped engine's <name>.env default stops claiming a model a running
// service already serves.
//
// The live case this reproduces, from an RTX 3090 node: a running TEI answered
// GET /info with model_id "Alibaba-NLP/gte-multilingual-base" while reporting no
// models at all, so the platform credited that model to a STOPPED vllm entry
// whose env default named the same id. The embedding model was advertised as a
// chat model on a dead engine, and the live engine advertised nothing.

// teiInfoHandler serves TEI's GET /info with the given model_id.
func teiInfoHandler(modelID string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model_id":         modelID,
			"model_dtype":      "float16",
			"max_input_length": 512,
		})
	})
	return mux
}

func TestDiscoverEmbeddingModel_ReportsServedModel(t *testing.T) {
	discovery, port := newTestDiscovery(t, teiInfoHandler("Alibaba-NLP/gte-multilingual-base"))

	models, err := discovery.DiscoverEmbeddingModel(context.Background(), port)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(models, []string{"Alibaba-NLP/gte-multilingual-base"}) {
		t.Fatalf("Models = %v, want [Alibaba-NLP/gte-multilingual-base]", models)
	}
}

func TestDiscoverEmbeddingModel_NoModelIDIsEmptyNotError(t *testing.T) {
	discovery, port := newTestDiscovery(t, teiInfoHandler(""))

	models, err := discovery.DiscoverEmbeddingModel(context.Background(), port)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("Models = %v, want empty", models)
	}
}

func TestDiscoverEmbeddingModel_UnreachableErrors(t *testing.T) {
	discovery := NewModelDiscovery()
	discovery.host = "127.0.0.1"

	// Port 1 is reserved and never listening.
	if _, err := discovery.DiscoverEmbeddingModel(context.Background(), 1); err == nil {
		t.Fatal("expected an error from an unreachable embedding server")
	}
}

// stubEmbeddingLister answers DiscoverEmbeddingModel from a fixed table.
type stubEmbeddingLister struct {
	models []string
	err    error
}

func (s stubEmbeddingLister) DiscoverEmbeddingModel(_ context.Context, _ int) ([]string, error) {
	return s.models, s.err
}

// alwaysHealthy / neverRunning are injected stand-ins for the live probes.
func alwaysHealthy(int) bool { return true }

// A NATIVE embedding server (no container) must still be reported. Container
// presence is not the test for "running": ollama on the 3090 node runs as a
// systemd unit, and a docker-only check reports a false negative for exactly
// that shape of install.
func TestCollectEmbeddingServices_NativeProcessCounts(t *testing.T) {
	// portIfRunning stands in for managedEnginePortIfRunning, whose real
	// implementation checks the container first and falls back to the native
	// service probe. Here only the native branch answers.
	nativeOnly := func(name string) (int, bool) {
		if name == "tei" {
			return 8102, true
		}
		return 0, false
	}
	md := stubEmbeddingLister{models: []string{"Alibaba-NLP/gte-multilingual-base"}}

	got := collectEmbeddingServices(context.Background(), nativeOnly, alwaysHealthy, md)
	if len(got) != 1 {
		t.Fatalf("expected the native embedding server reported, got %+v", got)
	}
	if got[0].Type != ServiceTypeEmbedding {
		t.Errorf("Type = %q, want %q", got[0].Type, ServiceTypeEmbedding)
	}
	if !reflect.DeepEqual(got[0].Models, []string{"Alibaba-NLP/gte-multilingual-base"}) {
		t.Errorf("Models = %v, want the model the server reports", got[0].Models)
	}
}

// The production wrapper resolves "running" through managedEnginePortIfRunning,
// so this pins the property that function must have: a service with NO container
// but a live native listener counts as running.
//
// Hermetic on purpose. A bogus engine binary makes the container branch fail
// unconditionally, and a temporary NativeServices entry pointed at a real
// listener on an ephemeral port makes the native branch the only thing that can
// answer. Reusing a real engine's port would pass or fail depending on what
// happens to be running on the developer's machine, which is how a
// container-only regression slips through green.
func TestManagedEnginePortIfRunning_DetectsNativeWithoutAContainer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener addr type %T", ln.Addr())
	}

	const name = "native-embedding-probe-test"
	nativesvc.NativeServices[name] = nativesvc.NativeService{
		Name:   name,
		Binary: "definitely-not-installed-binary",
		Port:   port.Port,
	}
	defer delete(nativesvc.NativeServices, name)

	gotPort, running := managedEnginePortIfRunning("no-such-container-runtime", name)
	if !running {
		t.Fatal("a natively served engine with no container must count as running")
	}
	if gotPort != port.Port {
		t.Errorf("port = %d, want the native listener port %d", gotPort, port.Port)
	}
}

// A server whose /info probe fails is still reported (running is real state),
// but must not have a model invented for it.
func TestCollectEmbeddingServices_ProbeFailureLeavesModelsEmpty(t *testing.T) {
	running := func(name string) (int, bool) { return 8102, name == "tei" }
	md := stubEmbeddingLister{err: context.DeadlineExceeded}

	got := collectEmbeddingServices(context.Background(), running, alwaysHealthy, md)
	if len(got) != 1 {
		t.Fatalf("expected tei reported, got %+v", got)
	}
	if len(got[0].Models) != 0 {
		t.Errorf("Models = %v, want empty when the probe failed", got[0].Models)
	}
}

// A stopped engine must not claim a model a RUNNING service already serves.
// This is what re-attributes gte-multilingual-base from the stopped vllm to the
// running tei on node 1297.
func TestCollectInstalledEngines_SkipsModelClaimedByRunningService(t *testing.T) {
	stubHotswapPreflightPass(t)
	dir := t.TempDir()
	writeInstalledEngine(t, dir, "vllm", "VLLM_MODEL=Alibaba-NLP/gte-multilingual-base")

	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})

	// Nothing running: the stopped vllm is still an honest swap candidate.
	unclaimed := c.collectInstalledEngines(map[string]struct{}{}, map[string]struct{}{}, SystemMetrics{})
	if len(unclaimed) != 1 || unclaimed[0].Name != "vllm" {
		t.Fatalf("without a claim, expected vllm advertised, got %+v", unclaimed)
	}

	// tei is running and serving that exact model: the stopped vllm drops it.
	claimed := map[string]struct{}{"Alibaba-NLP/gte-multilingual-base": {}}
	got := c.collectInstalledEngines(map[string]struct{}{}, claimed, SystemMetrics{})
	for _, e := range got {
		if e.Name == "vllm" {
			t.Fatalf("stopped vllm still claims a model the running tei serves: %+v", e)
		}
	}
}

// applyModelHotswap builds the claimed set from the running services it is
// handed, so the subtraction works end to end and not just via the helper.
func TestApplyModelHotswap_RunningEmbeddingServiceOutranksStoppedEngine(t *testing.T) {
	dir := t.TempDir()
	writeInstalledEngine(t, dir, "vllm", "VLLM_MODEL=Alibaba-NLP/gte-multilingual-base")

	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})
	st := &NodeStatus{Services: []ServiceInfo{
		{
			Name:   "tei",
			Type:   ServiceTypeEmbedding,
			Status: ServiceStatusRunning,
			Health: HealthStatusOK,
			Models: []string{"Alibaba-NLP/gte-multilingual-base"},
		},
	}}
	c.applyModelHotswap(st, map[string]struct{}{"tei": {}})

	for _, s := range st.Services {
		if s.Name == "vllm" {
			t.Fatalf("stopped vllm advertised a model the running tei serves: %+v", s)
		}
	}

	var tei *ServiceInfo
	for i := range st.Services {
		if st.Services[i].Name == "tei" {
			tei = &st.Services[i]
		}
	}
	if tei == nil {
		t.Fatal("tei disappeared from the status")
	}
	if !reflect.DeepEqual(tei.Models, []string{"Alibaba-NLP/gte-multilingual-base"}) {
		t.Fatalf("tei Models = %v, want the model it actually serves", tei.Models)
	}
	// An embedding server is not a serving engine for hotswap purposes, so it
	// must not pick up a residency flag meant for chat engines.
	if tei.Resident != nil {
		t.Errorf("tei Resident = %v, want nil (embedding services are not swap candidates)", tei.Resident)
	}
}
