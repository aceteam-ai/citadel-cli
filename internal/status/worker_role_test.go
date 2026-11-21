package status

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestWorkerRoleWireAndStartupSnapshot(t *testing.T) {
	cfg := CollectorConfig{Role: "control"}
	c := NewCollector(cfg)
	cfg.Role = ""
	if c.statusHeader().Role != "control" {
		t.Fatal("collector did not snapshot the startup role")
	}
	for _, role := range []string{"", "control"} {
		data, err := json.Marshal(NodeStatus{Role: role})
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(data, &wire); err != nil {
			t.Fatal(err)
		}
		value, exists := wire["role"]
		if exists != (role != "") || (exists && value != role) {
			t.Fatalf("role wire=%s", data)
		}
	}
}

func TestWorkerRoleCheapEnvelope(t *testing.T) {
	s := &Server{collector: NewCollector(CollectorConfig{Role: "control", WorkerLiveness: func() *WorkerLiveness { return &WorkerLiveness{Consuming: true} }})}
	w := httptest.NewRecorder()
	s.handleWorker(w, httptest.NewRequest("GET", "/worker", nil))
	var envelope WorkerEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Role != "control" || envelope.Worker == nil || !envelope.Worker.Consuming {
		t.Fatalf("worker envelope=%s", w.Body.String())
	}
}
