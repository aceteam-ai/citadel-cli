package platform_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowsestream"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func dispatch(t *testing.T, h *jobs.CobrowseSessionHandler, action, id string, fields map[string]string) (map[string]any, error) {
	t.Helper()
	p := map[string]string{"action": action, "session_id": id}
	for k, v := range fields {
		p[k] = v
	}
	out, err := h.Execute(jobs.JobContext{}, &nexus.Job{ID: "fake-dispatch", Type: "COBROWSE_SESSION", Payload: p})
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err = json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	return result, nil
}

func startDispatch(t *testing.T, tracking bool) (*platform.DispatchFixture, *jobs.CobrowseSessionHandler, string) {
	t.Helper()
	f := platform.NewDispatchFixture(t, tracking)
	h := jobs.NewCobrowseSessionHandlerWithManager(f.Manager)
	out, err := dispatch(t, h, "start", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := out["id"].(string)
	if tracking {
		assertPages(t, out, "a", 1)
	}
	return f, h, id
}

func assertPages(t *testing.T, out map[string]any, active string, n int) {
	t.Helper()
	pages, ok := out["targets"].([]any)
	if !ok || out["active_target"] != active || out["open_targets"] != float64(n) || len(pages) != n {
		t.Fatalf("page snapshot: %+v", out)
	}
	for _, item := range pages {
		p := item.(map[string]any)
		for _, key := range []string{"id", "title", "url", "active", "refused", "opened_by"} {
			if _, ok := p[key]; !ok {
				t.Fatalf("missing %s in %+v", key, p)
			}
		}
	}
}

func TestTargetSetRemoteDispatchPopupReadsAssertionsAndReturn(t *testing.T) {
	f, h, id := startDispatch(t, true)
	adapter := cobrowsestream.NewManagerSessionFrom(f.Manager)
	targetSession, ok := adapter.(cobrowsestream.TargetSession)
	if !ok {
		t.Fatal("no target session")
	}
	if !adapter.(interface{ TracksTargets(string) bool }).TracksTargets(id) {
		t.Fatal("tracking lost")
	}
	f.Set([2]string{"a", "https://app.test/"}, [2]string{"p", "https://login.test/"})
	out, err := dispatch(t, h, "status", id, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertPages(t, out, "p", 2)
	pages := out["targets"].([]any)
	if pages[0].(map[string]any)["opened_by"] != "start" || pages[1].(map[string]any)["opened_by"] != "page" {
		t.Fatalf("provenance: %+v", pages)
	}
	snap, err := targetSession.Targets(id)
	if err != nil || snap.Active != "p" || !strings.HasSuffix(snap.ActiveWebSocketURL, "/p") {
		t.Fatalf("stream follows popup: %+v %v", snap, err)
	}
	for _, action := range []string{"screenshot", "extract"} {
		out, err = dispatch(t, h, action, id, map[string]string{"target_id": "a", "selector": "body"})
		if err != nil {
			t.Fatal(err)
		}
		assertPages(t, out, "p", 2)
		if out["target_id"] != "a" {
			t.Fatalf("read target: %+v", out)
		}
		if action == "extract" && out["text"] != "a" {
			t.Fatalf("read popup instead of background: %+v", out)
		}
		if action == "screenshot" && out["url"] != "https://app.test/" {
			t.Fatalf("screenshot URL: %+v", out)
		}
	}
	for _, call := range f.Calls("p") {
		if strings.Contains(call, "captureScreenshot") {
			t.Fatal("background read hit popup")
		}
	}
	_, err = dispatch(t, h, "type", id, map[string]string{"target_id": "a", "text": "must not type"})
	if !errors.Is(err, platform.ErrTargetChanged) {
		t.Fatalf("stale assertion: %v", err)
	}
	for _, action := range []string{"handoff", "resume"} {
		_, err = dispatch(t, h, action, id, map[string]string{"target_id": "a"})
		if !errors.Is(err, platform.ErrTargetChanged) {
			t.Fatalf("%s ignored stale target: %v", action, err)
		}
	}
	f.Set([2]string{"a", "https://app.test/"})
	out, err = dispatch(t, h, "type", id, map[string]string{"target_id": "a", "text": "returned"})
	if err != nil {
		t.Fatal(err)
	}
	assertPages(t, out, "a", 1)
	snap, err = targetSession.Targets(id)
	if err != nil || snap.Active != "a" {
		t.Fatalf("stream return: %+v %v", snap, err)
	}
}

func TestTargetSetRemoteDispatchEveryActionHasPageMetadata(t *testing.T) {
	_, h, id := startDispatch(t, true)
	for _, tc := range []struct {
		action string
		fields map[string]string
	}{
		{"navigate", map[string]string{"url": "https://app.test/", "target_id": "a"}},
		{"click", map[string]string{"selector": "button", "target_id": "a"}},
		{"type", map[string]string{"text": "safe", "target_id": "a"}},
		{"select", map[string]string{"selector": "select", "value": "v", "target_id": "a"}},
		{"key", map[string]string{"key": "Enter", "target_id": "a"}},
		{"scroll", map[string]string{"x": "1", "y": "1", "dx": "0", "dy": "2", "target_id": "a"}},
		{"handoff", nil}, {"resume", nil}, {"stop", nil},
	} {
		t.Run(tc.action, func(t *testing.T) {
			out, err := dispatch(t, h, tc.action, id, tc.fields)
			if err != nil {
				t.Fatal(err)
			}
			if tc.action == "stop" {
				assertPages(t, out, "", 0)
			} else {
				assertPages(t, out, "a", 1)
			}
		})
	}
}

func TestTargetSetRemoteDispatchRejectsPopupRaceBeforeInput(t *testing.T) {
	f, h, id := startDispatch(t, true)
	f.OnCall(func(_ string, method string) {
		if method == "Runtime.evaluate" {
			f.Set([2]string{"a", "https://app.test/"}, [2]string{"p", "https://login.test/"})
		}
	})
	_, err := dispatch(t, h, "click", id, map[string]string{"selector": "button", "target_id": "a"})
	if !errors.Is(err, platform.ErrTargetChanged) {
		t.Fatalf("raced click: %v", err)
	}
	for _, page := range []string{"a", "p"} {
		for _, call := range f.Calls(page) {
			if strings.HasPrefix(call, "Input.") {
				t.Fatalf("input after race on %s: %v", page, call)
			}
		}
	}
}

func TestTargetSetRemoteDispatchReadDiscardsPolicyRace(t *testing.T) {
	f, h, id := startDispatch(t, true)
	f.Set([2]string{"a", "https://app.test/"}, [2]string{"p", "https://login.test/"})
	f.OnCall(func(page, method string) {
		if page == "a" && method == "Runtime.evaluate" {
			f.Set([2]string{"a", "http://169.254.169.254/"}, [2]string{"p", "https://login.test/"})
		}
	})
	out, err := dispatch(t, h, "extract", id, map[string]string{"target_id": "a", "selector": "body"})
	if err == nil || out != nil {
		t.Fatalf("read returned data after refusal: %+v %v", out, err)
	}
}

func TestTargetSetRemoteDispatchBoundPolicyAndProvenance(t *testing.T) {
	f, h, id := startDispatch(t, true)
	var pages [][2]string
	pages = append(pages, [2]string{"a", "https://app.test/"})
	for i := 0; i < 12; i++ {
		pages = append(pages, [2]string{fmt.Sprintf("n%d", i), "https://app.test/"})
	}
	pages = append(pages, [2]string{"private", "http://127.0.0.1/"})
	f.Set(pages...)
	out, err := dispatch(t, h, "status", id, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertPages(t, out, "a", platform.MaxCobrowseTargets)
	for _, p := range out["targets"].([]any) {
		if p.(map[string]any)["id"] == "private" {
			t.Fatal("private page allowed")
		}
	}
	for _, by := range []string{"agent", "user"} {
		if err = f.Manager.RecordTargetOpenedBy(id, "n0", by); err != nil {
			t.Fatal(err)
		}
		snap, _ := f.Manager.Targets(id)
		if snap.Targets[1].OpenedBy != by {
			t.Fatalf("opener: %+v", snap)
		}
	}
	if err = f.Manager.RecordTargetOpenedBy(id, "nonexistent", "agent"); err == nil {
		t.Fatal("unbounded phantom provenance accepted")
	}
	for _, raw := range []string{"http://127.0.0.1/", "http://169.254.169.254/", "file:///etc/passwd"} {
		_, err = dispatch(t, h, "navigate", id, map[string]string{"url": raw})
		if err == nil {
			t.Fatalf("navigation allowed: %s", raw)
		}
		_, err = dispatch(t, h, "start", "", map[string]string{"url": raw})
		if err == nil {
			t.Fatalf("start allowed: %s", raw)
		}
	}
}

func TestTargetSetRemoteDispatchLegacySessionKeepsSinglePage(t *testing.T) {
	f, h, id := startDispatch(t, false)
	// Enabling a manager for future launches must not retrofit existing sessions.
	f.Manager.EnableTargetTracking(nil, nil)
	f.Set([2]string{"a", "https://app.test/"}, [2]string{"p", "https://login.test/"})
	out, err := dispatch(t, h, "status", id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["targets"]; ok {
		t.Fatalf("legacy status changed: %+v", out)
	}
	if f.Manager.SessionTargetsEnabled(id) {
		t.Fatal("legacy retrofitted")
	}
	adapter := cobrowsestream.NewManagerSessionFrom(f.Manager)
	if adapter.(interface{ TracksTargets(string) bool }).TracksTargets(id) {
		t.Fatal("legacy viewer gained target frames")
	}
	_, err = dispatch(t, h, "type", id, map[string]string{"text": "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Calls("p")) != 0 {
		t.Fatal("legacy action went to popup")
	}
	out, err = dispatch(t, h, "screenshot", id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["target_id"]; ok {
		t.Fatalf("legacy screenshot gained empty target_id: %+v", out)
	}
	for _, action := range []string{"type", "screenshot", "extract"} {
		_, err = dispatch(t, h, action, id, map[string]string{"text": "no", "selector": "body", "target_id": "p"})
		if !errors.Is(err, platform.ErrTargetsDisabled) {
			t.Fatalf("legacy silently ignored %s target: %v", action, err)
		}
	}
}

func TestTargetSetRemoteDispatchCapabilitiesAndStatusList(t *testing.T) {
	_, h, id := startDispatch(t, true)
	out, err := dispatch(t, h, "capabilities", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	features, ok := out["features"].([]any)
	if !ok || len(features) != 1 || features[0] != "tabs" {
		t.Fatalf("capability report: %+v", out)
	}
	out, err = dispatch(t, h, "status", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	list := out["sessions"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != id {
		t.Fatalf("session list: %+v", list)
	}
	assertPages(t, list[0].(map[string]any), "a", 1)
}

// This connects the real dispatched manager adapter to the real viewer handler.
// All browser discovery and CDP traffic still terminates at the fake endpoint.
func TestTargetSetRemoteDispatchViewerFollowsAndChoosesPages(t *testing.T) {
	f, h, id := startDispatch(t, true)
	viewer := cobrowsestream.NewStreamHandler(cobrowsestream.Config{}, cobrowsestream.NewManagerSessionFrom(f.Manager), func(*http.Request) bool { return true })
	viewer.SetSilent()
	server := httptest.NewServer(viewer)
	t.Cleanup(func() {
		viewer.CloseViewers()
		server.Close()
		deadline := time.Now().Add(3 * time.Second)
		for viewer.ActiveConnections() != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if viewer.ActiveConnections() != 0 {
			t.Error("viewer did not exit")
		}
	})
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/?id="+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	read := func(active string) {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil && m["type"] == "targets" && m["active"] == active {
				return
			}
		}
	}
	read("a")
	f.Set([2]string{"a", "https://app.test/"}, [2]string{"p", "https://login.test/"})
	read("p")
	out, err := dispatch(t, h, "type", id, map[string]string{"target_id": "p", "text": "popup value"})
	if err != nil {
		t.Fatal(err)
	}
	assertPages(t, out, "p", 2)
	f.Set([2]string{"a", "https://app.test/"})
	read("a")
	f.Set([2]string{"a", "https://app.test/"}, [2]string{"p", "https://login.test/"})
	read("p")
	if _, err = dispatch(t, h, "handoff", id, nil); err != nil {
		t.Fatal(err)
	}
	if err = conn.WriteJSON(map[string]string{"type": "target_switch", "target": "a"}); err != nil {
		t.Fatal(err)
	}
	read("a")
	if err = conn.WriteJSON(map[string]string{"type": "target_close", "target": "p"}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Type    string `json:"type"`
			Targets []any  `json:"targets"`
		}
		if json.Unmarshal(raw, &m) == nil && m.Type == "targets" && len(m.Targets) == 1 {
			break
		}
	}
}
