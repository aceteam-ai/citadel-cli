package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestServerInDashboardAndJSON(t *testing.T) {
	data := StatusData{Server: "https://server.example.test"}
	m := StatusModel{data: data, width: 100, height: 80}
	if view := m.View(); !strings.Contains(view, data.Server) {
		t.Fatalf("dashboard omits server: %s", view)
	}
	b, err := json.Marshal(data)
	if err != nil || !strings.Contains(string(b), `"server":"https://server.example.test"`) {
		t.Fatalf("JSON: %s %v", b, err)
	}
}
