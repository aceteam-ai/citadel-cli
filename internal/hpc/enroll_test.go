//go:build !windows

package hpc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const testNodeID = "hpc-0123456789abcdef0123456789abcdef"
const testToken = "hpcet_secret_token_value"

func writeToken(t *testing.T, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(testToken+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadTokenFileRemovesAfterRead(t *testing.T) {
	p := writeToken(t, 0o600)
	tok, err := ReadTokenFile(p, os.Getuid())
	if err != nil || tok != testToken {
		t.Fatalf("got %q, %v", tok, err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("token file still present: %v", err)
	}
}

func TestReadTokenFileRefusals(t *testing.T) {
	loose := writeToken(t, 0o640)
	if _, err := ReadTokenFile(loose, os.Getuid()); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("group-readable: %v", err)
	}
	if _, err := os.Stat(loose); err != nil {
		t.Fatal("a refused token file must be left in place for the operator")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(writeToken(t, 0o600), link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTokenFile(link, os.Getuid()); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := ReadTokenFile(writeToken(t, 0o600), os.Getuid()+1); err == nil || !strings.Contains(err.Error(), "owned by") {
		t.Fatalf("owner: %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTokenFile(empty, os.Getuid()); err == nil {
		t.Fatal("empty token accepted")
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatal("a read token file must be removed even when its content is bad")
	}
}

func testLabels() Labels {
	return Labels{Cluster: "c", Partition: "p", Account: "a", SlurmJobID: "1", Host: "h", AllocatedGPUs: 1,
		GPUType: "A100", WallTimeEnd: time.Now().Add(time.Hour)}
}

func goodEnrollResponse(base string) map[string]any {
	return map[string]any{
		"node_id": testNodeID, "org_id": "org-1", "device_api_token": "dev_tok", "api_base_url": base,
		"leaf_pem": "", "chain_pem": "", "node_uid": "uid-1", "expires_at": "2026-11-12T12:05:00Z",
	}
}

func TestEnrollSendsContractAndParsesResponse(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != EnrollPath {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("enroll must not send an Authorization header")
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_ = json.NewEncoder(w).Encode(goodEnrollResponse("http://" + r.Host))
	}))
	defer srv.Close()
	resp, err := Client{}.Enroll(context.Background(), srv.URL, testToken, "-----BEGIN CERTIFICATE REQUEST-----\nx\n-----END CERTIFICATE REQUEST-----\n", testLabels())
	if err != nil {
		t.Fatal(err)
	}
	if resp.NodeID != testNodeID || resp.DeviceAPIToken != "dev_tok" {
		t.Fatalf("resp = %+v", resp)
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	if len(keys) != 3 || got["enrollment_token"] != testToken || got["csr_pem"] == nil || got["labels"] == nil {
		t.Fatalf("request = %v", got)
	}
}

func TestEnrollErrorMapping(t *testing.T) {
	for status, want := range map[int]string{409: "unavailable", 400: "labels", 403: "plan", 503: "burned", 500: "HTTP 500"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"x","echo":"` + testToken + `"}`))
		}))
		_, err := Client{}.Enroll(context.Background(), srv.URL, testToken, "csr", testLabels())
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%d: err = %v, want %q", status, err, want)
			continue
		}
		if strings.Contains(err.Error(), testToken) {
			t.Errorf("%d: error leaks the token", status)
		}
		if status == 409 && !errors.Is(err, ErrTokenUnavailable) {
			t.Errorf("409 is not ErrTokenUnavailable")
		}
	}
}

func TestEnrollRejectsBadResponses(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"numeric node id": func(m map[string]any) { m["node_id"] = "758" },
		"no token":        func(m map[string]any) { m["device_api_token"] = "" },
		"plain http base": func(m map[string]any) { m["api_base_url"] = "http://platform.example" },
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m := goodEnrollResponse("http://" + r.Host)
			mutate(m)
			_ = json.NewEncoder(w).Encode(m)
		}))
		_, err := Client{}.Enroll(context.Background(), srv.URL, testToken, "csr", testLabels())
		srv.Close()
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := (Client{}).Enroll(context.Background(), "http://platform.example", testToken, "csr", testLabels()); err == nil {
		t.Error("a non-loopback http enroll URL was accepted")
	}
}

func TestEnrollNeverFollowsRedirects(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+EnrollPath, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	if _, err := (Client{}).Enroll(context.Background(), srv.URL, testToken, "csr", testLabels()); err == nil {
		t.Fatal("a redirect was treated as success")
	}
	if hits.Load() != 0 {
		t.Fatal("the token was sent to a redirect target")
	}
}

func TestDeregister(t *testing.T) {
	var auth string
	reply := `{"success":true,"node_id":"` + testNodeID + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != DeregisterPath || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	if err := (Client{}).Deregister(context.Background(), srv.URL, "dev_tok", testNodeID); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer dev_tok" {
		t.Fatalf("authorization = %q", auth)
	}
	reply = `{"success":true,"node_id":"hpc-ffffffffffffffffffffffffffffffff"}`
	if err := (Client{}).Deregister(context.Background(), srv.URL, "dev_tok", testNodeID); err == nil {
		t.Fatal("a different node id was accepted")
	}
	reply = `{"success":false}`
	if err := (Client{}).Deregister(context.Background(), srv.URL, "dev_tok", testNodeID); err == nil {
		t.Fatal("an unconfirmed deregister was accepted")
	}
}

func TestHTTPClientHonorsProxyEnvironment(t *testing.T) {
	tr, ok := NewHTTPClient().Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("the enroll client has no proxy function")
	}
	if reflect.ValueOf(tr.Proxy).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		t.Fatal("the enroll client does not use the environment proxy")
	}
}

func TestReadTokenFileRefusesFIFOWithoutBlocking(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := ReadTokenFile(p, os.Getuid()); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ReadTokenFile blocked on a FIFO")
	}
	if _, err := os.Lstat(p); err != nil {
		t.Fatal("a refused file must be left in place")
	}
}

func TestVerifyLeafKey(t *testing.T) {
	mk := func() (*ecdsa.PrivateKey, string) {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
		if err != nil {
			t.Fatal(err)
		}
		return k, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	k1, leaf1 := mk()
	k2, _ := mk()
	if err := VerifyLeafKey(leaf1, &k1.PublicKey); err != nil {
		t.Fatalf("matching leaf refused: %v", err)
	}
	if err := VerifyLeafKey(leaf1, &k2.PublicKey); err == nil {
		t.Fatal("a leaf for another key was accepted")
	}
	if err := VerifyLeafKey("", &k1.PublicKey); err == nil {
		t.Fatal("an empty leaf was accepted")
	}
	if err := VerifyLeafKey("not pem", &k1.PublicKey); err == nil {
		t.Fatal("garbage was accepted")
	}
}
