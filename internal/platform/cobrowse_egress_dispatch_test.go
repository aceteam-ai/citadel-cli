package platform_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestTargetSetRemoteEgressDispatchedSession(t *testing.T) {
	f := platform.NewRemoteEgressFixture(t)
	h := jobs.NewCobrowseSessionHandlerWithManager(f.Manager)
	call := func(fields map[string]string) (map[string]any, error) {
		t.Helper()
		out, err := h.Execute(jobs.JobContext{}, &nexus.Job{ID: "fake-guarded-job", Type: "COBROWSE_SESSION", Payload: fields})
		if err != nil {
			return nil, err
		}
		var result map[string]any
		if err = json.Unmarshal(out, &result); err != nil {
			t.Fatal(err)
		}
		return result, nil
	}
	out, err := call(map[string]string{"action": "capabilities"})
	if err != nil {
		t.Fatal(err)
	}
	if out["features"].([]any)[0] != "egress_guard" {
		t.Fatalf("node capability absent: %+v", out)
	}
	out, err = call(map[string]string{"action": "start", "url": "about:blank"})
	if err != nil {
		t.Fatal(err)
	}
	id := out["id"].(string)
	if out["features"].([]any)[0] != "egress_guard" || f.Proxy() == "" {
		t.Fatalf("unguarded remote launch: %+v", out)
	}
	for _, raw := range []string{"http://127.0.0.1/", "http://169.254.169.254/", "http://10.0.0.1/"} {
		if _, err = call(map[string]string{"action": "navigate", "session_id": id, "url": raw}); err == nil {
			t.Fatalf("unsafe dispatched navigation: %s", raw)
		}
	}
	// Browser traffic uses exactly the proxy handed to the remote launcher.
	if got := f.Request("http://public.test/redirect"); got != http.StatusForbidden {
		t.Fatalf("redirect escaped: %d", got)
	}
	if got := f.Request("http://rebind.test/first"); got != http.StatusOK {
		t.Fatalf("public request: %d", got)
	}
	f.Rebind()
	if got := f.Request("http://rebind.test/second"); got != http.StatusForbidden {
		t.Fatalf("rebind escaped: %d", got)
	}
	if got := f.Request("http://127.0.0.1/subresource.js"); got != http.StatusForbidden {
		t.Fatalf("subresource escaped: %d", got)
	}
	if f.Hits() != 2 {
		t.Fatalf("forbidden request reached fixture: hits=%d", f.Hits())
	}
	out, err = call(map[string]string{"action": "status", "session_id": id})
	if err != nil {
		t.Fatal(err)
	}
	if out["egress_private_holds"] != float64(1) {
		t.Fatalf("private hold not observable: %+v", out)
	}
	f.CloseGuard()
	if _, err = call(map[string]string{"action": "extract", "session_id": id, "selector": "body"}); !errors.Is(err, platform.ErrEgressGuardUnavailable) {
		t.Fatalf("unguarded read admitted: %v", err)
	}
}
