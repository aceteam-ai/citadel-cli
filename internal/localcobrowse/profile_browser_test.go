package localcobrowse

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowseprofile"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// realProfileStore adapts cobrowseprofile.Store for the real-browser test.
type realProfileStore struct{ s *cobrowseprofile.Store }

func (r realProfileStore) ValidateName(n string) error { return r.s.ValidateName(n) }
func (r realProfileStore) State() (ProfileVaultState, error) {
	st, err := r.s.State()
	return ProfileVaultState{Configured: st.Configured, Strong: st.Strong}, err
}
func (r realProfileStore) SetPassphrase(p string, ack bool) error { return r.s.SetPassphrase(p, ack) }
func (r realProfileStore) Open(n, p string) (platform.SessionProfile, error) {
	h, err := r.s.Open(n, p)
	if err != nil {
		return nil, err
	}
	return h, nil
}

// TestRealBrowserPersistentProfileRoundTrip is S7's acceptance: with a real
// browser (Windows CI: Chrome and Edge; Linux CI: Chrome), a login cookie set in
// one session survives stop, seal, unlock and a second session. The cookie is
// persistent (Max-Age) and the session stops right after it is set, so the
// browser must flush it before the profile is sealed. The page is a synthetic
// local server; no other site is contacted.
func TestRealBrowserPersistentProfileRoundTrip(t *testing.T) {
	if os.Getenv("CITADEL_COBROWSE_BROWSER_IT") == "" {
		t.Skip("set CITADEL_COBROWSE_BROWSER_IT=1 to run the real-browser saved-logins round trip")
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	cookie := "rt" + hex.EncodeToString(b[:])
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/set":
			http.SetCookie(w, &http.Cookie{Name: "citadel_login", Value: cookie, Path: "/", MaxAge: 86400, HttpOnly: true})
			fmt.Fprint(w, `<!doctype html><h1 id="t">set</h1>`)
		case "/get":
			v := "none"
			if c, err := r.Cookie("citadel_login"); err == nil {
				v = c.Value
			}
			fmt.Fprintf(w, `<!doctype html><h1 id="t">%s</h1>`, v)
		default:
			http.NotFound(w, r)
		}
	}))
	defer page.Close()

	storeDir := t.TempDir()
	store := cobrowseprofile.NewStore(storeDir)
	m := platform.NewCobrowseSessionManager(filepath.Join(t.TempDir(), "sessions"), 2)
	m.SetLaunchOptions(platform.LaunchOptions{Quiet: true})
	defer m.StopAll()
	svc := New(Config{
		Browser:        m,
		Profiles:       realProfileStore{store},
		ReadsPerMinute: 100000,
		URLPolicy:      func(context.Context, string) error { return nil },
	})
	defer svc.Close()
	const pass = "lantern orbit pebble saffron"
	ctx := context.Background()

	waitText := func(id, want string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			res, err := svc.Extract(ctx, id, "#t", nil, 200, "check")
			if err == nil && res.Text == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("page text = %q (%v), want %q", res.Text, err, want)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

	// Session 1: set the cookie, then stop straight away.
	s1, err := svc.Start(ctx, page.URL+"/set", "log in", "rt")
	if err != nil || s1.State != StateLocked {
		t.Fatalf("start 1 = %+v %v", s1, err)
	}
	if err := svc.HumanSetupAndUnlock(ctx, s1.SessionID, pass, true); err != nil {
		t.Fatalf("setup and unlock: %v", err)
	}
	waitText(s1.SessionID, "set")
	st1, _ := m.SessionStatus(s1.SessionID)
	if err := svc.Stop(ctx, s1.SessionID); err != nil {
		t.Fatalf("stop 1: %v", err)
	}
	if st1.Profile != "" {
		if _, err := os.Stat(st1.Profile); !os.IsNotExist(err) {
			t.Fatalf("the plaintext working profile outlived stop: %v", err)
		}
	}
	blob, err := os.ReadFile(filepath.Join(storeDir, "cobrowse-profiles", "rt.enc"))
	if err != nil {
		t.Fatalf("no sealed profile after stop: %v", err)
	}
	if bytes.Contains(blob, []byte(cookie)) {
		t.Fatal("the sealed profile contains the cookie value in plaintext")
	}

	// Session 2: unlock the saved profile; the server sees the cookie.
	s2, err := svc.Start(ctx, page.URL+"/get", "check login", "rt")
	if err != nil || s2.State != StateLocked {
		t.Fatalf("start 2 = %+v %v", s2, err)
	}
	if err := svc.HumanUnlock(ctx, s2.SessionID, "lantern orbit pebble saffrom"); err == nil {
		t.Fatal("a wrong passphrase unlocked the profile")
	}
	if err := svc.HumanUnlock(ctx, s2.SessionID, pass); err != nil {
		t.Fatalf("unlock 2: %v", err)
	}
	waitText(s2.SessionID, cookie)
	if err := svc.Stop(ctx, s2.SessionID); err != nil {
		t.Fatalf("stop 2: %v", err)
	}

	// A throwaway session never sees it.
	s3, err := svc.Start(ctx, page.URL+"/get", "throwaway", "")
	if err != nil {
		t.Fatalf("start 3: %v", err)
	}
	waitText(s3.SessionID, "none")
}
