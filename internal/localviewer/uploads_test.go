package localviewer

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse/fakebrowser"
)

func TestViewerUploadsStateFollowsLiveGate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "local")
	settings := localcobrowse.NewUploadSettings(dir, "")
	svc := localcobrowse.New(localcobrowse.Config{Browser: fakebrowser.New(), Uploads: settings.Status})
	srv := New(Config{Service: svc})
	for _, value := range []string{"off", "on"} {
		if err := localcobrowse.WriteUploadSetting(dir, value); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		srv.handleState(w, httptest.NewRequest("GET", "/api/state", nil))
		var st State
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		if st.Uploads.Enabled != (value == "on") || st.Uploads.Reason == "" || strings.Contains(w.Body.String(), dir) {
			t.Fatalf("state: %s", w.Body.String())
		}
	}
}
