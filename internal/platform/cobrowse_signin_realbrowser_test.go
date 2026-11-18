package platform

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Seed old sign-in preferences through the real persistent-profile boundary.
// No user account, token or Google endpoint is involved.
type signinSeedProfile struct{}

func (signinSeedProfile) Materialize(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "Default", "Preferences"), []byte(`{"signin":{"allowed":true,"allowed_on_next_startup":true,"allowed_on_next_startup_cache":true}}`), 0o600)
}
func (signinSeedProfile) Persist(string) error { return nil }
func (signinSeedProfile) Close() error         { return nil }

func runBrowserSigninRealBrowser(t *testing.T, m *CobrowseSessionManager) {
	t.Helper()
	m.SetLaunchOptions(LaunchOptions{Quiet: true})
	st, err := m.StartSessionWithProfile("", signinSeedProfile{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.StopAll() })
	scheme := "chrome"
	if st.Browser == "edge" {
		scheme = "edge"
	}
	if _, err := cdpCommand(st.DebugPort, "Page.navigate", map[string]any{"url": scheme + "://prefs-internals/"}); err != nil {
		t.Fatal(err)
	}
	// Query the effective preference through CDP, not merely the launch argv
	// or the seeded file. Missing/unreadable prefs do not count as false.
	waitCond(t, 30*time.Second, "effective browser sign-in refusal after profile restore", func() bool {
		r, err := cdpCommand(st.DebugPort, "Runtime.evaluate", map[string]any{
			"expression":    `(()=>{try{const p=JSON.parse(document.body.innerText);const v=p["signin.allowed"]??p.signin?.allowed;return (v&&typeof v==="object")?v.value:v}catch{return null}})()`,
			"returnByValue": true,
		})
		if err != nil {
			return false
		}
		obj, _ := r["result"].(map[string]any)
		v, ok := obj["value"].(bool)
		return ok && !v
	})
	// Exercise the script itself on browser-owned UI. This also covers the
	// race where navigation follows the frame-tree guard but precedes evaluate.
	r, err := cdpCommand(st.DebugPort, "Runtime.evaluate", map[string]any{"expression": pageSignalsScript, "returnByValue": true})
	if err != nil {
		t.Fatal(err)
	}
	remote, _ := r["result"].(map[string]any)
	if remote["subtype"] != "null" {
		t.Fatal("page signal script read browser-owned UI instead of returning null")
	}
	if _, err := cdpCommand(st.DebugPort, "Page.navigate", map[string]any{"url": scheme + "://version/"}); err != nil {
		t.Fatal(err)
	}
	waitCond(t, 30*time.Second, "independent sync-disable switch", func() bool {
		r, err := cdpCommand(st.DebugPort, "Runtime.evaluate", map[string]any{"expression": `!!document.getElementById("command_line")?.textContent.includes("--disable-sync")`, "returnByValue": true})
		if err != nil {
			return false
		}
		obj, _ := r["result"].(map[string]any)
		return obj["value"] == true
	})
	// A synthetic website SSO redirect and cookie still work. This is not a
	// Google SSO acceptance claim and performs no external network request.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sso" {
			http.Redirect(w, r, "/callback", http.StatusFound)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "site_session", Value: "synthetic", Path: "/"})
		fmt.Fprint(w, "<!doctype html><title>Signed in to site</title>")
	}))
	t.Cleanup(srv.Close)
	if _, err := cdpCommand(st.DebugPort, "Page.navigate", map[string]any{"url": srv.URL + "/sso"}); err != nil {
		t.Fatal(err)
	}
	waitCond(t, 30*time.Second, "website redirect and cookie", func() bool {
		r, err := cdpCommand(st.DebugPort, "Runtime.evaluate", map[string]any{"expression": `location.pathname==="/callback"&&document.cookie.includes("site_session=synthetic")`, "returnByValue": true})
		if err != nil {
			return false
		}
		obj, _ := r["result"].(map[string]any)
		return obj["value"] == true
	})
}
