package platform

import (
	"net/url"
	"strings"
)

// BrowserInterstitial describes browser-owned UI, not website authentication.
type BrowserInterstitial string

// ChromeSigninInterstitial identifies browser account UI. It does not mean a
// website login failed, and must never be interpreted as an authenticated host.
const ChromeSigninInterstitial BrowserInterstitial = "chrome_signin"

// browserSigninURL returns only a browser-owned origin, never account-bearing
// paths, query strings, fragments or titles. Website lookalikes do not match.
func browserSigninURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || (u.Scheme != "chrome" && u.Scheme != "edge") {
		return "", false
	}
	host := strings.ToLower(u.Host)
	switch strings.TrimSuffix(host, ".top-chrome") {
	case "managed-user-profile-notice", "signin", "chrome-signin", "signin-dice-web-intercept", "sync-confirmation", "signin-error",
		"signin-email-confirmation", "signin-reauth", "profile-picker", "intro", "history-sync-optin", "profile-customization",
		"edge-signin", "edge-signin-error", "signin-internals":
		return u.Scheme + "://" + host + "/", true
	}
	return "", false
}

func redactBrowserTarget(t CobrowseTarget) CobrowseTarget {
	t.Interstitial = ""
	if safe, ok := browserSigninURL(t.URL); ok {
		t.URL, t.Title, t.Interstitial = safe, "", ChromeSigninInterstitial
	}
	return t
}

func withBrowserInterstitial(st CobrowseSessionStatus) CobrowseSessionStatus {
	st.Interstitial = ""
	if safe, ok := browserSigninURL(st.URL); ok {
		st.URL = safe
		st.Interstitial = ChromeSigninInterstitial
	}
	// The quarantined last page has no active URL. Report its type without
	// weakening the target fence or selecting that browser-owned page.
	if st.Targets != nil {
		st.Targets = append([]CobrowseTarget(nil), st.Targets...)
	}
	for i, target := range st.Targets {
		target = redactBrowserTarget(target)
		st.Targets[i] = target
		if target.Interstitial != "" && (target.Active || target.Refused) {
			st.Interstitial = target.Interstitial
		}
	}
	if st.Interstitial != "" {
		st.Title, st.PageText, st.PasswordField = "", "", nil
	}
	return st
}
