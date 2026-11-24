package platform_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestBrowserSigninTrackedDispatchRedactsQuarantinedIdentity(t *testing.T) {
	for _, atStart := range []bool{false, true} {
		t.Run(map[bool]string{false: "after_start", true: "during_start"}[atStart], func(t *testing.T) {
			f := platform.NewDispatchFixture(t, true)
			h := jobs.NewCobrowseSessionHandlerWithManager(f.Manager)
			setSignin := func() {
				f.Set([2]string{"a", "chrome://managed-user-profile-notice/?email=sentinel@example.test#sentinel"})
				f.SetTitle("a", "Sign in as sentinel@example.test")
			}
			if atStart {
				setSignin()
			}
			out, err := dispatch(t, h, "start", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			id := out["id"].(string)
			if !atStart {
				setSignin()
			}
			check := func(out map[string]any) {
				t.Helper()
				raw, _ := json.Marshal(out)
				u, _ := out["url"].(string)
				if strings.Contains(string(raw), "sentinel") || out["interstitial"] != "chrome_signin" || out["active_target"] != "" || u != "" {
					t.Fatalf("quarantined identity or missing marker: %s", raw)
				}
				pages := out["targets"].([]any)
				page := pages[0].(map[string]any)
				if page["title"] != "" || page["url"] != "chrome://managed-user-profile-notice/" || page["refused"] != true {
					t.Fatalf("unsafe target: %s", raw)
				}
			}
			if atStart {
				check(out)
			}
			for _, action := range []string{"status", "handoff", "resume"} {
				out, err = dispatch(t, h, action, id, nil)
				if err != nil {
					t.Fatal(err)
				}
				check(out)
			}
			snap, err := f.Manager.Targets(id)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(snap)
			if strings.Contains(string(raw), "sentinel") {
				t.Fatalf("viewer target leak: %s", raw)
			}
			_, err = dispatch(t, h, "screenshot", id, nil)
			if !errors.Is(err, platform.ErrPageRefused) {
				t.Fatalf("account page fence weakened: %v", err)
			}
		})
	}
}
