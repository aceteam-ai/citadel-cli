package status

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A route installed via AddControlRouteRegistrar must be served ONLY on the
// mTLS control mux, behind requireCoordinator, never on the plaintext mux that
// trusts mesh origin (citadel-cli#1305, the #5028 rule).
func TestControlRouteRegistrarCoordinatorOnly(t *testing.T) {
	ca := newTestCA(t, "fabric-ca")
	v, err := NewFabricCAVerifier(writeBundle(t, ca.certPEM), []string{coordinatorSAN})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	serverCert := ca.issueLeaf(t, "node-server")
	srv := NewServer(ServerConfig{CAVerifier: v, ControlServerCert: &serverCert}, NewCollector(CollectorConfig{NodeName: "test-node"}))

	called := false
	srv.AddControlRouteRegistrar(func(mux *http.ServeMux, gate func(http.HandlerFunc) http.HandlerFunc) {
		mux.HandleFunc("/control-probe", gate(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		}))
	})

	post := func(mux *http.ServeMux, leaf *x509.Certificate) int {
		req := httptest.NewRequest(http.MethodPost, "/control-probe", nil)
		if leaf != nil {
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}

	if code := post(srv.buildMux(), nil); code != http.StatusNotFound || called {
		t.Fatalf("control route leaked onto the plaintext mux: code=%d called=%v", code, called)
	}
	control := srv.buildControlMux()
	peer := ca.issueLeaf(t, "peer-node", "aceteam:node:abc", "aceteam:org:other")
	if code := post(control, peer.Leaf); code != http.StatusForbidden || called {
		t.Fatalf("non-coordinator reached a control route: code=%d called=%v", code, called)
	}
	coord := ca.issueLeaf(t, "coordinator", coordinatorSAN)
	if code := post(control, coord.Leaf); code != http.StatusOK || !called {
		t.Fatalf("coordinator refused on its control route: code=%d called=%v", code, called)
	}
}
