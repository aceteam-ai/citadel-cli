package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/serviceborrow"
)

// borrowAuthResolver records the remoteAddr it was asked about and returns a
// fixed identity/error — so a test can assert the gate keyed on the verified
// mesh RemoteAddr, never a spoofed header.
type borrowAuthResolver struct {
	id       *MeshPeerIdentity
	err      error
	lastAddr string
}

func (r *borrowAuthResolver) ResolvePeer(_ context.Context, remoteAddr string) (*MeshPeerIdentity, error) {
	r.lastAddr = remoteAddr
	if r.err != nil {
		return nil, r.err
	}
	return r.id, nil
}

func sameOwnerID() *MeshPeerIdentity {
	return &MeshPeerIdentity{NodeName: "peer", LoginName: "owner@x.co", StableID: "nodekey:a", OwnerID: "userid:1", SameOwner: true}
}

// upstreamStub stands in for a loopback service; it records hits and returns a
// configurable status + body.
type upstreamStub struct {
	srv    *httptest.Server
	addr   string
	hits   int
	status int
	body   string
}

func newUpstreamStub(t *testing.T, status int, body string) *upstreamStub {
	t.Helper()
	u := &upstreamStub{status: status, body: body}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.status)
		_, _ = io.WriteString(w, u.body)
	}))
	t.Cleanup(u.srv.Close)
	u.addr = strings.TrimPrefix(u.srv.URL, "http://")
	return u
}

type recordCall struct {
	kind, action, jobID string
	peer                serviceborrow.PeerIdentity
}

// borrowTestServer builds a gateway with the borrow routes registered the same
// way Start does. preflightErr/recordErr/recordCalls let a test drive the
// fail-closed paths without a real signer.
func borrowTestServer(t *testing.T, resolver MeshIdentityResolver, cfg *BorrowServeConfig) *Server {
	t.Helper()
	gw := NewServer(Config{Port: 0, NodeName: "serving-node"})
	if resolver != nil {
		gw.SetMeshResolver(resolver)
	}
	gw.SetBorrowServer(cfg)
	gw.registerBorrowRoutes()
	return gw
}

func borrowReq(method, target string, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.RemoteAddr = "100.64.0.9:1234"
	return r
}

func embeddingProvider(upstreamAddr string) *BorrowProvider {
	return &BorrowProvider{
		Kind:            "embedding",
		Upstream:        upstreamAddr,
		AllowedSubpaths: map[string]bool{"/v1/embeddings": true},
		WorkAction:      "embed_served",
		Identity: func() serviceborrow.ServiceIdentity {
			return serviceborrow.ServiceIdentity{Kind: "embedding", Model: "gte"}
		},
		Capacity: func() serviceborrow.Capacity { return serviceborrow.Capacity{Device: "cuda"} },
		Ready:    func() bool { return true },
	}
}

func baseCfg(up *upstreamStub, calls *[]recordCall) *BorrowServeConfig {
	return &BorrowServeConfig{
		Providers: map[string]*BorrowProvider{"embedding": embeddingProvider(up.addr)},
		Preflight: func() error { return nil },
		Record: func(kind, action, jobID string, peer serviceborrow.PeerIdentity, _, _ []byte) (string, string, error) {
			if calls != nil {
				*calls = append(*calls, recordCall{kind, action, jobID, peer})
			}
			return "serving-node-id", "receipt-id", nil
		},
	}
}

// --- Advertisement + gate (adversarial) -----------------------------------

func TestBorrow_Identity_SameOwnerAllowed(t *testing.T) {
	up := newUpstreamStub(t, 200, "{}")
	gw := borrowTestServer(t, &borrowAuthResolver{id: sameOwnerID()}, baseCfg(up, nil))
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, borrowReq(http.MethodGet, serviceborrow.IdentityPath("embedding"), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var adv serviceborrow.Advertisement
	if err := json.Unmarshal(rec.Body.Bytes(), &adv); err != nil {
		t.Fatalf("decode advertisement: %v", err)
	}
	if adv.Identity.Kind != "embedding" || adv.Identity.Model != "gte" || !adv.Ready {
		t.Fatalf("advertisement = %+v", adv)
	}
}

func TestBorrow_Gate_ForeignRefused(t *testing.T) {
	up := newUpstreamStub(t, 200, "{}")
	foreign := &MeshPeerIdentity{NodeName: "x", LoginName: "a@evil.co", StableID: "nodekey:z", OwnerID: "userid:99", SameOwner: false}
	gw := borrowTestServer(t, &borrowAuthResolver{id: foreign}, baseCfg(up, nil))
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, borrowReq(http.MethodGet, serviceborrow.IdentityPath("embedding"), ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a foreign peer", rec.Code)
	}
}

func TestBorrow_Gate_UnverifiedResolverError_Refused(t *testing.T) {
	up := newUpstreamStub(t, 200, "{}")
	gw := borrowTestServer(t, &borrowAuthResolver{err: context.DeadlineExceeded}, baseCfg(up, nil))
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, borrowReq(http.MethodGet, serviceborrow.IdentityPath("embedding"), ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 on resolver error", rec.Code)
	}
}

func TestBorrow_Gate_EmptyStableID_RefusedFailClosed(t *testing.T) {
	up := newUpstreamStub(t, 200, "{}")
	id := sameOwnerID()
	id.StableID = "" // unverified device
	gw := borrowTestServer(t, &borrowAuthResolver{id: id}, baseCfg(up, nil))
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, borrowReq(http.MethodGet, serviceborrow.IdentityPath("embedding"), ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when StableID is empty (fail closed)", rec.Code)
	}
}

func TestBorrow_Gate_PlatformInfraLoginAllowed(t *testing.T) {
	up := newUpstreamStub(t, 200, "{}")
	infra := &MeshPeerIdentity{NodeName: "coord", LoginName: "infra@aceteam.ai", StableID: "nodekey:i", OwnerID: "userid:1", SameOwner: false}
	cfg := baseCfg(up, nil)
	cfg.AllowedLogins = func() (map[string]bool, error) { return map[string]bool{"infra@aceteam.ai": true}, nil }
	gw := borrowTestServer(t, &borrowAuthResolver{id: infra}, cfg)
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, borrowReq(http.MethodGet, serviceborrow.IdentityPath("embedding"), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a platform-infra login", rec.Code)
	}
}

func TestBorrow_Gate_AllowlistLoaderError_Refused(t *testing.T) {
	up := newUpstreamStub(t, 200, "{}")
	cfg := baseCfg(up, nil)
	cfg.AllowedLogins = func() (map[string]bool, error) { return nil, context.DeadlineExceeded }
	gw := borrowTestServer(t, &borrowAuthResolver{id: sameOwnerID()}, cfg)
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, borrowReq(http.MethodGet, serviceborrow.IdentityPath("embedding"), ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when the allowlist loader errors (fail closed, no degrade)", rec.Code)
	}
}

func TestBorrow_Gate_KeysOnRemoteAddrNotHeader(t *testing.T) {
	up := newUpstreamStub(t, 200, "{}")
	rr := &borrowAuthResolver{id: sameOwnerID()}
	gw := borrowTestServer(t, rr, baseCfg(up, nil))
	req := borrowReq(http.MethodGet, serviceborrow.IdentityPath("embedding"), "")
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Real-IP", "5.6.7.8")
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, req)
	if rr.lastAddr != "100.64.0.9:1234" {
		t.Fatalf("gate resolved addr %q, want the verified RemoteAddr (spoofed header must be ignored)", rr.lastAddr)
	}
}

// --- Proxy path -----------------------------------------------------------

func TestBorrow_Proxy_SuccessSignsAndEchoesHeaders(t *testing.T) {
	up := newUpstreamStub(t, 200, `{"data":[1,2,3]}`)
	var calls []recordCall
	gw := borrowTestServer(t, &borrowAuthResolver{id: sameOwnerID()}, baseCfg(up, &calls))
	req := borrowReq(http.MethodPost, serviceborrow.RoutePrefix+"embedding/v1/embeddings", `{"input":["hi"]}`)
	req.Header.Set(serviceborrow.HeaderJobID, "parent-1.0")
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(serviceborrow.HeaderServingNodeID) != "serving-node-id" {
		t.Fatalf("serving node header = %q", rec.Header().Get(serviceborrow.HeaderServingNodeID))
	}
	if rec.Header().Get(serviceborrow.HeaderReceiptID) != "receipt-id" {
		t.Fatalf("receipt header = %q", rec.Header().Get(serviceborrow.HeaderReceiptID))
	}
	if rec.Header().Get(serviceborrow.HeaderJobID) != "parent-1.0" {
		t.Fatalf("job id header = %q", rec.Header().Get(serviceborrow.HeaderJobID))
	}
	if rec.Body.String() != `{"data":[1,2,3]}` {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if len(calls) != 1 || calls[0].jobID != "parent-1.0" || calls[0].action != "embed_served" {
		t.Fatalf("record calls = %+v", calls)
	}
	// The Record hook received the verified requester identity (bound into the
	// serving receipt's input manifest).
	if calls[0].peer.StableID != "nodekey:a" || calls[0].peer.OwnerID != "userid:1" {
		t.Fatalf("record peer = %+v, want the verified requester identity", calls[0].peer)
	}
}

func TestBorrow_Proxy_MissingJobID_BadRequest(t *testing.T) {
	up := newUpstreamStub(t, 200, "{}")
	gw := borrowTestServer(t, &borrowAuthResolver{id: sameOwnerID()}, baseCfg(up, nil))
	req := borrowReq(http.MethodPost, serviceborrow.RoutePrefix+"embedding/v1/embeddings", "{}")
	// no X-Borrow-Job-Id header
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a missing job id", rec.Code)
	}
	if up.hits != 0 {
		t.Fatalf("upstream must not be hit when the job id is missing")
	}
}

func TestBorrow_Proxy_SubpathNotAllowed_NotFound(t *testing.T) {
	up := newUpstreamStub(t, 200, "{}")
	gw := borrowTestServer(t, &borrowAuthResolver{id: sameOwnerID()}, baseCfg(up, nil))
	req := borrowReq(http.MethodPost, serviceborrow.RoutePrefix+"embedding/v1/secret", "{}")
	req.Header.Set(serviceborrow.HeaderJobID, "p.0")
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a non-allowlisted subpath", rec.Code)
	}
	if up.hits != 0 {
		t.Fatalf("upstream must not be hit for a disallowed subpath")
	}
}

func TestBorrow_Proxy_PreflightFails_ServiceUnavailableNoUpstream(t *testing.T) {
	up := newUpstreamStub(t, 200, "{}")
	cfg := baseCfg(up, nil)
	cfg.Preflight = func() error { return context.DeadlineExceeded }
	gw := borrowTestServer(t, &borrowAuthResolver{id: sameOwnerID()}, cfg)
	req := borrowReq(http.MethodPost, serviceborrow.RoutePrefix+"embedding/v1/embeddings", "{}")
	req.Header.Set(serviceborrow.HeaderJobID, "p.0")
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the node cannot sign", rec.Code)
	}
	if up.hits != 0 {
		t.Fatalf("upstream must not be dialed when signing is unavailable (never serve unsigned)")
	}
}

func TestBorrow_Proxy_UpstreamErrorStatus_NoReceipt(t *testing.T) {
	up := newUpstreamStub(t, http.StatusInternalServerError, "boom")
	var calls []recordCall
	gw := borrowTestServer(t, &borrowAuthResolver{id: sameOwnerID()}, baseCfg(up, &calls))
	req := borrowReq(http.MethodPost, serviceborrow.RoutePrefix+"embedding/v1/embeddings", "{}")
	req.Header.Set(serviceborrow.HeaderJobID, "p.0")
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the upstream 500 propagated", rec.Code)
	}
	if len(calls) != 0 {
		t.Fatalf("no receipt must be signed when the upstream did not succeed; got %d record calls", len(calls))
	}
	if rec.Header().Get(serviceborrow.HeaderServingNodeID) != "" {
		t.Fatalf("no serving headers on a non-2xx upstream response")
	}
}

func TestBorrow_Proxy_RecordError_ServiceUnavailable(t *testing.T) {
	up := newUpstreamStub(t, 200, "{}")
	cfg := baseCfg(up, nil)
	cfg.Record = func(_, _, _ string, _ serviceborrow.PeerIdentity, _, _ []byte) (string, string, error) {
		return "", "", context.DeadlineExceeded
	}
	gw := borrowTestServer(t, &borrowAuthResolver{id: sameOwnerID()}, cfg)
	req := borrowReq(http.MethodPost, serviceborrow.RoutePrefix+"embedding/v1/embeddings", "{}")
	req.Header.Set(serviceborrow.HeaderJobID, "p.0")
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the serving receipt cannot be recorded", rec.Code)
	}
}

func TestBorrow_UnknownKind_NotFound(t *testing.T) {
	up := newUpstreamStub(t, 200, "{}")
	gw := borrowTestServer(t, &borrowAuthResolver{id: sameOwnerID()}, baseCfg(up, nil))
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, borrowReq(http.MethodGet, serviceborrow.IdentityPath("nonsense"), ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unknown service kind", rec.Code)
	}
}

// TestBorrow_FullChainThroughBuildHandler proves the whole middleware chain
// lets a borrow request reach the borrow gate: categoryForPath always-allows
// /borrow/, so permissionMiddleware never blocks it before the identity-first
// gate (a borrow caller has no node passcode or capability).
func TestBorrow_FullChainThroughBuildHandler(t *testing.T) {
	up := newUpstreamStub(t, 200, "{}")
	gw := borrowTestServer(t, &borrowAuthResolver{id: sameOwnerID()}, baseCfg(up, nil))
	rec := httptest.NewRecorder()
	gw.BuildHandler().ServeHTTP(rec, borrowReq(http.MethodGet, serviceborrow.IdentityPath("embedding"), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d through BuildHandler, want 200 (categoryForPath must always-allow /borrow/)", rec.Code)
	}
}
