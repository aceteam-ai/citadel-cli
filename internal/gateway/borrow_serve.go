// internal/gateway/borrow_serve.go
//
// SERVING side of peer-service borrowing (citadel-cli#1289). A same-owner mesh
// peer — or the platform infra identity — can borrow a LOOPBACK service running
// on this node (embedding on TEI, transcription on whisper, ...) over the mesh,
// through the gateway's existing VPN listener. Nothing new is exposed: the
// service stays bound to loopback; this route is the only thing between a
// verified mesh peer and it, and it is the authorization boundary.
//
// # The gate (recorded per the issue's requirement)
//
// A borrow request must satisfy, checked FIRST (before any provider lookup or
// upstream dial, and fail-closed on a resolver error):
//
//  1. A verified mesh identity with a non-empty stable device id AND owner id
//     (serviceborrow.Decide's #1068 rule), that is EITHER same-owner OR on the
//     node operator's platform-infra allowlist (the same platform-exposure.json
//     the gateway's platform exposures use). A shared-org, foreign, or
//     unverified caller is refused. This reuses THIS server's existing
//     meshResolver (-> network.WhoIsPeer) — no second resolver.
//
// The posture is NOT a byte-for-byte copy of egressrelay.Authorize, and
// deliberately so: it borrows egress-relay's MECHANISM (WhoIsPeer via the
// gateway's resolver, no token/passcode fallback), takes its POLICY from
// cmd/platform_exposure.go (same-owner OR platform-infra allowlist — egress-relay
// is same-owner only), and TIGHTENS both with #1068's "empty stable/owner id
// means unverified -> refuse" rule, which egress-relay predates. See
// serviceborrow.Decide for the exact ordered check.
//
// Identity is resolved from the request's verified mesh RemoteAddr
// (resolvePeer), so a spoofed X-Forwarded-For / X-Real-IP is ignored. Because
// the gate fails closed on a plain LAN RemoteAddr (it never resolves to a
// verified peer), the route is safe on the LAN listener it shares with the VPN
// listener — same reasoning as /cache/.
//
// # Receipts (fail closed, reused machinery)
//
// A served borrow call signs a v2 AEP work receipt and writes a typed-unit work
// record (reusing jobs.WorkReceiptConfig via the injected Record hook; the
// gateway package itself imports no aep/usage). Signing is PREFLIGHTED before any
// upstream dial: a node that cannot sign refuses to serve (503 service_unavailable)
// rather than serve unsigned dispatched work. The serving receipt's node_id and
// an identifier are echoed to the requester (headers) so the requester records
// delegated_node_id == this node and references the serving work by job id.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/serviceborrow"
)

const (
	// maxBorrowRequestBytes bounds a borrowed request body (a mesh peer must not
	// be able to exhaust this node's memory).
	maxBorrowRequestBytes = 64 << 20
	// maxBorrowUpstreamBytes bounds the loopback service's response body — it is
	// buffered so its hash can be signed into the serving receipt before headers
	// go out. Streaming services are deliberately out of scope for this contract.
	maxBorrowUpstreamBytes = 64 << 20
)

// BorrowProvider is one borrowable loopback service registered with the serving
// gateway. The upstream address and allowed subpaths come ONLY from here (never
// from the request), so a peer cannot steer the proxy at an arbitrary target.
type BorrowProvider struct {
	// Kind is the service kind ("embedding", "transcription", ...), the first
	// path segment under /borrow/.
	Kind string
	// Upstream is the loopback address of the service (e.g. "127.0.0.1:8102").
	Upstream string
	// AllowedSubpaths is the exact set of POST subpaths this provider serves
	// (e.g. {"/v1/embeddings": true}). A subpath not in the set is 404.
	AllowedSubpaths map[string]bool
	// WorkAction is the aep work action signed into the serving receipt (e.g.
	// "embed_served"). It MUST be a known aep work action.
	WorkAction string
	// Identity/Capacity/Ready advertise the service; injected so tests need no
	// live loopback service. Ready reports whether the service is actually up.
	Identity func() serviceborrow.ServiceIdentity
	Capacity func() serviceborrow.Capacity
	Ready    func() bool
}

// BorrowServeConfig enables and configures the serving side. All I/O that the
// gateway package must not do itself (signing, usage recording, allowlist read)
// is injected, keeping this package free of aep/usage/network imports.
type BorrowServeConfig struct {
	// AllowedLogins returns the platform-infra allowlist (login -> true). A nil
	// func means same-owner only (the egress-relay posture). A non-nil error
	// REFUSES the request (fail closed), matching cmd/platform_exposure.go.
	AllowedLogins func() (map[string]bool, error)
	// Providers are the borrowable services, keyed by Kind.
	Providers map[string]*BorrowProvider
	// Preflight reports whether this node can sign a serving work receipt,
	// checked before any upstream dial so the node never serves unsigned
	// dispatched work. A non-nil error -> 503 service_unavailable to the requester.
	Preflight func() error
	// Record signs a serving work receipt and writes a work record for one served
	// call, returning the serving node id (== receipt node_id) and a receipt
	// identifier to echo to the requester. A non-nil error fails the serve closed.
	Record func(kind, action, jobID string, peer serviceborrow.PeerIdentity, reqBody, respBody []byte) (servingNodeID, receiptID string, err error)
}

// SetBorrowServer enables the peer-service borrowing routes (/borrow/...). Pass
// nil to leave them unregistered (the default). Must be called before Start.
func (s *Server) SetBorrowServer(cfg *BorrowServeConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.borrowCfg = cfg
}

// registerBorrowRoutes wires the borrow handler onto the mux (from Start, and
// directly from tests, so tests exercise the SAME registration path).
func (s *Server) registerBorrowRoutes() {
	s.mux.HandleFunc(serviceborrow.RoutePrefix, s.handleBorrow)
}

// borrowHTTPClient is the client used to reach a loopback service. A plain
// client with no proxy; the per-request context (bounded by the gateway's
// WriteTimeout) bounds the call.
var borrowHTTPClient = &http.Client{}

// authorizeBorrow runs the serving gate. It returns the verified requester
// identity and true only when the caller may borrow; otherwise it has written a
// JSON error and returns false. Fail-closed throughout.
func (s *Server) authorizeBorrow(w http.ResponseWriter, r *http.Request) (serviceborrow.PeerIdentity, bool) {
	s.mu.RLock()
	resolver := s.meshResolver
	cfg := s.borrowCfg
	s.mu.RUnlock()

	if cfg == nil {
		exposeDeny(w, http.StatusNotFound, "borrow serving not enabled")
		return serviceborrow.PeerIdentity{}, false
	}

	// Platform-infra allowlist. A loader error refuses the request (fail closed),
	// never degrades to same-owner-only — the platform_exposure.go posture.
	var allowed map[string]bool
	if cfg.AllowedLogins != nil {
		a, err := cfg.AllowedLogins()
		if err != nil {
			exposeDeny(w, http.StatusForbidden, "node platform allowlist unavailable")
			return serviceborrow.PeerIdentity{}, false
		}
		allowed = a
	}

	// Identity FIRST, from the verified mesh RemoteAddr (never a header).
	mid, ok := resolvePeer(resolver, r)
	if !ok {
		exposeDeny(w, http.StatusForbidden, "verified same-owner mesh peer required")
		return serviceborrow.PeerIdentity{}, false
	}
	pid := serviceborrow.PeerIdentity{
		NodeName:  mid.NodeName,
		LoginName: mid.LoginName,
		StableID:  mid.StableID,
		OwnerID:   mid.OwnerID,
		SameOwner: mid.SameOwner,
	}
	if err := serviceborrow.Decide(&pid, allowed); err != nil {
		exposeDeny(w, http.StatusForbidden, "borrow denied: "+err.Error())
		return serviceborrow.PeerIdentity{}, false
	}
	return pid, true
}

// handleBorrow dispatches /borrow/<kind>/identity (GET) and
// /borrow/<kind>/<subpath> (POST). The gate runs first for every method.
func (s *Server) handleBorrow(w http.ResponseWriter, r *http.Request) {
	peer, ok := s.authorizeBorrow(w, r)
	if !ok {
		return
	}

	s.mu.RLock()
	cfg := s.borrowCfg
	s.mu.RUnlock()

	kind, sub := splitBorrowPath(r.URL.Path)
	if kind == "" {
		exposeDeny(w, http.StatusNotFound, "not found")
		return
	}
	provider, found := cfg.Providers[kind]
	if !found || provider == nil {
		exposeDeny(w, http.StatusNotFound, "unknown service kind")
		return
	}

	if r.Method == http.MethodGet && sub == "/identity" {
		s.serveBorrowIdentity(w, provider)
		return
	}
	if r.Method == http.MethodPost {
		s.serveBorrowProxy(w, r, cfg, provider, sub, peer)
		return
	}
	exposeDeny(w, http.StatusNotFound, "not found")
}

// splitBorrowPath splits /borrow/<kind>/<subpath> into kind and the leading-slash
// subpath ("/identity", "/v1/embeddings", ...). Returns ("","") when malformed.
func splitBorrowPath(path string) (kind, sub string) {
	rest := strings.TrimPrefix(path, serviceborrow.RoutePrefix)
	if rest == path || rest == "" {
		return "", ""
	}
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return "", "" // /borrow/<kind> with no subpath is not a route
	}
	kind = rest[:i]
	sub = rest[i:]
	if kind == "" || sub == "/" {
		return "", ""
	}
	return kind, sub
}

func (s *Server) serveBorrowIdentity(w http.ResponseWriter, p *BorrowProvider) {
	adv := serviceborrow.Advertisement{}
	if p.Identity != nil {
		adv.Identity = p.Identity()
	}
	if p.Capacity != nil {
		adv.Capacity = p.Capacity()
	}
	if p.Ready != nil {
		adv.Ready = p.Ready()
	}
	body, err := json.Marshal(adv)
	if err != nil {
		exposeDeny(w, http.StatusInternalServerError, "encode error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) serveBorrowProxy(w http.ResponseWriter, r *http.Request, cfg *BorrowServeConfig, p *BorrowProvider, sub string, peer serviceborrow.PeerIdentity) {
	if !p.AllowedSubpaths[sub] {
		exposeDeny(w, http.StatusNotFound, "subpath not allowed for this service")
		return
	}
	jobID := r.Header.Get(serviceborrow.HeaderJobID)
	if !serviceborrow.ValidJobID(jobID) {
		// A server-minted id would be invisible to the requester's ledger, so the
		// requester MUST supply a valid per-call id. Reject rather than generate.
		exposeDeny(w, http.StatusBadRequest, "missing or invalid "+serviceborrow.HeaderJobID)
		return
	}

	// Fail closed BEFORE any upstream dial: never serve unsigned dispatched work.
	if cfg.Preflight != nil {
		if err := cfg.Preflight(); err != nil {
			exposeDeny(w, http.StatusServiceUnavailable, "node cannot sign a serving receipt")
			return
		}
	}

	reqBody, err := io.ReadAll(io.LimitReader(r.Body, maxBorrowRequestBytes))
	if err != nil {
		exposeDeny(w, http.StatusBadRequest, "read request body")
		return
	}

	respStatus, respBody, respCT, err := s.borrowUpstreamCall(r.Context(), p, sub, reqBody, r.Header.Get("Content-Type"))
	if err != nil {
		exposeDeny(w, http.StatusBadGateway, "loopback service unreachable")
		return
	}

	// Sign + record ONLY on an upstream success; otherwise propagate the status
	// with no receipt (no work was performed).
	if respStatus < 200 || respStatus >= 300 {
		if respCT != "" {
			w.Header().Set("Content-Type", respCT)
		}
		w.WriteHeader(respStatus)
		_, _ = w.Write(respBody)
		return
	}

	servingNodeID, receiptID := "", ""
	if cfg.Record != nil {
		var rerr error
		servingNodeID, receiptID, rerr = cfg.Record(p.Kind, p.WorkAction, jobID, peer, reqBody, respBody)
		if rerr != nil {
			// Could not sign/record the served work: fail closed. The work ran on
			// loopback, but without a signed receipt we never return it as served.
			exposeDeny(w, http.StatusServiceUnavailable, "could not record serving receipt")
			return
		}
	}

	if servingNodeID != "" {
		w.Header().Set(serviceborrow.HeaderServingNodeID, servingNodeID)
	}
	if receiptID != "" {
		w.Header().Set(serviceborrow.HeaderReceiptID, receiptID)
	}
	w.Header().Set(serviceborrow.HeaderJobID, jobID)
	if respCT != "" {
		w.Header().Set("Content-Type", respCT)
	}
	w.WriteHeader(respStatus)
	_, _ = w.Write(respBody)
}

// borrowUpstreamCall proxies reqBody to the provider's loopback service. It
// builds a FRESH request carrying only Content-Type — every inbound header
// (trust headers, the requester's Authorization, hop-by-hop headers, the
// X-Borrow-* headers) is dropped, so an unauthenticated loopback service never
// sees a forwarded credential or spoofed trust header. The upstream address
// comes only from the provider (never the request).
func (s *Server) borrowUpstreamCall(ctx context.Context, p *BorrowProvider, sub string, reqBody []byte, contentType string) (status int, body []byte, respContentType string, err error) {
	url := "http://" + net.JoinHostPort(hostPort(p.Upstream)) + sub
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return 0, nil, "", err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := borrowHTTPClient.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxBorrowUpstreamBytes))
	if err != nil {
		return 0, nil, "", err
	}
	return resp.StatusCode, body, resp.Header.Get("Content-Type"), nil
}

// hostPort returns host and port for net.JoinHostPort. The upstream is an
// operator-registered "host:port"; if it has no port, it is returned as the host
// with an empty port (JoinHostPort tolerates it, and the dial then fails cleanly
// — a misconfigured provider, never a request-controlled target).
func hostPort(addr string) (string, string) {
	if h, p, err := net.SplitHostPort(addr); err == nil {
		return h, p
	}
	return addr, ""
}
