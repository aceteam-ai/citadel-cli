package status

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateSSHPublicKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{
			name:    "valid ed25519",
			key:     "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test@example.com",
			wantErr: false,
		},
		{
			name:    "valid rsa",
			key:     "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC5O9ePrgLJt/F7v+5Xy8+pHTzK8SKBd6xVlqmJskER6W0lcbXfaqVvhePaBE0I/M/Wo75zt/gTCXHIN2k1nfjDknIHKLo1Bzqiz2Vyv3PvOjidB8lQC1gZLhWVco1Qu+87OHTe0JxbW7nTGlGEHGHhd1rjv6mJrt5UZM7swHEA9xgFgTEuYTYu0Zqr8zQhySl/AOdHwUsr42FkmRQ1G96yD152j2ijeZoVossA/3XFLAthG1dwh3s7kl4OJSH+e8KEtgDKQ/+G7silo4RiZfth/b1QPqoR7c6eW75bpdY7NXXH2mYQNH6deo4fsUdQfnRhFG/rp5wQ96EQsvafreX5 test@example.com",
			wantErr: false,
		},
		{
			name:    "valid ed25519 without comment",
			key:     "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl",
			wantErr: false,
		},
		{
			name:    "empty key",
			key:     "",
			wantErr: true,
		},
		{
			name:    "whitespace only",
			key:     "   ",
			wantErr: true,
		},
		{
			name:    "garbage data",
			key:     "not-a-key at-all",
			wantErr: true,
		},
		{
			name:    "truncated key",
			key:     "ssh-ed25519 AAAA",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSSHPublicKey(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSSHPublicKey() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestKeyMaterial(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "key with comment",
			line: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl user@host",
			want: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl",
		},
		{
			name: "key without comment",
			line: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl",
			want: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl",
		},
		{
			name: "same key different comments equal",
			line: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl other@host",
			want: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := keyMaterial(tt.line)
			if got != tt.want {
				t.Errorf("keyMaterial() = %q, want %q", got, tt.want)
			}
		})
	}

	// Verify that same key with different comments produces same material
	mat1 := keyMaterial("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl user1@host1")
	mat2 := keyMaterial("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl user2@host2")
	if mat1 != mat2 {
		t.Error("same key with different comments should produce equal material")
	}
}

func TestDeploySSHKeys(t *testing.T) {
	// Override HOME so we write to a temp directory
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	key1 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test1@example.com"
	key2 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPwFRoSvVFDe4FjpGgUHjBfcS20B3pjxFDfKFYB4z3KN test2@example.com"

	// First deploy: should add both keys
	result, err := deploySSHKeys([]string{key1, key2})
	if err != nil {
		t.Fatalf("deploySSHKeys() error = %v", err)
	}
	if result.Added != 2 {
		t.Errorf("added = %d, want 2", result.Added)
	}
	if result.Skipped != 0 {
		t.Errorf("skipped = %d, want 0", result.Skipped)
	}
	if result.Total != 2 {
		t.Errorf("total = %d, want 2", result.Total)
	}

	// Verify file was created with correct permissions
	sshDir := filepath.Join(tmpHome, ".ssh")
	authKeysPath := filepath.Join(sshDir, "authorized_keys")

	info, err := os.Stat(sshDir)
	if err != nil {
		t.Fatalf("stat .ssh: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0700 {
		t.Errorf(".ssh permissions = %o, want 0700", perm)
	}

	info, err = os.Stat(authKeysPath)
	if err != nil {
		t.Fatalf("stat authorized_keys: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("authorized_keys permissions = %o, want 0600", perm)
	}

	// Read file and verify contents
	data, _ := os.ReadFile(authKeysPath)
	content := string(data)
	if !strings.Contains(content, key1) {
		t.Error("authorized_keys should contain key1")
	}
	if !strings.Contains(content, key2) {
		t.Error("authorized_keys should contain key2")
	}

	// Second deploy with same keys: should skip both
	result, err = deploySSHKeys([]string{key1, key2})
	if err != nil {
		t.Fatalf("deploySSHKeys() error = %v", err)
	}
	if result.Added != 0 {
		t.Errorf("added = %d, want 0", result.Added)
	}
	if result.Skipped != 2 {
		t.Errorf("skipped = %d, want 2", result.Skipped)
	}
	if result.Total != 2 {
		t.Errorf("total = %d, want 2", result.Total)
	}

	// Third deploy with one new key and one existing: should add 1, skip 1
	key3 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDr8A4FN8FJ9Z6L5N7fWd5V4F7YHj3LKR6VL2bJB2sQ8 test3@example.com"
	result, err = deploySSHKeys([]string{key1, key3})
	if err != nil {
		t.Fatalf("deploySSHKeys() error = %v", err)
	}
	if result.Added != 1 {
		t.Errorf("added = %d, want 1", result.Added)
	}
	if result.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", result.Skipped)
	}
	if result.Total != 3 {
		t.Errorf("total = %d, want 3", result.Total)
	}
}

func TestDeploySSHKeysDeduplicatesByMaterial(t *testing.T) {
	// Same key with different comments should be treated as duplicate
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl user1@host1"
	sameKeyDiffComment := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl user2@host2"

	result, err := deploySSHKeys([]string{key})
	if err != nil {
		t.Fatalf("first deploy error = %v", err)
	}
	if result.Added != 1 {
		t.Errorf("first deploy added = %d, want 1", result.Added)
	}

	// Deploy same key with different comment — should be skipped
	result, err = deploySSHKeys([]string{sameKeyDiffComment})
	if err != nil {
		t.Fatalf("second deploy error = %v", err)
	}
	if result.Added != 0 {
		t.Errorf("second deploy added = %d, want 0 (dedup by material)", result.Added)
	}
	if result.Skipped != 1 {
		t.Errorf("second deploy skipped = %d, want 1", result.Skipped)
	}
}

func TestDeploySSHKeysPreservesExisting(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// Pre-populate authorized_keys with an existing key
	sshDir := filepath.Join(tmpHome, ".ssh")
	os.MkdirAll(sshDir, 0700)
	existingKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl existing@host"
	os.WriteFile(filepath.Join(sshDir, "authorized_keys"), []byte(existingKey+"\n"), 0600)

	// Deploy a new key
	newKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPwFRoSvVFDe4FjpGgUHjBfcS20B3pjxFDfKFYB4z3KN new@host"
	result, err := deploySSHKeys([]string{newKey})
	if err != nil {
		t.Fatalf("deploySSHKeys() error = %v", err)
	}
	if result.Added != 1 {
		t.Errorf("added = %d, want 1", result.Added)
	}
	if result.Total != 2 {
		t.Errorf("total = %d, want 2", result.Total)
	}

	// Verify existing key is preserved
	data, _ := os.ReadFile(filepath.Join(sshDir, "authorized_keys"))
	content := string(data)
	if !strings.Contains(content, existingKey) {
		t.Error("existing key should be preserved")
	}
	if !strings.Contains(content, newKey) {
		t.Error("new key should be added")
	}
}

func TestIsVPNOrigin(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		want       bool
	}{
		{"VPN address", "100.64.0.5:12345", true},
		{"VPN address upper range", "100.127.255.255:8080", true},
		{"Non-VPN private", "192.168.1.1:8080", false},
		{"Non-VPN loopback", "127.0.0.1:8080", false},
		{"Non-VPN public", "8.8.8.8:8080", false},
		{"Empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if got := isVPNOrigin(r); got != tt.want {
				t.Errorf("isVPNOrigin(%q) = %v, want %v", tt.remoteAddr, got, tt.want)
			}
		})
	}
}

func TestSSHPlaintextPathFailsClosed(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	authKeysPath := filepath.Join(tmpHome, ".ssh", "authorized_keys")

	collector := NewCollector(CollectorConfig{NodeName: "test-node"})
	srv := NewServer(ServerConfig{
		TokenValidator: &mockTokenValidator{validToken: "valid-token"},
		OrgID:          "test-org",
	}, collector)
	mux := srv.buildMux()

	key1 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test@example.com"

	// A VPN-origin POST (the exact attack the flat mesh allowed) must be refused
	// and write nothing. SSH-key injection now lives only on the mTLS control
	// listener (see TestSSHInjectionOverMTLS).
	t.Run("VPN origin no longer deploys keys", func(t *testing.T) {
		body, _ := json.Marshal(sshAuthorizedKeysRequest{Keys: []string{key1}})
		req := httptest.NewRequest(http.MethodPost, "/ssh/authorized-keys", bytes.NewReader(body))
		req.RemoteAddr = "100.64.0.5:12345"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
		if _, err := os.Stat(authKeysPath); !os.IsNotExist(err) {
			t.Error("authorized_keys must not be created via the plaintext path")
		}
	})

	// Even a valid org token on the plaintext path must not deploy keys now.
	t.Run("token on plaintext path no longer deploys keys", func(t *testing.T) {
		body, _ := json.Marshal(sshAuthorizedKeysRequest{Keys: []string{key1}})
		req := httptest.NewRequest(http.MethodPost, "/ssh/authorized-keys", bytes.NewReader(body))
		req.RemoteAddr = "192.168.1.1:12345"
		req.Header.Set("Authorization", "Bearer valid-token")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
		if _, err := os.Stat(authKeysPath); !os.IsNotExist(err) {
			t.Error("authorized_keys must not be created via the plaintext path")
		}
	})
}

// TestHandleSSHAuthorizedKeysValidation covers request validation directly;
// auth/identity gating is covered by the mTLS tests (TestSSHInjectionOverMTLS).
func TestHandleSSHAuthorizedKeysValidation(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	collector := NewCollector(CollectorConfig{NodeName: "test-node"})
	srv := NewServer(ServerConfig{}, collector)

	t.Run("invalid key format returns 400", func(t *testing.T) {
		body, _ := json.Marshal(sshAuthorizedKeysRequest{Keys: []string{"not-a-valid-key"}})
		req := httptest.NewRequest(http.MethodPost, "/ssh/authorized-keys", bytes.NewReader(body))
		w := httptest.NewRecorder()
		srv.handleSSHAuthorizedKeys(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("empty keys list returns 400", func(t *testing.T) {
		body, _ := json.Marshal(sshAuthorizedKeysRequest{Keys: []string{}})
		req := httptest.NewRequest(http.MethodPost, "/ssh/authorized-keys", bytes.NewReader(body))
		w := httptest.NewRecorder()
		srv.handleSSHAuthorizedKeys(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("method not allowed for GET", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/ssh/authorized-keys", nil)
		w := httptest.NewRecorder()
		srv.handleSSHAuthorizedKeys(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", w.Code)
		}
	})
}

func TestRequireVPNOrAuthNoValidator(t *testing.T) {
	// Server without a token validator — only VPN origin should work
	collector := NewCollector(CollectorConfig{NodeName: "test-node"})
	srv := NewServer(ServerConfig{}, collector)

	handler := srv.requireVPNOrAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	t.Run("VPN origin accepted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "100.64.0.5:12345"
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("non-VPN rejected without validator", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.168.1.1:12345"
		req.Header.Set("Authorization", "Bearer some-token")
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
	})
}
