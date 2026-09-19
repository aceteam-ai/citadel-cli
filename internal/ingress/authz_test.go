package ingress

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStripInbound_RemovesTrustHeadersAndSessionCookiePreservesRest(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://app.apps.example.com/", nil)
	// Trust headers a client must never be able to supply.
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	r.Header.Set("X-Forwarded-Proto", "http")
	r.Header.Set("X-Forwarded-Host", "evil.example.com")
	r.Header.Set("Forwarded", "for=1.2.3.4")
	r.Header.Set("X-Real-IP", "1.2.3.4")
	r.Header.Set("X-Ingress-Subject", "attacker") // spoofed subject
	r.Header.Set("X-Citadel-Node", "spoof")
	r.Header.Set("X-Auth-Token", "spoof")
	// Hop-by-hop headers that MUST survive (WebSocket upgrade depends on them).
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	// A legitimate app header that must survive.
	r.Header.Set("X-App-Feature", "on")
	// Cookies: session cookie is stripped, unrelated cookies survive.
	r.Header.Set("Cookie", "sess=secret; theme=dark; sess2=other")

	stripInbound(r, []string{"sess"})

	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "Forwarded", "X-Real-Ip", "X-Ingress-Subject", "X-Citadel-Node", "X-Auth-Token"} {
		if v := r.Header.Get(h); v != "" {
			t.Errorf("trust header %q survived: %q", h, v)
		}
	}
	if r.Header.Get("Connection") != "Upgrade" || r.Header.Get("Upgrade") != "websocket" {
		t.Error("hop-by-hop upgrade headers must survive stripInbound")
	}
	if r.Header.Get("X-App-Feature") != "on" {
		t.Error("legitimate app header must survive")
	}
	if _, err := r.Cookie("sess"); err == nil {
		t.Error("session cookie 'sess' should have been stripped")
	}
	if c, err := r.Cookie("theme"); err != nil || c.Value != "dark" {
		t.Errorf("unrelated cookie 'theme' should survive: %v", err)
	}
	if c, err := r.Cookie("sess2"); err != nil || c.Value != "other" {
		t.Errorf("unrelated cookie 'sess2' should survive: %v", err)
	}
}

func TestStripCookies_DeletesHeaderWhenEmpty(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://x/", nil)
	r.Header.Set("Cookie", "sess=secret")
	stripInbound(r, []string{"sess"})
	if v := r.Header.Get("Cookie"); v != "" {
		t.Errorf("Cookie header should be gone entirely, got %q", v)
	}
}

func TestIsPlatformSessionCookie(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"sb-projref-auth-token", true},               // base Supabase auth cookie
		{"sb-projref-auth-token.0", true},             // chunk
		{"sb-projref-auth-token.1", true},             // chunk
		{"sb-projref-auth-token.42", true},            // higher chunk
		{"sb-projref-auth-token-code-verifier", true}, // PKCE verifier
		{"SB-PROJREF-AUTH-TOKEN", true},               // case-insensitive
		{"sb-access-token", true},                     // legacy exact
		{"sb-refresh-token", true},                    // legacy exact
		{"supabase-auth-token", true},                 // legacy exact
		{"sess", false},                               // unrelated app session cookie
		{"theme", false},                              // unrelated
		{"sb-feature-flag", false},                    // sb- prefix but not an auth token
		{"my-auth-token", false},                      // has auth-token but not sb- prefix
		{"", false},
	}
	for _, tc := range cases {
		if got := isPlatformSessionCookie(tc.name); got != tc.want {
			t.Errorf("isPlatformSessionCookie(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestStripCookies_StripsPlatformFamilyWithEmptyAugment is the direct unit-level
// version of the invariant-1 no-op bug: with an EMPTY augment list (the routes
// payload's default), the hardcoded platform family -- including chunked cookies
// -- is still stripped, while unrelated cookies survive.
func TestStripCookies_StripsPlatformFamilyWithEmptyAugment(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://x/", nil)
	r.Header.Set("Cookie", "sb-projref-auth-token.0=aaa; sb-projref-auth-token.1=bbb; theme=dark; sess=keep")
	stripInbound(r, nil) // empty augment: only the hardcoded family can drive the strip

	if _, err := r.Cookie("sb-projref-auth-token.0"); err == nil {
		t.Error("chunk .0 should be stripped with an empty augment")
	}
	if _, err := r.Cookie("sb-projref-auth-token.1"); err == nil {
		t.Error("chunk .1 should be stripped with an empty augment")
	}
	if c, err := r.Cookie("theme"); err != nil || c.Value != "dark" {
		t.Errorf("unrelated cookie theme should survive: %v", err)
	}
	if c, err := r.Cookie("sess"); err != nil || c.Value != "keep" {
		t.Errorf("unrelated cookie sess should survive (not named, not platform): %v", err)
	}
}

func TestOutboundSessionCookieLeaked(t *testing.T) {
	leaked := httptest.NewRequest(http.MethodGet, "https://x/", nil)
	leaked.Header.Set("Cookie", "keep=1; sb-projref-auth-token=stillhere")
	if !outboundSessionCookieLeaked(leaked) {
		t.Error("a surviving platform session cookie must be detected as leaked")
	}
	clean := httptest.NewRequest(http.MethodGet, "https://x/", nil)
	clean.Header.Set("Cookie", "keep=1; theme=dark")
	if outboundSessionCookieLeaked(clean) {
		t.Error("unrelated cookies must not be reported as a leak")
	}
}

func TestGatedSessionCookie(t *testing.T) {
	// Chunked Supabase cookie is conveyed whole (header form), plus an augment
	// name, but unrelated cookies are NOT forwarded to the control plane.
	r := httptest.NewRequest(http.MethodGet, "https://x/", nil)
	r.Header.Set("Cookie", "sb-projref-auth-token.0=aaa; sb-projref-auth-token.1=bbb; app-sess=xyz; theme=dark")
	got := gatedSessionCookie(r, []string{"app-sess"})
	for _, want := range []string{"sb-projref-auth-token.0=aaa", "sb-projref-auth-token.1=bbb", "app-sess=xyz"} {
		if !strings.Contains(got, want) {
			t.Errorf("gatedSessionCookie missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "theme") {
		t.Errorf("unrelated cookie must not be forwarded to authz: %q", got)
	}

	// No session present -> empty (triggers the login redirect, no authz allow).
	none := httptest.NewRequest(http.MethodGet, "https://x/", nil)
	none.Header.Set("Cookie", "theme=dark")
	if v := gatedSessionCookie(none, nil); v != "" {
		t.Errorf("gatedSessionCookie with no session material = %q, want empty", v)
	}
}

func TestSetForwardHeaders_UsesHostOnlyForXFF(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://app.apps.example.com/", nil)
	r.RemoteAddr = "203.0.113.7:54321"
	setForwardHeaders(r, "app.apps.example.com", "user-42")
	if got := r.Header.Get("X-Forwarded-For"); got != "203.0.113.7" {
		t.Errorf("X-Forwarded-For = %q, want host only (no port)", got)
	}
	if got := r.Header.Get("X-Forwarded-Proto"); got != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https", got)
	}
	if got := r.Header.Get("X-Forwarded-Host"); got != "app.apps.example.com" {
		t.Errorf("X-Forwarded-Host = %q", got)
	}
	if got := r.Header.Get("X-Ingress-Subject"); got != "user-42" {
		t.Errorf("X-Ingress-Subject = %q, want user-42", got)
	}
}

func TestRewriteSetCookie(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"drop domain mid", "id=abc; Domain=apps.example.com; Path=/; HttpOnly", "id=abc; Path=/; HttpOnly; Secure"},
		{"drop domain case", "id=abc; DOMAIN=.example.com; Path=/", "id=abc; Path=/; Secure"},
		{"drop domain end", "id=abc; Path=/; domain=example.com", "id=abc; Path=/; Secure"},
		{"already secure", "id=abc; Path=/; Secure", "id=abc; Path=/; Secure"},
		{"add secure", "id=abc; Path=/; HttpOnly", "id=abc; Path=/; HttpOnly; Secure"},
		{"no attrs", "id=abc", "id=abc; Secure"},
		{"value with domain-like text untouched", "id=domain=weird; Path=/", "id=domain=weird; Path=/; Secure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rewriteSetCookie(tc.in); got != tc.want {
				t.Errorf("rewriteSetCookie(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDecisionCache_HitAndExpiry(t *testing.T) {
	now := time.Unix(0, 0)
	d := newDecisionCache(time.Second, 8)
	d.now = func() time.Time { return now }
	d.put("app", "cookieval", true, "subj")
	e, ok := d.get("app", "cookieval")
	if !ok || !e.allow || e.subject != "subj" {
		t.Fatalf("expected cached allow: %+v ok=%v", e, ok)
	}
	// Different cookie -> different key -> miss.
	if _, ok := d.get("app", "other"); ok {
		t.Fatal("different cookie should not hit")
	}
	now = now.Add(2 * time.Second)
	if _, ok := d.get("app", "cookieval"); ok {
		t.Fatal("entry should have expired")
	}
}

func TestDeriveAuthzURL(t *testing.T) {
	got, err := DeriveAuthzURL("https://cp.example.com/ingress/routes")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://cp.example.com/ingress/authz" {
		t.Errorf("DeriveAuthzURL = %q", got)
	}
	if strings.Contains(got, "routes") {
		t.Errorf("authz URL still references routes: %q", got)
	}
}
