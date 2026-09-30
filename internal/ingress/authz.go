package ingress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
)

// trustHeaderPrefixes and trustHeaderExact enumerate the headers a client must
// never be able to supply -- the ingress deletes every one of them from an
// inbound request before the pod sees it, then sets only the ones it vouches
// for (see setForwardHeaders). This is the same reasoning internal/gateway uses
// when it deletes X-Ingress-Path before conditionally re-setting it: a trust
// header is a statement the ingress makes, so a client-supplied copy is a spoof.
var trustHeaderPrefixes = []string{
	"X-Forwarded-",
	"X-Ingress-",
	"X-Citadel-",
	"X-Auth-",
}

var trustHeaderExact = []string{
	"Forwarded",
	"X-Real-Ip", // canonical MIME form of X-Real-IP
}

// platformSessionCookieExact names the platform session cookies that do not fit
// the sb-<ref>-auth-token pattern. Together with isPlatformSessionCookie's broad
// prefix test, this is the ingress's OWN authoritative source of truth for the
// platform session-cookie family: it is stripped from every forwarded request
// regardless of what the routes payload supplies, so an empty or missing
// session_cookies field can never leave a session credential exposed to a pod.
// The payload's session_cookies only AUGMENTS this set.
var platformSessionCookieExact = []string{
	"sb-access-token",     // legacy single access-token cookie
	"sb-refresh-token",    // legacy single refresh-token cookie
	"supabase-auth-token", // legacy combined auth cookie
}

const (
	// AppCredentialCookieName is the host-only credential minted by the control
	// plane for an app host. AppNonceCookieName binds the one-time browser
	// handoff to the browser that initiated it. Neither may ever reach a pod.
	AppCredentialCookieName = "__Host-ace_app"
	AppNonceCookieName      = "__Host-ace_app_nonce"
)

// isPlatformSessionCookie reports whether a cookie name belongs to the platform
// session family. It matches the Supabase auth-token family broadly -- the base
// cookie sb-<ref>-auth-token, every chunk suffix (.0, .1, ...) a large session
// splits into, and the PKCE code-verifier -- via an sb- prefix + auth-token
// substring test, then falls back to the exact legacy names. A broad predicate
// is deliberate (fail-closed toward stripping a session cookie) while genuinely
// unrelated cookies (sess, theme, an app's own id) match neither test.
func isPlatformSessionCookie(name string) bool {
	decoded := name
	if unescaped, err := url.PathUnescape(name); err == nil {
		decoded = unescaped
	}
	lower := strings.ToLower(decoded)
	for _, prefix := range []string{"__secure-", "__host-"} {
		if strings.HasPrefix(lower, prefix) {
			lower = strings.TrimPrefix(lower, prefix)
			break
		}
	}
	if strings.HasPrefix(lower, "sb-") && strings.Contains(lower, "auth-token") {
		return true
	}
	for _, e := range platformSessionCookieExact {
		if lower == e {
			return true
		}
	}
	return false
}

// stripInbound removes every trust header and every session cookie (the hardcoded
// platform family, ALWAYS, plus any augment name) from the request before it is
// forwarded to the pod. It removes trust-header tokens from Connection so a
// client cannot nominate an ingress-owned header for ReverseProxy to delete
// after the ingress sets it. Other hop-by-hop tokens, notably Upgrade for a
// WebSocket handshake, survive. Unrelated cookies survive.
func stripInbound(r *http.Request, augment []string) {
	for key := range r.Header {
		if isTrustHeader(key) {
			r.Header.Del(key)
		}
	}
	sanitizeConnectionHeader(r.Header)
	stripCookies(r, augment)
}

// sanitizeConnectionHeader removes only tokens that name trust headers. Go's
// ReverseProxy strips every header nominated by Connection after Director has
// run, so leaving these client-controlled tokens in place would let a client
// erase the ingress-owned values set by setForwardHeaders. Non-trust tokens are
// preserved, including Upgrade, which is required for WebSocket proxying.
func sanitizeConnectionHeader(h http.Header) {
	values := h.Values("Connection")
	if len(values) == 0 {
		return
	}

	h.Del("Connection")
	for _, value := range values {
		var kept []string
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token == "" || isTrustHeader(token) {
				continue
			}
			kept = append(kept, token)
		}
		if len(kept) > 0 {
			h.Add("Connection", strings.Join(kept, ", "))
		}
	}
}

func isTrustHeader(key string) bool {
	for _, p := range trustHeaderPrefixes {
		if strings.HasPrefix(strings.ToLower(key), strings.ToLower(p)) {
			return true
		}
	}
	for _, e := range trustHeaderExact {
		if strings.EqualFold(key, e) {
			return true
		}
	}
	return false
}

// stripCookies drops every platform session cookie (the authoritative hardcoded
// family, ALWAYS -- see isPlatformSessionCookie) plus any augment name from the
// request's Cookie header, then re-serializes the remainder. The Cookie header is
// a single `;`-joined value, so a naive Header.Del would remove ALL cookies; this
// parses, filters, and rebuilds, deleting the header entirely when nothing
// remains. augment (the routes payload's session_cookies, or the configured
// default) only ADDS names -- it can never be required for isolation, so an empty
// augment still strips the platform family. Unrelated cookies survive.
func stripCookies(r *http.Request, augment []string) {
	cookies := r.Cookies()
	if len(cookies) == 0 {
		return
	}
	drop := make(map[string]struct{}, len(augment))
	for _, n := range augment {
		drop[n] = struct{}{}
	}
	kept := cookies[:0]
	for _, ck := range cookies {
		if _, ok := drop[ck.Name]; ok {
			continue
		}
		if isPlatformSessionCookie(ck.Name) {
			continue
		}
		kept = append(kept, ck)
	}
	r.Header.Del("Cookie")
	if len(kept) == 0 {
		return
	}
	parts := make([]string, 0, len(kept))
	for _, ck := range kept {
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	r.Header.Set("Cookie", strings.Join(parts, "; "))
}

// outboundSessionCookieLeaked reports whether any platform session cookie
// survived stripInbound and would reach the pod. It is the fail-closed backstop
// for the session-isolation invariant: once the ingress has read a session
// cookie for its own authz decision, that cookie must never reach the pod, so a
// request whose outbound Cookie header still carries one is refused rather than
// forwarded (see Proxy.ServeHTTP). It covers both the hardcoded platform family
// and the active feed/config augment so newly introduced credentials fail
// closed if stripping ever regresses.
func outboundSessionCookieLeaked(r *http.Request, augment []string) bool {
	drop := cookieNameSet(augment)
	for _, ck := range r.Cookies() {
		if isPlatformSessionCookie(ck.Name) || cookieNameInSet(ck.Name, drop) {
			return true
		}
	}
	return false
}

func cookieNameSet(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = struct{}{}
		}
	}
	return set
}

func cookieNameInSet(name string, set map[string]struct{}) bool {
	if _, ok := set[name]; ok {
		return true
	}
	if unescaped, err := url.PathUnescape(name); err == nil {
		_, ok := set[unescaped]
		return ok
	}
	return false
}

// protectedSetCookie reports whether a pod-originated Set-Cookie attempts to
// replace a platform or configured ingress credential. Malformed values are
// left to net/http/browser parsing; values with no cookie-pair cannot name a
// protected cookie.
func protectedSetCookie(v string, augment []string) bool {
	pair, _, _ := strings.Cut(v, ";")
	name, _, ok := strings.Cut(strings.TrimSpace(pair), "=")
	if !ok {
		return false
	}
	name = strings.TrimSpace(name)
	return isPlatformSessionCookie(name) || cookieNameInSet(name, cookieNameSet(augment))
}

// gatedSessionCookie extracts the session-cookie material a gated app's authz
// check needs, in Cookie-header `name=value; name=value` form. It includes every
// platform-family cookie (so a chunked Supabase token is conveyed whole, and an
// empty routes payload can never blind the gated check) plus any augment-named
// cookie. It deliberately forwards ONLY session cookies, not the whole Cookie
// header, so an app's unrelated cookies are never sent to the control plane. An
// empty result means no session was presented.
func gatedSessionCookie(r *http.Request, augment []string) string {
	keep := make(map[string]struct{}, len(augment))
	for _, n := range augment {
		keep[n] = struct{}{}
	}
	var parts []string
	for _, ck := range r.Cookies() {
		if isPlatformSessionCookie(ck.Name) {
			parts = append(parts, ck.Name+"="+ck.Value)
			continue
		}
		if _, ok := keep[ck.Name]; ok {
			parts = append(parts, ck.Name+"="+ck.Value)
		}
	}
	return strings.Join(parts, "; ")
}

// setForwardHeaders sets the trust headers the ingress vouches for, AFTER
// stripInbound has removed any client-supplied copies. subject (when non-empty)
// is the authenticated identity from a gated authz decision; the pod can trust
// X-Ingress-Subject precisely because inbound copies were deleted first.
func setForwardHeaders(r *http.Request, publicHost, subject string) {
	// ReverseProxy owns X-Forwarded-For and derives it once from RemoteAddr
	// after stripping hop-by-hop headers. Setting it here would make the proxy
	// append the same client IP a second time.
	r.Header.Set("X-Forwarded-Proto", "https")
	if publicHost != "" {
		r.Header.Set("X-Forwarded-Host", publicHost)
	}
	if subject != "" {
		r.Header.Set("X-Ingress-Subject", subject)
	}
}

// rewriteSetCookie strips the Domain= attribute (in any position, any case) so a
// pod cannot set a cookie for the apps domain or its parent, and adds Secure if
// missing. All other attributes are untouched. Pure and table-tested.
func rewriteSetCookie(v string) string {
	parts := strings.Split(v, ";")
	out := make([]string, 0, len(parts))
	hasSecure := false
	for i, p := range parts {
		trimmed := strings.TrimSpace(p)
		lower := strings.ToLower(trimmed)
		if i > 0 {
			name, _, hasValue := strings.Cut(trimmed, "=")
			if hasValue && strings.EqualFold(strings.TrimSpace(name), "domain") {
				continue // drop the Domain attribute
			}
		}
		if lower == "secure" {
			hasSecure = true
		}
		out = append(out, trimmed)
	}
	if !hasSecure {
		out = append(out, "Secure")
	}
	return strings.Join(out, "; ")
}

// Authorizer is the injectable server-to-server gated-app check. The production
// implementation POSTs to the control plane; a fake is used in tests.
type AuthzDecision struct {
	Allow   bool
	Subject string
	Reason  string
}

type Authorizer interface {
	// Authorize reports whether the (slug, session cookie) pair may access the
	// app, and returns the authenticated subject when allowed.
	Authorize(ctx context.Context, slug, cookie string) (AuthzDecision, error)
}

// maxGatedSessionCookieBytes caps the assembled session-cookie material sent to
// the authz endpoint. It matches Node's default --max-http-header-size (16384),
// the real wall the aceteam authz request hits: @supabase/ssr chunks the session
// at ~3180 B/cookie with NO chunk-count cap, so a large multi-chunk session can
// exceed it. Rather than send a doomed request the control plane rejects with an
// opaque 4xx (which the ingress would surface as a confusing 403), an oversize
// session is detected locally and routed to a clean re-login (see ServeHTTP).
const maxGatedSessionCookieBytes = 16384

// decisionKey is the singleflight de-dup key for a gated authz call: sha256 over
// (slug, session cookie) so concurrent identical requests collapse into ONE
// upstream call WITHOUT the raw session token ever being held as an in-memory
// map key. NOTE: the authz verdict is deliberately NOT cached across requests
// (the control plane marks it Cache-Control: no-store); this key exists only to
// dedup in-flight calls, so a revoked or downgraded session loses access on the
// very next request.
func decisionKey(slug, cookie string) string {
	sum := sha256.Sum256([]byte(slug + "\x00" + cookie))
	return hex.EncodeToString(sum[:])
}
