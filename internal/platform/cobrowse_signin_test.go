package platform

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestBrowserSigninSuppressionAllLaunchModes(t *testing.T) {
	for _, stealth := range []bool{false, true} {
		for _, headless := range []bool{false, true} {
			args := buildChromeArgs(cobrowseLaunchOptions{profileDir: "/restored/profile", stealth: stealth, headless: headless})
			for _, flag := range []string{"--allow-browser-signin=false", "--disable-sync"} {
				if !containsArg(args, flag) || countPrefixArg(args, strings.Split(flag, "=")[0]) != 1 {
					t.Fatalf("stealth=%v headless=%v: missing/overridden %s: %v", stealth, headless, flag, args)
				}
			}
		}
	}
	args := buildXvfbChromeArgs(9222, "/restored/profile", "https://site.example/oauth", LaunchOptions{})
	if !containsArg(args, "--allow-browser-signin=false") || !containsArg(args, "--disable-sync") {
		t.Fatal("Xvfb runner dropped browser-account protection")
	}
}

func TestBrowserSigninInterstitialClassification(t *testing.T) {
	for _, raw := range []string{"chrome://managed-user-profile-notice/", "chrome://signin/", "chrome://sync-confirmation/", "edge://managed-user-profile-notice/"} {
		st := withBrowserInterstitial(CobrowseSessionStatus{URL: raw})
		if st.Interstitial != ChromeSigninInterstitial || st.URL != raw {
			t.Fatalf("%s: %+v", raw, st)
		}
		b, _ := json.Marshal(st)
		if !strings.Contains(string(b), `"interstitial":"chrome_signin"`) {
			t.Fatalf("typed wire field missing: %s", b)
		}
	}
	for _, raw := range []string{"https://managed-user-profile-notice/", "https://site.example/chrome://signin/", "chrome://settings/", "chrome://signin.example/", "", "chrome://user@signin/"} {
		st := withBrowserInterstitial(CobrowseSessionStatus{URL: raw, Interstitial: ChromeSigninInterstitial})
		if st.Interstitial != "" {
			t.Fatalf("misclassified or stale marker: %s", raw)
		}
	}
}

func TestBrowserSigninInterstitialStatusWiring(t *testing.T) {
	m, started, f := startSessionOnFakeCDP(t)
	t.Cleanup(func() { _ = m.StopAll() })
	f.mu.Lock()
	f.listedURL = func() string { return "chrome://managed-user-profile-notice/" }
	f.mu.Unlock()
	if st, ok := m.SessionStatus(started.ID); !ok || st.Interstitial != ChromeSigninInterstitial {
		t.Fatalf("probed status lost typed marker: %+v", st)
	}
	s := m.sessions[started.ID]
	if st := s.statusWithURL(context.Background(), "chrome://sync-confirmation/"); st.Interstitial != ChromeSigninInterstitial {
		t.Fatalf("action status lost typed marker: %+v", st)
	}
	if st := s.statusWithURL(context.Background(), "https://site.example/"); st.Interstitial != "" {
		t.Fatalf("website inherited interstitial marker: %+v", st)
	}
}

func TestBrowserSigninInterstitialStartRedirect(t *testing.T) {
	st, _ := startOnFakeCDPWithURL(t, map[string]any{
		"url": "chrome://managed-user-profile-notice/", "ready": "complete", "title": "Sign in",
	}, "https://site.example/oauth")
	if st.Interstitial != ChromeSigninInterstitial {
		t.Fatalf("start redirect omitted interstitial: %+v", st)
	}
}

func TestBrowserSigninInterstitialDoesNotReadAccountPageText(t *testing.T) {
	f := &signalsCaller{text: "synthetic account identity"}
	m, s, connects := signalsEnv(t, f, okGate)
	pw := true
	st := m.WithPageSignals(CobrowseSessionStatus{ID: s.id, URL: "chrome://managed-user-profile-notice/", PageText: "old page", PasswordField: &pw})
	if st.Interstitial != ChromeSigninInterstitial || *connects != 0 || st.PageText != "" || st.PasswordField != nil {
		t.Fatalf("browser account UI was treated as website content: %+v, connects=%d", st, *connects)
	}
}
