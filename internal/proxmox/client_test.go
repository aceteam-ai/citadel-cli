package proxmox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	insecure := true
	client := NewClient(ClientConfig{
		BaseURL:            srv.URL,
		TokenID:            "test@pve!testtoken",
		TokenSecret:        "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		InsecureSkipVerify: &insecure,
	})
	client.httpClient = srv.Client()
	return srv, client
}

func wrapData(t *testing.T, v interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling test data: %v", err)
	}
	return []byte(`{"data":` + string(data) + `}`)
}

func TestListNodes(t *testing.T) {
	nodes := []Node{
		{Node: "pve1", Status: "online", MaxCPU: 16, Uptime: 3600},
		{Node: "pve2", Status: "offline", MaxCPU: 8},
	}

	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", 404)
			return
		}
		w.Write(wrapData(t, nodes))
	})

	result, err := client.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(result))
	}
	if result[0].Node != "pve1" {
		t.Errorf("expected node pve1, got %s", result[0].Node)
	}
	if result[1].Status != "offline" {
		t.Errorf("expected offline, got %s", result[1].Status)
	}
}

func TestListVMs(t *testing.T) {
	vms := []Guest{
		{VMID: 100, Name: "web-server", Status: "running", CPUs: 4, Uptime: 7200},
		{VMID: 101, Name: "db-server", Status: "stopped", CPUs: 2},
	}

	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/qemu" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", 404)
			return
		}
		w.Write(wrapData(t, vms))
	})

	result, err := client.ListVMs(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 VMs, got %d", len(result))
	}
	if result[0].Type != "qemu" {
		t.Errorf("expected type qemu, got %s", result[0].Type)
	}
	if result[0].Name != "web-server" {
		t.Errorf("expected web-server, got %s", result[0].Name)
	}
}

func TestListContainers(t *testing.T) {
	cts := []Guest{
		{VMID: 200, Name: "nginx-proxy", Status: "running", CPUs: 1},
	}

	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/lxc" {
			http.Error(w, "not found", 404)
			return
		}
		w.Write(wrapData(t, cts))
	})

	result, err := client.ListContainers(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 container, got %d", len(result))
	}
	if result[0].Type != "lxc" {
		t.Errorf("expected type lxc, got %s", result[0].Type)
	}
}

func TestListAllGuests(t *testing.T) {
	vms := []Guest{{VMID: 100, Name: "vm1", Status: "running"}}
	cts := []Guest{{VMID: 200, Name: "ct1", Status: "stopped"}}

	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/nodes/pve1/qemu":
			w.Write(wrapData(t, vms))
		case "/api2/json/nodes/pve1/lxc":
			w.Write(wrapData(t, cts))
		default:
			http.Error(w, "not found", 404)
		}
	})

	result, err := client.ListAllGuests(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("ListAllGuests: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 guests, got %d", len(result))
	}
	if result[0].Type != "qemu" || result[1].Type != "lxc" {
		t.Errorf("expected qemu+lxc, got %s+%s", result[0].Type, result[1].Type)
	}
}

func TestStartGuest(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api2/json/nodes/pve1/qemu/100/status/start" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"data":"UPID:pve1:00001234:00000000:12345678:qmstart:100:root@pam:"}`))
	})

	if err := client.StartGuest(context.Background(), "pve1", "qemu", 100); err != nil {
		t.Fatalf("StartGuest: %v", err)
	}
}

func TestShutdownGuest(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/qemu/101/status/shutdown" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"data":"UPID:pve1:00001234:00000000:12345678:qmshutdown:101:root@pam:"}`))
	})

	if err := client.ShutdownGuest(context.Background(), "pve1", "qemu", 101); err != nil {
		t.Fatalf("ShutdownGuest: %v", err)
	}
}

func TestStopGuest(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/lxc/200/status/stop" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"data":"UPID:pve1:00001234:00000000:12345678:vzstop:200:root@pam:"}`))
	})

	if err := client.StopGuest(context.Background(), "pve1", "lxc", 200); err != nil {
		t.Fatalf("StopGuest: %v", err)
	}
}

func TestRebootGuest(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/qemu/100/status/reboot" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"data":"UPID:pve1:00001234:00000000:12345678:qmreboot:100:root@pam:"}`))
	})

	if err := client.RebootGuest(context.Background(), "pve1", "qemu", 100); err != nil {
		t.Fatalf("RebootGuest: %v", err)
	}
}

func TestGetGuestStatus(t *testing.T) {
	status := GuestStatus{
		VMID: 100, Status: "running", Name: "web-server",
		CPUs: 4, Uptime: 7200, PID: 12345,
	}

	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/qemu/100/status/current" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write(wrapData(t, status))
	})

	result, err := client.GetGuestStatus(context.Background(), "pve1", "qemu", 100)
	if err != nil {
		t.Fatalf("GetGuestStatus: %v", err)
	}
	if result.Status != "running" {
		t.Errorf("expected running, got %s", result.Status)
	}
	if result.PID != 12345 {
		t.Errorf("expected PID 12345, got %d", result.PID)
	}
}

func TestListStorage(t *testing.T) {
	pools := []StoragePool{
		{Storage: "local", Type: "dir", Total: 100 * 1024 * 1024 * 1024, Active: 1, Enabled: 1},
		{Storage: "local-lvm", Type: "lvmthin", Active: 1, Enabled: 1},
	}

	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/storage" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write(wrapData(t, pools))
	})

	result, err := client.ListStorage(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("ListStorage: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 pools, got %d", len(result))
	}
}

func TestAPIError(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"errors":{"username":"invalid credentials"}}`))
	})

	_, err := client.ListNodes(context.Background())
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestPing(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/version" {
			http.Error(w, "not found", 404)
			return
		}
		w.Write([]byte(`{"data":{"version":"8.2.4","release":"8.2","repoid":"80dee2c6"}}`))
	})

	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestAuthHeader(t *testing.T) {
	var gotAuth string
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"data":[]}`))
	})

	_, _ = client.ListNodes(context.Background())

	expected := "PVEAPIToken=test@pve!testtoken=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if gotAuth != expected {
		t.Errorf("auth header = %q, want %q", gotAuth, expected)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()

	cfg := &Config{
		BaseURL:     "https://192.168.2.4:8006",
		TokenID:     "root@pam!citadel",
		TokenSecret: "12345678-abcd-efgh-ijkl-123456789012",
	}

	if err := SaveConfig(dir, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	path := filepath.Join(dir, configFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("file permissions = %o, want 0600", perm)
	}

	loaded, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.BaseURL != cfg.BaseURL {
		t.Errorf("BaseURL = %q, want %q", loaded.BaseURL, cfg.BaseURL)
	}
	if loaded.TokenID != cfg.TokenID {
		t.Errorf("TokenID = %q, want %q", loaded.TokenID, cfg.TokenID)
	}
	if loaded.TokenSecret != cfg.TokenSecret {
		t.Errorf("TokenSecret = %q, want %q", loaded.TokenSecret, cfg.TokenSecret)
	}
}

func TestLoadConfig_NotExist(t *testing.T) {
	cfg, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg != nil {
		t.Error("expected nil config for missing file")
	}
}

func TestIsConfigured(t *testing.T) {
	dir := t.TempDir()

	if IsConfigured(dir) {
		t.Error("expected not configured for empty dir")
	}

	_ = SaveConfig(dir, &Config{BaseURL: "https://pve:8006"})
	if !IsConfigured(dir) {
		t.Error("expected configured after saving config")
	}
}
