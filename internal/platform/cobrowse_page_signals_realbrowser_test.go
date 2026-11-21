package platform

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const wallPage = `<!doctype html><title>Sign in</title>
<h1>Join now to see</h1><p>Already a member?</p>
<input type="email" name="u"><input type="password" name="p">
<input type="password" id="hidden" style="display:none">`

const ordinaryPage = `<!doctype html><title>Plain</title>
<h1>Quarterly notes</h1><p>Nothing to sign in to.</p>
<input type="password" style="display:none">`

// runPageSignalsRealBrowser checks page_text and password_field against a real
// browser: a wall-like page reports its text and a visible password input, an
// ordinary page reports none (aceteam#13773). The OS-specific tests supply the
// manager.
func runPageSignalsRealBrowser(t *testing.T, m *CobrowseSessionManager) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/plain" {
			fmt.Fprint(w, ordinaryPage)
			return
		}
		fmt.Fprint(w, wallPage)
	}))
	t.Cleanup(srv.Close)

	st, err := m.StartSession(srv.URL + "/wall")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = m.StopAll() })

	var got CobrowseSessionStatus
	waitCond(t, 30*time.Second, "the wall page signals", func() bool {
		cur, ok := m.SessionStatus(st.ID)
		if !ok {
			return false
		}
		got = m.WithPageSignals(cur)
		return got.PasswordField != nil && strings.Contains(got.PageText, "Join now to see")
	})
	if !*got.PasswordField {
		t.Fatalf("wall page: text=%q password=%v", got.PageText, *got.PasswordField)
	}

	if _, err := m.Navigate(st.ID, srv.URL+"/plain"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	waitCond(t, 30*time.Second, "the ordinary page signals", func() bool {
		cur, ok := m.SessionStatus(st.ID)
		if !ok {
			return false
		}
		got = m.WithPageSignals(cur)
		return got.PasswordField != nil && strings.Contains(got.PageText, "Quarterly notes")
	})
	if *got.PasswordField || strings.Contains(got.PageText, "Join now") {
		t.Fatalf("ordinary page: text=%q password=%v", got.PageText, *got.PasswordField)
	}

	// Nothing is read while a human drives.
	if _, err := m.Handoff(st.ID); err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	cur, _ := m.SessionStatus(st.ID)
	if h := m.WithPageSignals(cur); h.PageText != "" || h.PasswordField != nil {
		t.Fatalf("read during handoff: %+v", h)
	}
}
