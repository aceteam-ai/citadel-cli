package devicemode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
)

// TestStartDevicePairingWireShape verifies the request body the platform's
// pairing/start endpoint receives: device profile, CSR, machine_id — and that
// the private key is NOT in it.
func TestStartDevicePairingWireShape(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/fabric/pairing/start" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"ok": true, "expires_in": 300}`))
	}))
	defer srv.Close()

	store := nodeidentity.New(t.TempDir())
	err := StartDevicePairing(
		context.Background(), srv.Client(), srv.URL, "ABC234", store, "my-laptop", "machine-1",
	)
	if err != nil {
		t.Fatal(err)
	}

	if received["profile"] != "device" {
		t.Fatalf("profile = %v, want device", received["profile"])
	}
	if received["machine_id"] != "machine-1" {
		t.Fatalf("machine_id = %v", received["machine_id"])
	}
	csr, _ := received["csr_pem"].(string)
	if csr == "" || !containsPEMBlock(csr, "CERTIFICATE REQUEST") {
		t.Fatalf("csr_pem missing or malformed: %q", csr)
	}
	if containsPEMBlock(csr, "PRIVATE KEY") {
		t.Fatal("private key leaked into pairing request")
	}
	nodeInfo, _ := received["node_info"].(map[string]any)
	if nodeInfo["hostname"] != "my-laptop" {
		t.Fatalf("hostname = %v", nodeInfo["hostname"])
	}
}

func TestStartDevicePairingSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"detail": "rate limited"}`))
	}))
	defer srv.Close()

	store := nodeidentity.New(t.TempDir())
	err := StartDevicePairing(
		context.Background(), srv.Client(), srv.URL, "ABC234", store, "h", "m",
	)
	if err == nil {
		t.Fatal("expected error on HTTP 429")
	}
}

func TestWaitForApprovalConfirmed(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&polls, 1)
		if n < 3 {
			_, _ = w.Write([]byte(`{"status": "pending"}`))
			return
		}
		_, _ = fmt.Fprint(w, `{
			"status": "confirmed",
			"authkey": "hskey-auth-abc",
			"leaf_pem": "LEAF",
			"chain_pem": "CHAIN",
			"node_uid": "uid-1"
		}`)
	}))
	defer srv.Close()

	result, err := WaitForApproval(context.Background(), srv.Client(), srv.URL, "ABC234")
	if err != nil {
		t.Fatal(err)
	}
	if result.Authkey != "hskey-auth-abc" || result.NodeUID != "uid-1" ||
		result.LeafPem != "LEAF" || result.ChainPem != "CHAIN" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if atomic.LoadInt32(&polls) < 3 {
		t.Fatalf("expected >= 3 polls, got %d", polls)
	}
}

func TestWaitForApprovalExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status": "expired"}`))
	}))
	defer srv.Close()

	if _, err := WaitForApproval(context.Background(), srv.Client(), srv.URL, "ABC234"); err == nil {
		t.Fatal("expected expiry error")
	}
}

func TestWaitForApprovalConfirmedWithoutAuthkeyFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status": "confirmed", "authkey": ""}`))
	}))
	defer srv.Close()

	if _, err := WaitForApproval(context.Background(), srv.Client(), srv.URL, "ABC234"); err == nil {
		t.Fatal("expected error for confirmed-without-authkey")
	}
}

func TestWaitForApprovalHonorsContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status": "pending"}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := WaitForApproval(ctx, srv.Client(), srv.URL, "ABC234"); err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func containsPEMBlock(s, blockType string) bool {
	return strings.Contains(s, "-----BEGIN "+blockType+"-----")
}
