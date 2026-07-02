package whatsapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateAdminKey(t *testing.T) {
	k1, err := GenerateAdminKey()
	if err != nil {
		t.Fatalf("GenerateAdminKey() error = %v", err)
	}
	k2, _ := GenerateAdminKey()
	if k1 == k2 {
		t.Errorf("expected distinct keys, got identical: %s", k1)
	}
	if !strings.HasPrefix(k1, "wab_admin_") {
		t.Errorf("expected wab_admin_ prefix, got %s", k1)
	}
	if len(k1) < 40 {
		t.Errorf("admin key too short: %d chars", len(k1))
	}
}

func TestEnvRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := map[string]string{
		"ADMIN_API_KEY":  "wab_admin_secret",
		"BRIDGE_PORT":    "8081",
		"TENANT_API_KEY": "wab_tenant_xyz",
	}
	if err := SaveEnv(dir, in); err != nil {
		t.Fatalf("SaveEnv() error = %v", err)
	}
	out, err := LoadEnv(dir)
	if err != nil {
		t.Fatalf("LoadEnv() error = %v", err)
	}
	for k, v := range in {
		if out[k] != v {
			t.Errorf("env[%q] = %q, want %q", k, out[k], v)
		}
	}
	// File must be 0600 (holds the admin secret).
	info, err := os.Stat(EnvPath(dir))
	if err != nil {
		t.Fatalf("stat env file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("env file perm = %o, want 600", perm)
	}
}

func TestLoadEnvMissingFile(t *testing.T) {
	out, err := LoadEnv(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("LoadEnv() on missing dir error = %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty map, got %v", out)
	}
}

func TestRenderQR(t *testing.T) {
	if got := RenderQRANSI(""); got != "" {
		t.Errorf("RenderQRANSI(empty) = %q, want empty", got)
	}
	if got := RenderQRBlocks(""); got != "" {
		t.Errorf("RenderQRBlocks(empty) = %q, want empty", got)
	}
	ansi := RenderQRANSI("2@abc123,def456==")
	if ansi == "" {
		t.Error("RenderQRANSI returned empty for non-empty payload")
	}
	blocks := RenderQRBlocks("2@abc123,def456==")
	if !strings.Contains(blocks, "█") {
		t.Errorf("RenderQRBlocks did not contain block chars: %q", blocks)
	}
}

func TestCreateTenant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/tenants" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("X-Admin-Key") != "admin-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"t1","name":"default","api_key":"wab_tenant_abc","qr_url":"/qr?key=wab_tenant_abc"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "admin-secret")
	tenant, err := c.CreateTenant(context.Background(), "default", "")
	if err != nil {
		t.Fatalf("CreateTenant() error = %v", err)
	}
	if tenant.APIKey != "wab_tenant_abc" {
		t.Errorf("api_key = %q, want wab_tenant_abc", tenant.APIKey)
	}

	// Wrong admin key must surface an error.
	bad := NewClient(srv.URL, "wrong")
	if _, err := bad.CreateTenant(context.Background(), "default", ""); err == nil {
		t.Error("expected error with wrong admin key, got nil")
	}
}

func TestQRStringAndHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "tenant-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/qr.txt":
			w.Write([]byte("2@rawqr==\n"))
		case "/health":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"status":"ok","connected":true,"logged_in":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	qr, err := c.QRString(context.Background(), "tenant-key")
	if err != nil {
		t.Fatalf("QRString() error = %v", err)
	}
	if qr != "2@rawqr==" {
		t.Errorf("QRString() = %q, want trimmed 2@rawqr==", qr)
	}
	h, err := c.Health(context.Background(), "tenant-key")
	if err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if !h.LoggedIn {
		t.Error("expected logged_in true")
	}
}

// TestProjectNamePreservesExistingVolume is the regression guard for the
// container-name collision fix (aceteam-ai/citadel-cli#436): the pinned compose
// project MUST equal what compose derived implicitly before the fix (the
// services-dir basename, "services"), otherwise upgrading an already-deployed
// node would move its `<project>_whatsapp_pgdata` volume and silently unlink the
// user's WhatsApp session.
func TestProjectNamePreservesExistingVolume(t *testing.T) {
	// Every node's bridge lives in <configDir>/services, so the project -- and
	// thus the auth-state volume prefix -- must remain "services".
	if got := ProjectName(filepath.Join("/home/user/citadel-node", "services")); got != "services" {
		t.Errorf("ProjectName(...services) = %q, want %q (must match the pre-fix implicit project so the pgdata volume is preserved)", got, "services")
	}
}

// TestProjectNameSanitizes checks the derived project is always a value docker
// compose accepts (lowercase [a-z0-9_-], non-empty).
func TestProjectNameSanitizes(t *testing.T) {
	cases := map[string]string{
		"services":        "services",
		"Services":        "services",
		"My Services":     "my-services",
		"svc.dir":         "svc-dir",
		"---":             "services", // degenerate -> fallback
		"":                "services",
		"node_1-services": "node_1-services",
	}
	for in, want := range cases {
		if got := ProjectName(in); got != want {
			t.Errorf("ProjectName(%q) = %q, want %q", in, got, want)
		}
	}
}
