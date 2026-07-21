package mesh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mkStatus(nodeName string, svcs ...struct {
	name, status string
	port         int
	models       []string
}) *nodeStatus {
	st := &nodeStatus{}
	st.Node.Name = nodeName
	for _, s := range svcs {
		st.Services = append(st.Services, struct {
			Name   string   `json:"name"`
			Type   string   `json:"type"`
			Status string   `json:"status"`
			Port   int      `json:"port"`
			Models []string `json:"models"`
		}{Name: s.name, Type: "llm", Status: s.status, Port: s.port, Models: s.models})
	}
	return st
}

func TestEngineTypeFromName(t *testing.T) {
	cases := map[string]string{
		"vllm":             "vllm",
		"citadel-vllm":     "vllm",
		"ollama":           "ollama",
		"bonsai":           "bonsai",
		"llamacpp":         "llamacpp",
		"llama.cpp":        "llamacpp",
		"llama-cpp-server": "llamacpp",
		"postgres":         "",
		"OLLAMA-Big":       "ollama", // case-insensitive; ollama checked before llama
	}
	for in, want := range cases {
		if got := EngineTypeFromName(in); got != want {
			t.Errorf("EngineTypeFromName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModelsFromStatus(t *testing.T) {
	p := Peer{Hostname: "gpu-1", IP: "100.64.0.5", Online: true}
	st := mkStatus("gpu-1-node",
		struct {
			name, status string
			port         int
			models       []string
		}{"vllm", "running", 8201, []string{"Qwen/Qwen2.5-7B"}},
		// stopped service -> excluded
		struct {
			name, status string
			port         int
			models       []string
		}{"ollama", "stopped", 11434, []string{"llama3"}},
		// running but no port -> excluded (cannot route)
		struct {
			name, status string
			port         int
			models       []string
		}{"bonsai", "running", 0, []string{"Bonsai-27B"}},
		// running, no models -> excluded
		struct {
			name, status string
			port         int
			models       []string
		}{"llamacpp", "running", 8200, nil},
		// running with two models -> two entries, blank trimmed
		struct {
			name, status string
			port         int
			models       []string
		}{"llamacpp", "running", 8200, []string{"modelA", "  ", "modelB"}},
	)

	got := modelsFromStatus(p, st)
	if len(got) != 3 {
		t.Fatalf("expected 3 served models, got %d: %+v", len(got), got)
	}
	// vllm entry
	assertModel(t, got, "Qwen/Qwen2.5-7B", "vllm", 8201)
	assertModel(t, got, "modelA", "llamacpp", 8200)
	assertModel(t, got, "modelB", "llamacpp", 8200)
	for _, m := range got {
		if m.Hostname != "gpu-1" || m.IP != "100.64.0.5" || m.NodeName != "gpu-1-node" {
			t.Errorf("model %q has wrong node identity: %+v", m.Model, m)
		}
	}
}

func assertModel(t *testing.T, models []ServedModel, name, engine string, port int) {
	t.Helper()
	for _, m := range models {
		if m.Model == name {
			if m.Engine != engine || m.Port != port {
				t.Errorf("model %q = engine %q port %d, want engine %q port %d", name, m.Engine, m.Port, engine, port)
			}
			return
		}
	}
	t.Errorf("model %q not found in %+v", name, models)
}

func TestDiscoverAggregatesAndSkipsUnreachable(t *testing.T) {
	peers := []Peer{
		{Hostname: "self", IP: "100.64.0.1", Online: true},
		{Hostname: "gpu-a", IP: "100.64.0.2", Online: true},
		{Hostname: "gpu-b", IP: "100.64.0.3", Online: true},
		{Hostname: "down", IP: "100.64.0.4", Online: false}, // offline -> filtered
		{Hostname: "noip", IP: "", Online: true},            // no IP -> filtered
		{Hostname: "unreachable", IP: "100.64.0.9", Online: true},
	}
	lister := func(ctx context.Context) ([]Peer, error) { return peers, nil }

	fetch := func(ctx context.Context, ip string) (*nodeStatus, error) {
		switch ip {
		case "100.64.0.2":
			return mkStatus("gpu-a-node", struct {
				name, status string
				port         int
				models       []string
			}{"vllm", "running", 8201, []string{"shared-model", "a-only"}}), nil
		case "100.64.0.3":
			return mkStatus("gpu-b-node", struct {
				name, status string
				port         int
				models       []string
			}{"ollama", "running", 11434, []string{"shared-model"}}), nil
		case "100.64.0.9":
			return nil, errors.New("connection refused")
		default:
			t.Fatalf("unexpected probe to %s (self should be excluded)", ip)
			return nil, nil
		}
	}

	inv, err := discover(context.Background(), lister, fetch, Options{SelfIP: "100.64.0.1"})
	if err != nil {
		t.Fatalf("discover error: %v", err)
	}

	// 3 targets after filtering (self, offline, noip excluded).
	if len(inv.Nodes) != 3 {
		t.Fatalf("expected 3 node results, got %d: %+v", len(inv.Nodes), inv.Nodes)
	}

	// Flattened models: gpu-a has 2, gpu-b has 1 => 3 total.
	if len(inv.Models) != 3 {
		t.Fatalf("expected 3 served models, got %d: %+v", len(inv.Models), inv.Models)
	}

	// shared-model appears on BOTH nodes as distinct entries.
	var sharedNodes []string
	for _, m := range inv.Models {
		if m.Model == "shared-model" {
			sharedNodes = append(sharedNodes, m.Hostname)
		}
	}
	if len(sharedNodes) != 2 {
		t.Errorf("shared-model should appear on 2 nodes, got %v", sharedNodes)
	}

	// unreachable peer recorded with error, not dropped.
	var foundUnreachable bool
	for _, n := range inv.Nodes {
		if n.Hostname == "unreachable" {
			foundUnreachable = true
			if n.Reachable {
				t.Errorf("unreachable node marked reachable")
			}
			if n.Error == "" {
				t.Errorf("unreachable node missing error")
			}
		}
	}
	if !foundUnreachable {
		t.Errorf("unreachable node missing from inventory")
	}

	// Models are sorted deterministically by (model, hostname).
	prev := ""
	for _, m := range inv.Models {
		if m.Model < prev {
			t.Errorf("models not sorted: %q after %q", m.Model, prev)
		}
		prev = m.Model
	}
}

func TestDiscoverListerError(t *testing.T) {
	lister := func(ctx context.Context) ([]Peer, error) { return nil, errors.New("not connected") }
	fetch := func(ctx context.Context, ip string) (*nodeStatus, error) { return nil, nil }
	if _, err := discover(context.Background(), lister, fetch, Options{}); err == nil {
		t.Fatal("expected error when lister fails")
	}
}

func TestDiscoverIncludeOffline(t *testing.T) {
	peers := []Peer{{Hostname: "down", IP: "100.64.0.4", Online: false}}
	lister := func(ctx context.Context) ([]Peer, error) { return peers, nil }
	var probed bool
	fetch := func(ctx context.Context, ip string) (*nodeStatus, error) {
		probed = true
		return mkStatus("down-node"), nil
	}
	if _, err := discover(context.Background(), lister, fetch, Options{IncludeOffline: true}); err != nil {
		t.Fatal(err)
	}
	if !probed {
		t.Error("IncludeOffline should probe offline peers")
	}
}

func TestFindModel(t *testing.T) {
	inv := &Inventory{Models: []ServedModel{
		{Model: "shared", Hostname: "gpu-a", IP: "100.64.0.2", Engine: "vllm", Port: 8201},
		{Model: "shared", Hostname: "gpu-b", IP: "100.64.0.3", Engine: "ollama", Port: 11434},
		{Model: "unique", Hostname: "gpu-a", IP: "100.64.0.2", Engine: "vllm", Port: 8201},
	}}

	// Unique model -> resolves without node.
	m, err := inv.FindModel("", "unique")
	if err != nil || m.Hostname != "gpu-a" {
		t.Fatalf("FindModel(unique) = %+v, %v", m, err)
	}

	// Ambiguous model without node -> error.
	if _, err := inv.FindModel("", "shared"); err == nil {
		t.Error("expected ambiguity error for shared model")
	}

	// Disambiguate by node (hostname).
	m, err = inv.FindModel("gpu-b", "shared")
	if err != nil || m.IP != "100.64.0.3" {
		t.Fatalf("FindModel(gpu-b, shared) = %+v, %v", m, err)
	}

	// Disambiguate by node (IP).
	m, err = inv.FindModel("100.64.0.2", "shared")
	if err != nil || m.Hostname != "gpu-a" {
		t.Fatalf("FindModel(ip, shared) = %+v, %v", m, err)
	}

	// Node with single model -> model optional.
	single := &Inventory{Models: []ServedModel{{Model: "only", Hostname: "n1", IP: "100.64.0.7", Port: 8201}}}
	if m, err := single.FindModel("n1", ""); err != nil || m.Model != "only" {
		t.Fatalf("FindModel(n1, \"\") = %+v, %v", m, err)
	}

	// Substring fallback.
	if m, err := inv.FindModel("gpu-a", "uni"); err != nil || m.Model != "unique" {
		t.Fatalf("FindModel(gpu-a, uni) = %+v, %v", m, err)
	}

	// Unknown node.
	if _, err := inv.FindModel("nope", ""); err == nil {
		t.Error("expected error for unknown node")
	}
}

// TestHTTPFetcherOverInjectedDialer exercises the real HTTP path (httpFetcher)
// against a local httptest server via an injected dialer, proving Discover works
// end-to-end without a mesh.
func TestHTTPFetcherOverInjectedDialer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"node":{"name":"probe-node"},"services":[{"name":"vllm","type":"llm","status":"running","port":8201,"models":["m1"]}]}`)
	}))
	defer srv.Close()
	srvAddr := strings.TrimPrefix(srv.URL, "http://")

	// Dialer ignores the mesh target addr and dials the local test server.
	dialer := func(ctx context.Context, netw, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", srvAddr)
	}

	lister := func(ctx context.Context) ([]Peer, error) {
		return []Peer{{Hostname: "probe", IP: "100.64.0.42", Online: true}}, nil
	}

	inv, err := Discover(context.Background(), lister, dialer, Options{Port: 8080, ProbeTimeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(inv.Models) != 1 || inv.Models[0].Model != "m1" {
		t.Fatalf("expected 1 model m1, got %+v", inv.Models)
	}
	if inv.Models[0].NodeName != "probe-node" || inv.Models[0].Port != 8201 {
		t.Fatalf("unexpected model identity: %+v", inv.Models[0])
	}
	if !inv.Nodes[0].Reachable {
		t.Fatalf("node should be reachable: %+v", inv.Nodes[0])
	}
}
