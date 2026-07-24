package cmd

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/worklock"
)

func TestNoWorkerMessage(t *testing.T) {
	out := noWorkerMessage()
	for _, want := range []string{"No citadel worker is running", "citadel work"} {
		if !strings.Contains(out, want) {
			t.Errorf("noWorkerMessage missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderServicesSection_Empty(t *testing.T) {
	if got := renderServicesSection(nil); got != "" {
		t.Errorf("renderServicesSection(nil) = %q, want empty", got)
	}
	if got := renderServicesSection([]attachService{}); got != "" {
		t.Errorf("renderServicesSection(empty) = %q, want empty", got)
	}
}

func TestRenderServicesSection_SortedAndFormatted(t *testing.T) {
	// Deliberately unsorted input to prove the section is name-sorted for a stable
	// view regardless of the daemon's roster order.
	services := []attachService{
		{Name: "vllm", Status: "running", Health: "healthy", Port: 8000},
		{Name: "bonsai", Status: "stopped"},
	}
	out := renderServicesSection(services)

	if !strings.Contains(out, "Managed services:") {
		t.Fatalf("missing header\n---\n%s", out)
	}
	// bonsai must render before vllm (name-sorted).
	bi := strings.Index(out, "bonsai")
	vi := strings.Index(out, "vllm")
	if bi < 0 || vi < 0 || bi > vi {
		t.Errorf("services not name-sorted (bonsai before vllm)\n---\n%s", out)
	}
	for _, want := range []string{"bonsai", "stopped", "vllm", "running", "(healthy)", ":8000"} {
		if !strings.Contains(out, want) {
			t.Errorf("services section missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderServicesSection_Fallbacks(t *testing.T) {
	// An empty name/status must degrade to placeholders, never render a blank cell.
	out := renderServicesSection([]attachService{{}})
	for _, want := range []string{"(unnamed)", "unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("services section missing fallback %q\n---\n%s", want, out)
		}
	}
	// No port/health -> no ":" port suffix or parenthetical health.
	if strings.Contains(out, ":0") {
		t.Errorf("zero port should be omitted\n---\n%s", out)
	}
}

func TestBuildAttachView_BannerPlusServices(t *testing.T) {
	start := time.Date(2026, 7, 15, 9, 12, 44, 0, time.UTC)
	now := start.Add(90 * time.Minute)
	holder := worklock.HolderRecord{PID: 4242, StartTime: start, Version: "2.87.0"}
	st := &attachStatus{Version: "2.87.0", NodeName: "gpu-1297", Health: "ok"}
	services := []attachService{{Name: "vllm", Status: "running", Port: 8000}}

	out := buildAttachView(holder, now, st, services)

	// Banner content (reused from work_attach.go) is present...
	for _, want := range []string{"already running", "PID 4242", "Node: gpu-1297 (ok)"} {
		if !strings.Contains(out, want) {
			t.Errorf("attach view missing banner element %q\n---\n%s", want, out)
		}
	}
	// ...followed by the services section.
	if !strings.Contains(out, "Managed services:") || !strings.Contains(out, "vllm") {
		t.Errorf("attach view missing services section\n---\n%s", out)
	}
}

func TestBuildAttachView_NoServicesOmitsSection(t *testing.T) {
	holder := worklock.HolderRecord{PID: 7, StartTime: time.Now().Add(-time.Minute), Version: "1.0.0"}
	out := buildAttachView(holder, time.Now(), nil, nil)
	if strings.Contains(out, "Managed services:") {
		t.Errorf("attach view should omit services header when none are known\n---\n%s", out)
	}
	// The banner must still render.
	if !strings.Contains(out, "already running") {
		t.Errorf("attach view missing banner\n---\n%s", out)
	}
}

// testServerPort extracts the numeric port from an httptest.Server URL so the
// loopback probe helpers (which build a 127.0.0.1:<port> URL) can target it.
func testServerPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}
	return port
}

func TestProbeLocalServices_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"services":[{"name":"vllm","status":"running","health":"healthy","port":8000},{"name":"bonsai","status":"stopped"}]}`))
	}))
	defer srv.Close()

	services, ok := probeLocalServices(testServerPort(t, srv))
	if !ok {
		t.Fatal("probeLocalServices ok=false, want true")
	}
	if len(services) != 2 {
		t.Fatalf("got %d services, want 2", len(services))
	}
	// Verify field decoding on the first entry.
	if services[0].Name != "vllm" || services[0].Status != "running" || services[0].Port != 8000 {
		t.Errorf("unexpected first service: %+v", services[0])
	}
}

func TestProbeLocalServices_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, ok := probeLocalServices(testServerPort(t, srv)); ok {
		t.Error("probeLocalServices ok=true on 401, want false")
	}
}

func TestProbeLocalServices_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	if _, ok := probeLocalServices(testServerPort(t, srv)); ok {
		t.Error("probeLocalServices ok=true on malformed JSON, want false")
	}
}

func TestProbeLocalServices_ZeroPort(t *testing.T) {
	if _, ok := probeLocalServices(0); ok {
		t.Error("probeLocalServices(0) ok=true, want false")
	}
}
