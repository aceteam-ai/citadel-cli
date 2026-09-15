package ingress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
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

// stripInbound removes every trust header and every named session cookie from
// the request before it is forwarded to the pod. It deliberately does NOT touch
// hop-by-hop headers (Connection, Upgrade, ...) so a WebSocket upgrade still
// works through the proxy. Unrelated cookies survive.
func stripInbound(r *http.Request, cookieNames []string) {
	for key := range r.Header {
		if isTrustHeader(key) {
			r.Header.Del(key)
		}
	}
	stripCookies(r, cookieNames)
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

// stripCookies drops the named session cookies from the request's Cookie header
// and re-serializes the remainder. The Cookie header is a single `;`-joined
// value, so a naive Header.Del would remove ALL cookies; this parses, filters
// by name, and rebuilds, deleting the header entirely when nothing remains.
func stripCookies(r *http.Request, names []string) {
	if len(names) == 0 {
		return
	}
	drop := make(map[string]struct{}, len(names))
	for _, n := range names {
		drop[n] = struct{}{}
	}
	kept := r.Cookies()[:0]
	for _, ck := range r.Cookies() {
		if _, ok := drop[ck.Name]; ok {
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

// setForwardHeaders sets the trust headers the ingress vouches for, AFTER
// stripInbound has removed any client-supplied copies. subject (when non-empty)
// is the authenticated identity from a gated authz decision; the pod can trust
// X-Ingress-Subject precisely because inbound copies were deleted first.
func setForwardHeaders(r *http.Request, publicHost, subject string) {
	if ip := clientIP(r.RemoteAddr); ip != "" {
		r.Header.Set("X-Forwarded-For", ip)
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if publicHost != "" {
		r.Header.Set("X-Forwarded-Host", publicHost)
	}
	if subject != "" {
		r.Header.Set("X-Ingress-Subject", subject)
	}
}

// clientIP returns just the host part of a RemoteAddr (which is host:port).
// The existing gateway code sets the raw RemoteAddr including the port; for an
// X-Forwarded-For value the port is wrong, so we strip it.
func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
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
		if i > 0 && strings.HasPrefix(lower, "domain=") {
			continue // drop the Domain attribute
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
type Authorizer interface {
	// Authorize reports whether the (slug, session cookie) pair may access the
	// app, and returns the authenticated subject when allowed.
	Authorize(ctx context.Context, slug, cookie string) (allow bool, subject string, err error)
}

// decisionCache memoizes allow decisions per (slug, cookie hash) for a short
// TTL so a burst of requests for one gated app makes at most one authz call per
// TTL window. It is bounded so a cookie-spray cannot grow it without limit, and
// keys on sha256(cookie) so raw session tokens are never held in memory as map
// keys.
type decisionCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	entries    map[string]decisionEntry
	now        func() time.Time
}

type decisionEntry struct {
	allow   bool
	subject string
	expiry  time.Time
}

func newDecisionCache(ttl time.Duration, maxEntries int) *decisionCache {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	if maxEntries <= 0 {
		maxEntries = 4096
	}
	return &decisionCache{
		ttl:        ttl,
		maxEntries: maxEntries,
		entries:    make(map[string]decisionEntry),
		now:        time.Now,
	}
}

func decisionKey(slug, cookie string) string {
	sum := sha256.Sum256([]byte(slug + "\x00" + cookie))
	return hex.EncodeToString(sum[:])
}

func (d *decisionCache) get(slug, cookie string) (decisionEntry, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[decisionKey(slug, cookie)]
	if !ok {
		return decisionEntry{}, false
	}
	if d.now().After(e.expiry) {
		delete(d.entries, decisionKey(slug, cookie))
		return decisionEntry{}, false
	}
	return e, true
}

func (d *decisionCache) put(slug, cookie string, allow bool, subject string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.entries) >= d.maxEntries {
		now := d.now()
		for k, e := range d.entries {
			if now.After(e.expiry) {
				delete(d.entries, k)
			}
		}
		if len(d.entries) >= d.maxEntries {
			for k := range d.entries {
				delete(d.entries, k)
				break
			}
		}
	}
	d.entries[decisionKey(slug, cookie)] = decisionEntry{
		allow:   allow,
		subject: subject,
		expiry:  d.now().Add(d.ttl),
	}
}
