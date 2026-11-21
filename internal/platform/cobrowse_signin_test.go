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
		"url": "chrome://managed-user-profile-notice/?email=sentinel@example.test", "ready": "complete", "title": "sentinel@example.test",
	}, "https://site.example/oauth")
	if st.Interstitial != ChromeSigninInterstitial {
		t.Fatalf("start redirect omitted interstitial: %+v", st)
	}
	assertNoSigninIdentity(t, st)
}

func assertNoSigninIdentity(t *testing.T, st CobrowseSessionStatus) {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil || strings.Contains(string(raw), "sentinel") || st.Title != "" || st.URL != "chrome://managed-user-profile-notice/" {
		t.Fatalf("browser identity escaped: %s (%v)", raw, err)
	}
}

func TestBrowserSigninInterstitialNavigateRedirectRedactedLast(t *testing.T) {
	shortSettle(t, 0)
	m, started, f := startSessionOnFakeCDP(t)
	t.Cleanup(func() { _ = m.StopAll() })
	f.mu.Lock()
	f.evaluateValue = func(string) any {
		return map[string]any{"url": "chrome://managed-user-profile-notice/?email=sentinel@example.test", "ready": "complete", "title": "sentinel@example.test"}
	}
	f.mu.Unlock()
	st, err := m.Navigate(started.ID, "https://site.example/oauth")
	if err != nil || st.Interstitial != ChromeSigninInterstitial {
		t.Fatalf("redirect: %+v %v", st, err)
	}
	assertNoSigninIdentity(t, st)
}

func TestBrowserSigninInterstitialHostVariantsAndSpoofs(t *testing.T) {
	for _, host := range []string{"managed-user-profile-notice", "signin", "chrome-signin", "signin-dice-web-intercept", "sync-confirmation", "signin-error", "signin-email-confirmation", "signin-reauth", "profile-picker", "intro", "history-sync-optin", "profile-customization", "edge-signin", "edge-signin-error", "signin-internals"} {
		for _, scheme := range []string{"chrome", "edge"} {
			for _, suffix := range []string{"", ".top-chrome"} {
				origin := scheme + "://" + host + suffix + "/"
				st := withBrowserInterstitial(CobrowseSessionStatus{URL: origin + "path?email=sentinel@example.test#sentinel", Title: "sentinel"})
				if st.Interstitial != ChromeSigninInterstitial || st.URL != origin || st.Title != "" {
					t.Fatalf("variant: %+v", st)
				}
			}
		}
	}
	for _, raw := range []string{"https://signin/", "chrome://signin.evil/", "chrome://signin.top-chrome.evil/", "chrome://signin:123/", "edge://evil@signin/", "chrome://signin.top-chrome.top-chrome/"} {
		if _, ok := browserSigninURL(raw); ok {
			t.Fatalf("spoof classified: %s", raw)
		}
	}
}

func TestBrowserSigninPageSignalsRejectLateDiscovery(t *testing.T) {
	f := &signalsCaller{text: "sentinel@example.test", pw: true}
	m, s, _ := signalsEnv(t, f, okGate)
	old := pageSignalsConnect
	pageSignalsConnect = func(ctx context.Context, p pin) (cdpCaller, func(), error) {
		// Status was sampled on a site; discovery now selects browser account UI.
		f.frameURL = "chrome://managed-user-profile-notice/?email=sentinel@example.test"
		return old(ctx, p)
	}
	st := m.WithPageSignals(CobrowseSessionStatus{ID: s.id, URL: "https://site.example/"})
	raw, _ := json.Marshal(st)
	if st.PageText != "" || st.PasswordField != nil || strings.Contains(string(raw), "sentinel") || strings.Join(f.calls, ",") != "Page.getFrameTree" {
		t.Fatalf("late browser account page read: %s calls=%v", raw, f.calls)
	}
}

func TestBrowserSigninPageSignalsScriptGuardsBeforeBody(t *testing.T) {
	guard := strings.Index(pageSignalsScript, `location.protocol !== "http:" && location.protocol !== "https:"`)
	body := strings.Index(pageSignalsScript, "document.body")
	if guard < 0 || body < 0 || guard >= body {
		t.Fatal("execution-context protocol guard must precede every body read")
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
