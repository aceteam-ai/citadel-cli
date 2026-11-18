package platform

import "net/url"

// BrowserInterstitial describes browser-owned UI, not website authentication.
type BrowserInterstitial string

// ChromeSigninInterstitial identifies browser account UI. It does not mean a
// website login failed, and must never be interpreted as an authenticated host.
const ChromeSigninInterstitial BrowserInterstitial = "chrome_signin"

func withBrowserInterstitial(st CobrowseSessionStatus) CobrowseSessionStatus {
	st.Interstitial = ""
	u, err := url.Parse(st.URL)
	if err != nil || u.User != nil || (u.Scheme != "chrome" && u.Scheme != "edge") {
		return st
	}
	switch u.Host {
	case "managed-user-profile-notice", "signin", "chrome-signin", "signin-dice-web-intercept", "sync-confirmation", "signin-error":
		st.Interstitial = ChromeSigninInterstitial
		st.PageText, st.PasswordField = "", nil
	}
	return st
}
