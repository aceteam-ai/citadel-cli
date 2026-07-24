// internal/gateway/exposure.go
//
// Service ingress / fabric exposure for the Citadel gateway (issue #598).
//
// This adds a productized way to expose a node's LOCAL service (a dashboard, an
// engine UI, Frigate's web UI, ...) on the fabric through the SAME gateway that
// already terminates TLS on 8443 and rides the tsnet VPN listener — no parallel
// `tailscale serve` path. Every exposed service is served under the dedicated
// /expose/<name>/ namespace and gated by a page-style VISIBILITY LADDER:
//
//	private — only the creator (by tailnet login).
//	org     — any authenticated same-owner mesh peer (org == tailnet owner).
//	link    — a signed, expiring, revocable access token (no mesh identity).
//
// # Why a dedicated namespace (the layering that makes link/org work)
//
// The capability layer (permissionMiddleware -> categoryForRequest) gates the
// builtin and /modules/<prefix>/ routes and FAILS CLOSED (a disabled capability
// or a missing node passcode returns 403/401 BEFORE any later middleware runs).
// A `link` recipient has neither a capability nor the node passcode — that is the
// whole point of a shareable link — so exposed routes must NOT be gated by the
// capability switch. categoryForPath therefore returns "" (always-allowed) for
// /expose/..., and this file's exposureMiddleware is the SOLE gate for it. It
// fails closed for any /expose/<name> that has no registered policy (404), so
// "always-allowed at the capability layer" never means "open".
//
// # Standalone by design
//
// Identity comes from an injected MeshIdentityResolver (mirroring the terminal
// endpoint's #585 resolver), so this package stays decoupled from
// internal/network and unit-testable with a mock. The cmd layer wires
// network.WhoIsPeer. `private` needs the creator's tailnet login, which only the
// backend/MCP caller knows (a locally-run `citadel expose` cannot know a REMOTE
// creator's login) — it is an explicit input on ExposePolicy, and `private` is
// inert end-to-end until aceteam passes it. `link` and `org` function fully in a
// node-only v1.
package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// exposeNamePattern matches a valid exposed-service name: lowercase alphanumeric
// plus internal dashes, no slashes or dots, so a name can never inject a path
// segment or traverse. Mirrors the catalog package's gateway-prefix rule (kept
// local to avoid a gateway->catalog dependency).
var exposeNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ExposeRoutePrefix is the namespace under which EVERY exposed service is served
// on the gateway. It is deliberately distinct from ModuleRoutePrefix
// ("/modules/") so the capability layer can always-allow it (categoryForPath)
// and hand sole gating to exposureMiddleware.
const ExposeRoutePrefix = "/expose/"

// ExposeRoutePath returns the gateway route path for an exposed service name:
// "/expose/<name>". The convention lives here so the gateway (route
// registration) and any out-of-process URL builder agree.
func ExposeRoutePath(name string) string {
	return ExposeRoutePrefix + name
}

// Visibility is the access level of an exposed service. It mirrors the pages
// model exactly (private/org/link).
type Visibility string

const (
	// VisibilityPrivate restricts access to the creator (by tailnet login).
	VisibilityPrivate Visibility = "private"
	// VisibilityOrg allows any authenticated same-owner mesh peer.
	VisibilityOrg Visibility = "org"
	// VisibilityLink allows any caller presenting a valid signed access token.
	VisibilityLink Visibility = "link"
)

// Valid reports whether v is one of the three known visibility levels.
func (v Visibility) Valid() bool {
	switch v {
	case VisibilityPrivate, VisibilityOrg, VisibilityLink:
		return true
	}
	return false
}

// MeshPeerIdentity is the verified identity of a peer that connected over the
// mesh/VPN listener, produced by a MeshIdentityResolver and consumed by
// exposureMiddleware to authorize private/org access without a token.
type MeshPeerIdentity struct {
	// NodeName is the peer's node name (audit log).
	NodeName string
	// LoginName is the peer's tailnet user login (e.g. an email). It is the
	// identity compared against a private exposure's Creator.
	LoginName string
	// SameOwner reports whether the peer belongs to the same tailnet owner/org as
	// this node. It is the gate for `org` visibility.
	SameOwner bool
}

// MeshIdentityResolver resolves an inbound connection's remote address to a
// verified mesh peer identity. Injected via Server.SetMeshResolver so the
// gateway stays standalone and unit-testable (tests pass a MockMeshResolver;
// production wires network.WhoIsPeer through the cmd layer).
type MeshIdentityResolver interface {
	// ResolvePeer resolves remoteAddr ("ip:port") to a verified identity, or an
	// error if the peer cannot be verified. An error is treated as unverified and
	// fails the private/org check closed.
	ResolvePeer(ctx context.Context, remoteAddr string) (*MeshPeerIdentity, error)
}

// MockMeshResolver is a MeshIdentityResolver for tests: it returns a fixed
// identity (or error) regardless of the remote address.
type MockMeshResolver struct {
	Identity *MeshPeerIdentity
	Err      error
}

// ResolvePeer implements MeshIdentityResolver for the mock.
func (m *MockMeshResolver) ResolvePeer(context.Context, string) (*MeshPeerIdentity, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	return m.Identity, nil
}

// ExposePolicy is the visibility policy recorded for one exposed service. It is
// registered via Server.Expose and consulted per request by exposureMiddleware.
type ExposePolicy struct {
	// Visibility is the access level (private/org/link).
	Visibility Visibility
	// Creator is the tailnet login authorized for a `private` exposure. It is an
	// explicit input (only the backend/MCP caller knows the remote creator's
	// login); an empty Creator makes a `private` exposure inert (fails closed).
	Creator string
	// TokenEpoch is bound into every `link` token this exposure mints. Bumping it
	// (re-exposing with an incremented epoch) invalidates all previously issued
	// link tokens for this service — the revoke-all primitive behind "revocable".
	TokenEpoch int
}

// exposePrefixFromPath extracts the exposed-service name from a /expose/<name>
// (or /expose/<name>/...) request path, or "" if the path is not an exposure
// route. Inverse of ExposeRoutePath.
func exposePrefixFromPath(path string) string {
	if !strings.HasPrefix(path, ExposeRoutePrefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, ExposeRoutePrefix)
	if rest == "" {
		return ""
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// SetMeshResolver injects the resolver used to verify a caller's mesh identity
// for private/org exposures. A nil resolver makes every private/org exposure
// fail closed (only `link` works without it). Safe to call before or after Start.
func (s *Server) SetMeshResolver(r MeshIdentityResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meshResolver = r
}

// SetExposeSigningKey sets the per-node secret used to sign/verify `link` access
// tokens. An empty key makes every link token fail verification (fail closed).
// Safe to call before or after Start.
func (s *Server) SetExposeSigningKey(key []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exposeSigningKey = key
}

// SetExposure records (or replaces) the visibility policy for an exposed service.
// It does NOT wire the reverse-proxy route — use Expose for that. Exposed under
// s.mu so a running gateway can gain/lose exposures live.
func (s *Server) SetExposure(name string, policy *ExposePolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exposures == nil {
		s.exposures = make(map[string]*ExposePolicy)
	}
	s.exposures[name] = policy
}

// RemoveExposure deletes an exposure's policy. The middleware then fails closed
// (404) for that name even if its proxy route object still exists.
func (s *Server) RemoveExposure(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.exposures, name)
}

// getExposure returns the policy for name, or nil if not exposed.
func (s *Server) getExposure(name string) *ExposePolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.exposures[name]
}

// Expose wires the /expose/<name> reverse-proxy route to a loopback address AND
// records its visibility policy, atomically from the caller's perspective. It is
// the one call the EXPOSE control path (worker job / CLI) makes to program the
// gateway. name must be a clean slug (lowercase alphanumeric + dash) so it can
// never inject a path segment.
func (s *Server) Expose(name, address string, policy *ExposePolicy) error {
	if name == "" {
		return fmt.Errorf("gateway: Expose requires a non-empty service name")
	}
	if !exposeNamePattern.MatchString(name) {
		return fmt.Errorf("gateway: expose name %q must be lowercase alphanumeric and dashes only", name)
	}
	if policy == nil || !policy.Visibility.Valid() {
		return fmt.Errorf("gateway: Expose requires a valid visibility (private|org|link)")
	}
	// StripPrefix so the exposed app's own paths (/, /assets, ...) map through
	// unchanged, exactly like a module route. WebSocket is enabled because a real
	// web app (Frigate live view, dashboards) upgrades to WS; registerProxy routes
	// plain HTTP and WS upgrades through the same handler.
	//
	// NOTE (subpath constraint): because the route is served under /expose/<name>/
	// with StripPrefix, an exposed app that emits ABSOLUTE asset paths (<script
	// src="/assets/...">) will have the browser resolve them at the gateway root
	// (no /expose/<name> prefix) and 404 — for EVERY visibility level, this is not
	// an auth issue. Such an app must be configured to serve under a base path
	// matching the expose prefix (e.g. Frigate's base-path config, handled by the
	// #597 nvr module). Apps that use relative paths work unchanged.
	s.wireExposeRoute(ExposeRoutePath(name), address)
	s.SetExposure(name, policy)
	return nil
}

// wireExposeRoute registers (or re-points) the /expose/<name> reverse-proxy
// route with StripPrefix + WebSocket enabled. It mirrors WireModuleRoute's
// re-wire-safe semantics (#449): a new route created after Start registers its
// handler live; an existing route has its address mutated in place (never
// replaced) so the running proxy keeps reading the current target.
func (s *Server) wireExposeRoute(prefix, address string) {
	s.mu.Lock()
	up, ok := s.config.Upstreams[prefix]
	if !ok {
		up = &Upstream{StripPrefix: true, WebSocket: true}
		up.setAddr(address)
		s.config.Upstreams[prefix] = up
		if s.started {
			s.registerProxy(prefix, up)
		}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	up.setAddr(address)
}

// explicitLinkToken extracts a `link` access token presented EXPLICITLY (header
// or query) — the shareable-link entry point. The header is primary; the query
// parameter is the fallback for plain browser navigation (a shared link is
// opened in a browser, which cannot set a custom header). The cookie fallback is
// handled separately so the middleware can set the cookie on an explicit hit.
func explicitLinkToken(r *http.Request) string {
	if h := r.Header.Get("X-Citadel-Access"); h != "" {
		return h
	}
	return r.URL.Query().Get("access_token")
}

// exposeCookieName is the per-exposure cookie that carries a validated link
// token forward to sub-resource requests. It is per-name so one exposure's
// session cannot authorize another, and it is path-scoped to /expose/<name>/.
func exposeCookieName(name string) string {
	return "citadel_expose_" + name
}

// setLinkCookie plants the validated token as a path-scoped session cookie so a
// browser's subsequent sub-resource fetches (which carry no ?access_token=) stay
// authorized. The cookie VALUE is the same signed token, so every subsequent
// request is re-verified (including the token's own expiry) — the cookie widens
// nothing. Secure + HttpOnly + SameSite=Lax; no Max-Age (session cookie), since
// the token's embedded expiry is the real bound.
func setLinkCookie(w http.ResponseWriter, name, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     exposeCookieName(name),
		Value:    token,
		Path:     ExposeRoutePath(name) + "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// MintLinkToken produces a signed access token for a `link` exposure that is
// valid until exp. The token binds the service name and the exposure's TokenEpoch
// so (a) a token for one service cannot open another and (b) bumping the epoch
// revokes every outstanding token. Format: base64url(payload)"."base64url(hmac),
// payload = "<name>.<epoch>.<expUnix>". Returns "" if key is empty (no signer).
func MintLinkToken(key []byte, name string, epoch int, exp time.Time) string {
	if len(key) == 0 {
		return ""
	}
	payload := fmt.Sprintf("%s.%d.%d", name, epoch, exp.Unix())
	sig := signPayload(key, payload)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

// signPayload computes the HMAC-SHA256 of payload under key.
func signPayload(key []byte, payload string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// verifyLinkToken reports whether token is a currently-valid link token for
// (name, epoch) under key. It fails closed on every malformed/expired/tampered
// input and uses a constant-time signature comparison. now is injected for
// deterministic tests.
func verifyLinkToken(key []byte, name string, epoch int, token string, now time.Time) bool {
	if len(key) == 0 || token == "" {
		return false
	}
	dot := strings.IndexByte(token, '.')
	if dot <= 0 || dot == len(token)-1 {
		return false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(token[:dot])
	if err != nil {
		return false
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(token[dot+1:])
	if err != nil {
		return false
	}
	payload := string(payloadBytes)
	wantSig := signPayload(key, payload)
	if subtle.ConstantTimeCompare(gotSig, wantSig) != 1 {
		return false
	}
	// Signature verified — now the claims must match this exposure and be unexpired.
	// Parse from the RIGHT: expUnix and epoch are numeric, but name may itself
	// contain dots... it cannot (prefixPattern forbids dots), so a plain 3-way
	// split is safe. Guard anyway.
	parts := strings.Split(payload, ".")
	if len(parts) != 3 {
		return false
	}
	if parts[0] != name {
		return false
	}
	gotEpoch, err := strconv.Atoi(parts[1])
	if err != nil || gotEpoch != epoch {
		return false
	}
	expUnix, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return false
	}
	return now.Before(time.Unix(expUnix, 0))
}

// exposureMiddleware is the SOLE access gate for /expose/<name>/ routes. It fails
// closed: an unregistered name is 404, and any visibility check that cannot be
// affirmatively satisfied is denied. Non-exposure paths pass straight through.
func (s *Server) exposureMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := exposePrefixFromPath(r.URL.Path)
		if name == "" {
			next.ServeHTTP(w, r)
			return
		}

		policy := s.getExposure(name)
		if policy == nil {
			// Not exposed (or revoked): do not reveal, do not serve.
			exposeDeny(w, http.StatusNotFound, "not found")
			return
		}

		s.mu.RLock()
		resolver := s.meshResolver
		key := s.exposeSigningKey
		s.mu.RUnlock()

		switch policy.Visibility {
		case VisibilityLink:
			now := time.Now()
			// 1. Explicit token (shareable link entry). On success, plant a
			// path-scoped cookie so the browser's sub-resource fetches — which do
			// NOT carry ?access_token= — stay authorized. Without this, the HTML
			// loads but every /assets/* fetch 401s and the app is unusable in a
			// browser (works only for curl).
			if tok := explicitLinkToken(r); tok != "" {
				if verifyLinkToken(key, name, policy.TokenEpoch, tok, now) {
					setLinkCookie(w, name, tok)
					next.ServeHTTP(w, r)
					return
				}
				exposeDeny(w, http.StatusUnauthorized, "a valid access token is required")
				return
			}
			// 2. Cookie (sub-resource requests after the first). Re-verified every
			// time, so an expired/revoked token in the cookie still fails closed.
			if c, err := r.Cookie(exposeCookieName(name)); err == nil {
				if verifyLinkToken(key, name, policy.TokenEpoch, c.Value, now) {
					next.ServeHTTP(w, r)
					return
				}
			}
			exposeDeny(w, http.StatusUnauthorized, "a valid access token is required")
			return

		case VisibilityOrg:
			if id, ok := resolvePeer(resolver, r); ok && id.SameOwner {
				next.ServeHTTP(w, r)
				return
			}
			exposeDeny(w, http.StatusForbidden, "org membership required")
			return

		case VisibilityPrivate:
			// Inert until the creator login is provided by the backend/MCP caller.
			if policy.Creator == "" {
				exposeDeny(w, http.StatusForbidden, "private exposure has no creator set")
				return
			}
			if id, ok := resolvePeer(resolver, r); ok && id.LoginName != "" && id.LoginName == policy.Creator {
				next.ServeHTTP(w, r)
				return
			}
			exposeDeny(w, http.StatusForbidden, "not the exposure creator")
			return

		default:
			// Unknown visibility — fail closed.
			exposeDeny(w, http.StatusForbidden, "access denied")
			return
		}
	})
}

// resolvePeer runs the injected resolver against the request's RemoteAddr,
// returning (identity, true) only on success. A nil resolver or any error yields
// (nil, false) so the caller fails closed.
func resolvePeer(resolver MeshIdentityResolver, r *http.Request) (*MeshPeerIdentity, bool) {
	if resolver == nil {
		return nil, false
	}
	id, err := resolver.ResolvePeer(r.Context(), r.RemoteAddr)
	if err != nil || id == nil {
		return nil, false
	}
	return id, true
}

// exposeDeny writes a JSON error with the given status for a denied exposure
// request.
func exposeDeny(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q}`, msg)
}
