package dashboard

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rivo/tview"
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
	d := TviewDashboard{data: data, nodeInfo: tview.NewTextView()}
	d.updateNodeInfo()
	if view := d.nodeInfo.GetText(true); !strings.Contains(view, data.Server) {
		t.Fatalf("alternate dashboard omits server: %s", view)
	}
}
