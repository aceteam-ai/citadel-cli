package cmd

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

func platformTestSeams(t *testing.T, who func(context.Context, string) (*network.PeerIdentity, error)) *net.Listener {
	t.Helper()
	dir := t.TempDir()
	origListen, origWho, origDir, origMode := platformListenVPN, platformWhoIsPeer, platformConfigDir, platformMode
	var listener net.Listener
	platformListenVPN = func(_, _ string) (net.Listener, string, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		listener = ln
		return ln, "100.64.0.9", err
	}
	platformWhoIsPeer = who
	platformConfigDir = func() string { return dir }
	platformMode = func() network.BackendMode { return network.ModeUserspace }
	t.Cleanup(func() {
		for name := range platformExposures {
			stopPlatformExposure(name)
		}
		platformListenVPN, platformWhoIsPeer, platformConfigDir, platformMode = origListen, origWho, origDir, origMode
	})
	return &listener
}

func TestPlatformExposureIdentityAndLifecycle(t *testing.T) {
	gw := setupExposeOpsTest(t)
	var identityMu sync.RWMutex
	var identity *network.PeerIdentity
	var whoErr error
	setPeer := func(id *network.PeerIdentity, err error) {
		identityMu.Lock()
		identity, whoErr = id, err
		identityMu.Unlock()
	}
	listener := platformTestSeams(t, func(context.Context, string) (*network.PeerIdentity, error) {
		identityMu.RLock()
		defer identityMu.RUnlock()
		return identity, whoErr
	})
	fakeListen := platformListenVPN
	platformListenVPN = func(network, port string) (net.Listener, string, error) {
		if port != "80" {
			t.Errorf("VPN listener port = %q, want same requested port", port)
		}
		return fakeListen(network, port)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.Host) }))
	defer target.Close()
	addr := strings.TrimPrefix(target.URL, "http://")
	ops := liveExposeOps{}
	const port = 80
	res, err := ops.Expose(context.Background(), worker.ExposeRequest{Name: "web", Port: port, Visibility: "platform", ForwardTarget: addr})
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "http://100.64.0.9:80/" {
		t.Fatalf("URL = %q", res.URL)
	}
	if gw.ExposureNames() != nil && len(gw.ExposureNames()) != 0 {
		t.Fatal("platform listener unexpectedly installed a gateway route")
	}
	request := func() (string, error) {
		client := http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		req, _ := http.NewRequest("GET", "http://"+(*listener).Addr().String()+"/asset", nil)
		req.Host = "example.test"
		req.Close = true
		response, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		return string(body), err
	}
	setPeer(&network.PeerIdentity{NodeName: "peer", OwnerID: "userid:7", SameOwner: true}, nil)
	if body, err := request(); err != nil || body != "example.test" {
		t.Fatalf("same owner: %q, %v", body, err)
	}
	setPeer(&network.PeerIdentity{NodeName: "peer", SameOwner: true}, nil)
	if body, err := request(); err == nil {
		t.Fatalf("same-owner claim without a stable owner ID admitted %q", body)
	}
	if err := os.WriteFile(filepath.Join(platformConfigDir(), "platform-exposure.json"), []byte(`{invalid`), 0o600); err != nil {
		t.Fatal(err)
	}
	if body, err := request(); err == nil {
		t.Fatalf("malformed node allowlist admitted %q", body)
	}
	if err := os.Remove(filepath.Join(platformConfigDir(), "platform-exposure.json")); err != nil {
		t.Fatal(err)
	}
	setPeer(&network.PeerIdentity{NodeName: "peer", LoginName: "other", SameOwner: false}, nil)
	if body, err := request(); err == nil {
		t.Fatalf("different owner got %q", body)
	}
	setPeer(nil, nil)
	if body, err := request(); err == nil {
		t.Fatalf("unknown peer got %q", body)
	}
	setPeer(nil, errors.New("unverified"))
	if body, err := request(); err == nil {
		t.Fatalf("resolver error got %q", body)
	}
	setPeer(nil, nil)
	if err := os.WriteFile(filepath.Join(platformConfigDir(), "platform-exposure.json"), []byte(`{"allowed_logins":["infra"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	setPeer(&network.PeerIdentity{NodeName: "peer", LoginName: "infra"}, nil)
	if body, err := request(); err != nil || body != "example.test" {
		t.Fatalf("allowlisted peer: %q, %v", body, err)
	}
	list, err := ops.List(context.Background())
	if err != nil || len(list.Exposures) != 1 || !list.Exposures[0].Live || list.Exposures[0].ForwardTarget != addr || list.Exposures[0].URL != res.URL {
		t.Fatalf("list: %+v, %v", list, err)
	}
	if _, err := ops.Expose(context.Background(), worker.ExposeRequest{Name: "other", Port: port, Visibility: "platform"}); err == nil {
		t.Fatal("same port was accepted twice")
	}
	stopPlatformExposure("web")
	restoreExposures(gw)
	list, err = ops.List(context.Background())
	if err != nil || !list.Exposures[0].Live {
		t.Fatalf("restore/list: %+v, %v", list, err)
	}
	removed, err := ops.Unexpose(context.Background(), "web")
	if err != nil || !removed.WasExposed || !removed.DurableRecordRemoved {
		t.Fatalf("unexpose: %+v, %v", removed, err)
	}
	if len(platformExposures) != 0 {
		t.Fatal("listener survived unexpose")
	}
}

func TestPlatformExposureBindModeAndTargetValidation(t *testing.T) {
	setupExposeOpsTest(t)
	platformTestSeams(t, func(context.Context, string) (*network.PeerIdentity, error) { return nil, nil })
	if target, err := platformTarget(80, ""); err != nil || target != "127.0.0.1:80" {
		t.Fatalf("default target = %q, %v", target, err)
	}
	if _, err := platformTarget(80, "example.com:80"); err == nil {
		t.Fatal("DNS target accepted")
	}
	if _, err := platformTarget(80, "8.8.8.8:80"); err == nil {
		t.Fatal("public target accepted")
	}
	if target, err := platformTarget(80, "10.0.0.8:8080"); err != nil || target != "10.0.0.8:8080" {
		t.Fatalf("private target = %q, %v", target, err)
	}
	if err := os.WriteFile(filepath.Join(platformConfigDir(), "platform-exposure.json"), []byte(`{"allowed_logins":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (liveExposeOps{}).Expose(context.Background(), worker.ExposeRequest{Name: "web", Port: 80, Visibility: "platform"}); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("malformed allowlist startup error = %v", err)
	}
	if len(platformExposures) != 0 {
		t.Fatal("listener started with a malformed allowlist")
	}
	if err := os.Remove(filepath.Join(platformConfigDir(), "platform-exposure.json")); err != nil {
		t.Fatal(err)
	}
	platformListenVPN = func(_, _ string) (net.Listener, string, error) { return nil, "", errors.New("permission denied") }
	platformMode = func() network.BackendMode { return network.ModeTUN }
	_, err := (liveExposeOps{}).Expose(context.Background(), worker.ExposeRequest{Name: "web", Port: 80, Visibility: "platform"})
	if err == nil || !strings.Contains(err.Error(), "tun mode") || !strings.Contains(err.Error(), "CAP_NET_BIND_SERVICE") {
		t.Fatalf("bind error = %v", err)
	}
}
