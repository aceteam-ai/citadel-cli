//go:build !windows

package cmd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/hpc"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
)

func testCertPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "leaf"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

type fakeEnrollServer struct {
	*httptest.Server
	calls  atomic.Int32
	deregs atomic.Int32
	csr    atomic.Value
	// leaf, when set, replaces the certificate issued for the CSR key.
	leaf *string
}

// certForCSR issues a self-signed certificate for the CSR's public key.
func certForCSR(t *testing.T, csrPEM string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		t.Fatal("no CSR in the request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "leaf"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, csr.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func newFakeEnrollServer(t *testing.T) *fakeEnrollServer {
	f := &fakeEnrollServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == hpc.DeregisterPath {
			f.deregs.Add(1)
			_, _ = w.Write([]byte(`{"success":true,"node_id":"` + testHPCNodeID + `"}`))
			return
		}
		f.calls.Add(1)
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		f.csr.Store(req["csr_pem"])
		leaf := certForCSR(t, req["csr_pem"].(string))
		if f.leaf != nil {
			leaf = *f.leaf
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id": testHPCNodeID, "org_id": "org-1", "device_api_token": "dev_tok",
			"api_base_url": "http://" + r.Host, "leaf_pem": leaf, "chain_pem": leaf,
			"node_uid": "uid-1", "expires_at": "2026-11-12T12:05:00Z",
		})
	}))
	t.Cleanup(f.Close)
	return f
}

func testEnrollLayout(t *testing.T) hpc.Layout {
	t.Helper()
	l, err := hpc.ResolveLayout(hpc.Inputs{JobID: "4242", TmpDir: privateTempDir(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := hpc.Prepare(l, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	return l
}

func testEnrollOptions(t *testing.T, base, tokenFile string) hpcEnrollOptions {
	now := time.Now()
	env := map[string]string{
		hpc.EnvClusterName: "testcluster", hpc.EnvPartition: "gpu", hpc.EnvAccount: "lab-a",
		hpc.EnvJobID: "4242", hpc.EnvNodeName: "gpu-node-07", hpc.EnvVisibleGPUs: "0",
		hpc.EnvJobEndTime: strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
	}
	return hpcEnrollOptions{
		TokenFile: tokenFile, BaseURL: base, Client: hpc.Client{HTTP: hpc.NewHTTPClient()},
		Getenv: func(k string) string { return env[k] }, Now: now, UID: os.Getuid(),
	}
}

func writeTestToken(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte("hpcet_token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnrollHPCNodePersistsIntoJobRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the hpc profile refuses root")
	}
	srv := newFakeEnrollServer(t)
	layout := testEnrollLayout(t)
	token := writeTestToken(t)

	creds, err := enrollHPCNode(context.Background(), layout, testEnrollOptions(t, srv.URL, token))
	if err != nil {
		t.Fatal(err)
	}
	if creds.NodeID != testHPCNodeID || creds.OrgID != "org-1" || creds.Token != "dev_tok" {
		t.Fatalf("creds = %+v", creds)
	}
	if _, err := os.Stat(token); !os.IsNotExist(err) {
		t.Fatal("the token file was not removed")
	}
	info, err := os.Stat(hpcDeviceConfigPath(layout.NodeDir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config.yaml: %v %v", info, err)
	}
	// The worker's own reader sees the platform node id.
	dc := readDeviceConfigFromDirs([]string{layout.NodeDir})
	if dc == nil || dc.FabricNodeID != testHPCNodeID || dc.DeviceAPIToken != "dev_tok" {
		t.Fatalf("device config = %+v", dc)
	}
	// The CSR came from the job-local key, and the leaf is stored beside it.
	store := nodeidentity.New(filepath.Join(layout.NodeDir, "identity"))
	if !store.HasLeaf() {
		t.Fatal("fabric leaf not stored in the job root")
	}
	block, _ := pem.Decode([]byte(srv.csr.Load().(string)))
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := store.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Equal(csr.PublicKey) {
		t.Fatal("the CSR is not for the job-local key")
	}

	// A second enrollment in the same root is refused before any network call.
	if _, err := enrollHPCNode(context.Background(), layout, testEnrollOptions(t, srv.URL, writeTestToken(t))); err == nil {
		t.Fatal("second enrollment accepted")
	}
	if srv.calls.Load() != 1 {
		t.Fatalf("enroll calls = %d", srv.calls.Load())
	}
}

func TestEnrollHPCNodeLabelProblemKeepsToken(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the hpc profile refuses root")
	}
	srv := newFakeEnrollServer(t)
	token := writeTestToken(t)
	opts := testEnrollOptions(t, srv.URL, token)
	opts.Getenv = func(string) string { return "" }
	if _, err := enrollHPCNode(context.Background(), testEnrollLayout(t), opts); err == nil {
		t.Fatal("enrollment without Slurm labels accepted")
	}
	if _, err := os.Stat(token); err != nil {
		t.Fatal("a labeling problem consumed the token")
	}
	if srv.calls.Load() != 0 {
		t.Fatal("the platform was called")
	}
}

func TestEnrollHPCNodeRefusesAndDeregistersOnLocalFailureAfterEnroll(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the hpc profile refuses root")
	}
	other := testCertPEM(t) // a certificate for some other key
	for name, leaf := range map[string]string{"leaf for another key": other, "empty leaf": ""} {
		srv := newFakeEnrollServer(t)
		srv.leaf = &leaf
		layout := testEnrollLayout(t)
		_, err := enrollHPCNode(context.Background(), layout, testEnrollOptions(t, srv.URL, writeTestToken(t)))
		if err == nil {
			t.Fatalf("%s: enrollment accepted", name)
		}
		if srv.deregs.Load() != 1 {
			t.Fatalf("%s: the new node was not deregistered (deregs=%d)", name, srv.deregs.Load())
		}
		if _, statErr := os.Stat(hpcDeviceConfigPath(layout.NodeDir)); !os.IsNotExist(statErr) {
			t.Fatalf("%s: a credential was left on disk", name)
		}
		if nodeidentity.New(filepath.Join(layout.NodeDir, "identity")).HasLeaf() {
			t.Fatalf("%s: a mismatched leaf was stored", name)
		}
	}
}

func TestEnrollHPCNodeLocalKeyFailureKeepsToken(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the hpc profile refuses root")
	}
	srv := newFakeEnrollServer(t)
	layout := testEnrollLayout(t)
	// A file where the identity directory must go: key generation fails.
	if err := os.WriteFile(filepath.Join(layout.NodeDir, "identity"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	token := writeTestToken(t)
	if _, err := enrollHPCNode(context.Background(), layout, testEnrollOptions(t, srv.URL, token)); err == nil {
		t.Fatal("enrollment succeeded without a key")
	}
	if _, err := os.Stat(token); err != nil {
		t.Fatal("a local key failure consumed the token")
	}
	if srv.calls.Load() != 0 {
		t.Fatal("the platform was called")
	}
}

type fakeDrainTarget struct {
	mu      sync.Mutex
	drained bool
	active  int
	order   *[]string
}

func (f *fakeDrainTarget) Drain() {
	f.mu.Lock()
	f.drained = true
	*f.order = append(*f.order, "drain")
	f.mu.Unlock()
}
func (f *fakeDrainTarget) ActiveJobs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.drained && f.active > 0 {
		f.active--
		return f.active + 1
	}
	return f.active
}

func newTestShutdown(order *[]string, deregs *atomic.Int32) *hpcShutdown {
	var mu sync.Mutex
	add := func(s string) { mu.Lock(); *order = append(*order, s); mu.Unlock() }
	return &hpcShutdown{
		deregister:   func(context.Context) error { deregs.Add(1); add("deregister"); return nil },
		cancel:       func() { add("cancel") },
		drainTimeout: time.Second,
		poll:         time.Millisecond,
	}
}

func TestHPCShutdownDrainOrder(t *testing.T) {
	var order []string
	var deregs atomic.Int32
	s := newTestShutdown(&order, &deregs)
	target := &fakeDrainTarget{active: 3, order: &order}
	s.setTarget(target)
	s.drain(context.Background())
	if got := []string{"drain", "deregister", "cancel"}; len(order) != 3 || order[0] != got[0] || order[1] != got[1] || order[2] != got[2] {
		t.Fatalf("order = %v, want %v", order, got)
	}
	if target.ActiveJobs() != 0 {
		t.Fatal("deregistered before in-flight jobs finished")
	}
	s.terminate()
	if deregs.Load() != 1 {
		t.Fatalf("deregister ran %d times", deregs.Load())
	}
}

func TestHPCShutdownDrainIsBounded(t *testing.T) {
	var order []string
	var deregs atomic.Int32
	s := newTestShutdown(&order, &deregs)
	s.drainTimeout = 50 * time.Millisecond
	stuck := &stuckTarget{}
	s.setTarget(stuck)
	start := time.Now()
	s.drain(context.Background())
	if time.Since(start) > 2*time.Second || deregs.Load() != 1 {
		t.Fatalf("drain took %s, deregs %d", time.Since(start), deregs.Load())
	}
}

type stuckTarget struct{}

func (stuckTarget) Drain()          {}
func (stuckTarget) ActiveJobs() int { return 1 }

func TestWatchHPCSignalsDispatch(t *testing.T) {
	var order []string
	var deregs atomic.Int32
	s := newTestShutdown(&order, &deregs)
	term := make(chan os.Signal, 1)
	drain := make(chan os.Signal, 1)
	term <- syscall.SIGTERM
	watchHPCSignals(context.Background(), term, drain, s)
	if deregs.Load() != 1 || order[len(order)-1] != "cancel" {
		t.Fatalf("terminate: order %v", order)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	watchHPCSignals(ctx, make(chan os.Signal), make(chan os.Signal), s) // returns on ctx
}

// TestWatchHPCSignalsRealUSR1 sends a real SIGUSR1 to this process (with the
// handler already registered) and checks it drains and deregisters.
func TestWatchHPCSignalsRealUSR1(t *testing.T) {
	var order []string
	var deregs atomic.Int32
	s := newTestShutdown(&order, &deregs)
	s.setTarget(&fakeDrainTarget{active: 1, order: &order})
	drain := make(chan os.Signal, 1)
	signalNotify(drain, hpcDrainSignals...)
	defer signalStop(drain)
	done := make(chan struct{})
	go func() { watchHPCSignals(context.Background(), make(chan os.Signal), drain, s); close(done) }()
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGUSR1 did not drain")
	}
	if deregs.Load() != 1 || order[0] != "drain" {
		t.Fatalf("order %v", order)
	}
}

// TestWatchHPCSignalsTermDuringDrainDeregistersAtOnce is the scancel case:
// the drain signal arrives, then SIGTERM while jobs still run. The node must
// deregister right away, not after the drain timeout.
func TestWatchHPCSignalsTermDuringDrainDeregistersAtOnce(t *testing.T) {
	var order []string
	var deregs atomic.Int32
	s := newTestShutdown(&order, &deregs)
	s.drainTimeout = time.Minute
	s.setTarget(&stuckTarget{})
	term := make(chan os.Signal, 1)
	drain := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() { watchHPCSignals(context.Background(), term, drain, s); close(done) }()
	drain <- syscall.SIGUSR1
	time.Sleep(50 * time.Millisecond) // the drain is now waiting on the stuck job
	if deregs.Load() != 0 {
		t.Fatal("deregistered before the drain finished or a term signal")
	}
	start := time.Now()
	term <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM during the drain was ignored")
	}
	if time.Since(start) > 3*time.Second || deregs.Load() != 1 {
		t.Fatalf("took %s, deregs %d", time.Since(start), deregs.Load())
	}
}
