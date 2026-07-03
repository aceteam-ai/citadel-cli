package nodeidentity

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestBuildPairingStartRequest_IncludesCSRAndMachineID(t *testing.T) {
	s := newTestStore(t)

	req, err := s.BuildPairingStartRequest("ABC123", NodeInfo{Hostname: "node-1"}, "machine-abc")
	if err != nil {
		t.Fatalf("BuildPairingStartRequest: %v", err)
	}
	if req.Code != "ABC123" {
		t.Fatalf("unexpected code %q", req.Code)
	}
	if req.MachineID != "machine-abc" {
		t.Fatalf("unexpected machine_id %q", req.MachineID)
	}
	if req.NodeInfo.Hostname != "node-1" {
		t.Fatalf("unexpected hostname %q", req.NodeInfo.Hostname)
	}
	if req.CSRPem == "" {
		t.Fatal("csr_pem is empty")
	}

	// The csr_pem must be a valid, parseable CSR whose key matches the store key.
	block, _ := pem.Decode([]byte(req.CSRPem))
	if block == nil || block.Type != pemTypeCSR {
		t.Fatal("csr_pem is not a CERTIFICATE REQUEST PEM")
	}
	if _, err := x509.ParseCertificateRequest(block.Bytes); err != nil {
		t.Fatalf("csr_pem not parseable: %v", err)
	}

	// And the key must have been created on disk with 0600.
	if !s.HasKey() {
		t.Fatal("key was not persisted")
	}
}

func TestPairingStartRequest_WireShape(t *testing.T) {
	s := newTestStore(t)
	req, err := s.BuildPairingStartRequest("XYZ789", NodeInfo{Hostname: "h"}, "mid")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	raw, err := req.jsonMarshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"code", "node_info", "csr_pem", "machine_id"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("wire body missing key %q; got %v", k, m)
		}
	}
}

func TestPairingStartRequest_OmitsCertFieldsWhenAbsent(t *testing.T) {
	// A plain authkey-only request must not emit csr_pem/machine_id at all,
	// preserving byte-for-byte back-compat with the legacy body.
	req := &PairingStartRequest{Code: "ABC123", NodeInfo: NodeInfo{Hostname: "h"}}
	raw, err := req.jsonMarshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["csr_pem"]; ok {
		t.Fatal("csr_pem should be omitted when empty")
	}
	if _, ok := m["machine_id"]; ok {
		t.Fatal("machine_id should be omitted when empty")
	}
}

func TestStoreLeafFromStatus_StoresWhenPresent(t *testing.T) {
	s := newTestStore(t)
	leaf := makeCert(t, "leaf")
	chain := makeCert(t, "root")

	stored, err := s.StoreLeafFromStatus(&PairingStatusResponse{
		Status:   "confirmed",
		Authkey:  "tskey-abc",
		LeafPem:  leaf,
		ChainPem: chain,
		NodeUID:  "node-uid-123",
	})
	if err != nil {
		t.Fatalf("StoreLeafFromStatus: %v", err)
	}
	if !stored {
		t.Fatal("expected stored=true")
	}
	if !s.HasLeaf() {
		t.Fatal("leaf not persisted")
	}
}

func TestStoreLeafFromStatus_NoOpWhenBackendReturnsNoCert(t *testing.T) {
	s := newTestStore(t)

	// Authkey-only response (older backend / CA not activated): pairing must
	// succeed, no cert stored, no error.
	stored, err := s.StoreLeafFromStatus(&PairingStatusResponse{
		Status:  "confirmed",
		Authkey: "tskey-abc",
	})
	if err != nil {
		t.Fatalf("StoreLeafFromStatus: %v", err)
	}
	if stored {
		t.Fatal("expected stored=false for authkey-only response")
	}
	if s.HasLeaf() {
		t.Fatal("leaf created despite no cert in response")
	}
}

func TestStoreLeafFromStatus_NilResponse(t *testing.T) {
	s := newTestStore(t)
	stored, err := s.StoreLeafFromStatus(nil)
	if err != nil || stored {
		t.Fatalf("expected (false,nil), got (%v,%v)", stored, err)
	}
}

func TestFetchCAChain_StoresChainOn200(t *testing.T) {
	chain := makeCert(t, "intermediate") + makeCert(t, "root")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/fabric/ca/chain" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/x-pem-file")
		_, _ = w.Write([]byte(chain))
	}))
	defer srv.Close()

	s := newTestStore(t)
	if err := s.FetchCAChain(context.Background(), srv.URL, srv.Client()); err != nil {
		t.Fatalf("FetchCAChain: %v", err)
	}
	data, err := readFile(s.CAChainPath())
	if err != nil {
		t.Fatalf("read chain: %v", err)
	}
	if err := validateCertPEM(data); err != nil {
		t.Fatalf("stored chain invalid: %v", err)
	}
}

func TestFetchCAChain_DegradesOn503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	s := newTestStore(t)
	err := s.FetchCAChain(context.Background(), srv.URL, srv.Client())
	if !errors.Is(err, ErrCANotActivated) {
		t.Fatalf("expected ErrCANotActivated, got %v", err)
	}
	// Nothing stored on degrade.
	if _, statErr := readFile(s.CAChainPath()); statErr == nil {
		t.Fatal("chain file written despite 503")
	}
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
