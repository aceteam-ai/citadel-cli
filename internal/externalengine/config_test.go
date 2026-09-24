package externalengine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestEndpointValidationAndFormatting(t *testing.T) {
	for _, host := range []string{"http://127.0.0.1", "127.0.0.1/path", "169.254.169.254", "fe80::1", "::", "224.0.0.1", "[::1]", "::ffff:127.0.0.1", "127.0.0.01"} {
		if _, err := ParseHost(host); err == nil {
			t.Errorf("unsafe host %q accepted", host)
		}
	}
	e, err := ValidateEndpoint(Endpoint{Host: "localhost", Port: 58000}, true)
	if err != nil || e.BaseURL() != "http://127.0.0.1:58000" {
		t.Fatalf("loopback = %v, %v", e, err)
	}
	e, err = ValidateEndpoint(Endpoint{Host: "::1", Port: 58000}, true)
	if err != nil || e.BaseURL() != "http://[::1]:58000" {
		t.Fatalf("IPv6 = %v, %v", e, err)
	}
}

func TestNonLoopbackRequiresAssignedInterface(t *testing.T) {
	local := func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.0.2.10"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	e, err := validateEndpoint(Endpoint{Host: "192.0.2.10", Port: 58000}, true, local)
	if err != nil || e.BaseURL() != "http://192.0.2.10:58000" {
		t.Fatalf("assigned endpoint: %v, %v", e, err)
	}
	if _, err := validateEndpoint(Endpoint{Host: "192.0.2.11", Port: 58000}, true, local); err == nil {
		t.Fatal("unassigned endpoint accepted")
	}
}

func TestConfigRefGoldenVector(t *testing.T) {
	c := Config{Mode: "adopted", Endpoint: Endpoint{Host: "127.0.0.1", Port: 58000}, Model: "example/model", Revision: "7"}
	const want = "external-vllm-sha256:ec2c0933d8fa3eab999db8d5f1d4c45c74afc1d7a583dbf2890388c4116d7f8b"
	if got := Ref(c); got != want {
		t.Fatalf("config_ref = %q, want %q", got, want)
	}
}

func TestAtomicPersistenceAndDetachTombstone(t *testing.T) {
	dir := t.TempDir()
	first, err := Validate(Config{Version: 1, Mode: "adopted", Endpoint: Endpoint{Host: "127.0.0.1", Port: 58000}, Model: "vendor/model", Revision: "1", RequestID: "00000000-0000-4000-8000-000000000001", NodeID: "12"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, first); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	detached := first
	detached.Mode = "detached"
	detached.Revision = "2"
	detached.RequestID = "00000000-0000-4000-8000-000000000002"
	detached.ConfigRef = Ref(detached)
	if err := Save(dir, detached); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil || loaded.Mode != "detached" || loaded.Endpoint != first.Endpoint || loaded.Model != first.Model || loaded.ConfigRef != detached.ConfigRef {
		t.Fatalf("loaded = %+v, %v", loaded, err)
	}
	backup, err := os.ReadFile(Path(dir) + ".bak")
	if err != nil || !strings.Contains(string(backup), `"mode":"adopted"`) {
		t.Fatalf("backup = %s, %v", backup, err)
	}
}

func TestLoadPersistedAllowsFormerLocalAddressForRecovery(t *testing.T) {
	dir := t.TempDir()
	stored, err := Validate(Config{Version: 1, Mode: "adopted", Endpoint: Endpoint{Host: "192.0.2.10", Port: 58000}, Model: "vendor/model", Revision: "1", RequestID: "00000000-0000-4000-8000-000000000001", NodeID: "12"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("runtime load accepted an endpoint that is not assigned to this node")
	}
	loaded, err := LoadPersisted(dir)
	if err != nil || loaded == nil || *loaded != stored {
		t.Fatalf("reconciliation load = %+v, %v", loaded, err)
	}
}

func TestProbeRequiresExactModelAndDoesNotRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"data":[{"id":"vendor/model"}]}`)) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/models", http.StatusFound)
	}))
	defer server.Close()
	parse := func(raw string) Endpoint {
		u, _ := url.Parse(raw)
		e := Endpoint{Host: u.Hostname()}
		_, _ = fmt.Sscan(u.Port(), &e.Port)
		return e
	}
	if err := Probe(context.Background(), parse(target.URL), "vendor/model"); err != nil {
		t.Fatal(err)
	}
	if err := Probe(context.Background(), parse(target.URL), "vendor/other"); err == nil {
		t.Fatal("model mismatch accepted")
	}
	if err := Probe(context.Background(), parse(server.URL), "vendor/model"); err == nil {
		t.Fatal("redirect accepted")
	}
}
