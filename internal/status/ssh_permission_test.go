package status

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// The control listener's /ssh/authorized-keys runs the operator's SSH
// permission after the coordinator check: off or nil refuses with a fixed 403
// and writes nothing, on deploys, and a non-coordinator never reaches it.
func TestControlSSHDeployHonorsPermission(t *testing.T) {
	ca := newTestCA(t, "fabric-ca")
	v, err := NewFabricCAVerifier(writeBundle(t, ca.certPEM), []string{coordinatorSAN})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	serverCert := ca.issueLeaf(t, "node-server")
	coord := ca.issueLeaf(t, "coordinator", coordinatorSAN)
	peer := ca.issueLeaf(t, "peer-node", "aceteam:node:abc", "aceteam:org:other")
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl coordinator@aceteam"

	post := func(t *testing.T, allowed func() bool, leaf *x509.Certificate) (int, string, bool) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		// deploySSHKeys resolves home through platform.UserHomeDir; the fixture
		// pins it ahead of HOME, sudo and account lookup, so no case can reach
		// a real authorized_keys.
		restore, err := platform.OverrideHomeDirectoryForTesting(home)
		if err != nil {
			t.Fatalf("home fixture: %v", err)
		}
		defer func() {
			if err := restore(); err != nil {
				t.Errorf("restore home fixture: %v", err)
			}
		}()
		srv := NewServer(ServerConfig{CAVerifier: v, ControlServerCert: &serverCert, SSHDeployAllowed: allowed}, NewCollector(CollectorConfig{NodeName: "test-node"}))
		body, _ := json.Marshal(sshAuthorizedKeysRequest{Keys: []string{key}})
		req := httptest.NewRequest(http.MethodPost, "/ssh/authorized-keys", bytes.NewReader(body))
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
		w := httptest.NewRecorder()
		srv.buildControlMux().ServeHTTP(w, req)
		data, _ := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
		return w.Code, w.Body.String(), strings.Contains(string(data), key)
	}

	t.Run("permission off refuses", func(t *testing.T) {
		code, body, wrote := post(t, func() bool { return false }, coord.Leaf)
		if code != http.StatusForbidden || wrote {
			t.Fatalf("code=%d wrote=%v, want 403 and no write", code, wrote)
		}
		if !strings.Contains(body, "disabled on this node by its permission toggle") {
			t.Fatalf("unexpected refusal body: %s", body)
		}
	})
	t.Run("nil callback refuses", func(t *testing.T) {
		if code, _, wrote := post(t, nil, coord.Leaf); code != http.StatusForbidden || wrote {
			t.Fatalf("code=%d wrote=%v, want 403 and no write", code, wrote)
		}
	})
	t.Run("permission on deploys", func(t *testing.T) {
		if code, _, wrote := post(t, func() bool { return true }, coord.Leaf); code != http.StatusOK || !wrote {
			t.Fatalf("code=%d wrote=%v, want 200 and the key written", code, wrote)
		}
	})
	t.Run("non-coordinator never consults the permission", func(t *testing.T) {
		calls := 0
		code, _, wrote := post(t, func() bool { calls++; return true }, peer.Leaf)
		if code != http.StatusForbidden || wrote || calls != 0 {
			t.Fatalf("code=%d wrote=%v calls=%d, want 403, no write, no permission read", code, wrote, calls)
		}
	})
}
