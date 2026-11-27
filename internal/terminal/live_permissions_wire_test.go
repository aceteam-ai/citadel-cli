//go:build !windows

package terminal

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/gorilla/websocket"
)

func TestLivePolicyTerminalAttachedSession(t *testing.T) {
	for _, change := range []string{"disable", "rotate", "malformed"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			p := &config.Permissions{Console: true}
			if err := p.SetPasscode("fixture-pin"); err != nil {
				t.Fatal(err)
			}
			if err := config.SavePermissions(dir, p); err != nil {
				t.Fatal(err)
			}
			cfg := DefaultConfig()
			cfg.SessionName = ""
			cfg.Shell = "/bin/cat"
			cfg.OrgID = "fixture-org"
			cfg.PermissionSource = func() (*config.Permissions, error) { return config.LoadPermissionsStrict(dir) }
			auth := NewMockTokenValidator()
			auth.AddValidToken("fixture-token", &TokenInfo{UserID: "fixture-user", OrgID: cfg.OrgID})
			s := NewServer(cfg, auth)
			s.SetSilent()
			h := httptest.NewServer(http.HandlerFunc(s.handleWebSocket))
			defer h.Close()
			c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http")+"/terminal?token=fixture-token", http.Header{"X-Citadel-Passcode": []string{"fixture-pin"}})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			deadline := time.Now().Add(2 * time.Second)
			for s.SessionCount() == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if s.SessionCount() != 1 {
				t.Fatal("permitted session did not attach")
			}
			switch change {
			case "disable":
				p.Console = false
			case "rotate":
				if err := p.SetPasscode("next-fixture-pin"); err != nil {
					t.Fatal(err)
				}
			}
			if change == "malformed" {
				if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), []byte("console: ["), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := config.SavePermissions(dir, p); err != nil {
				t.Fatal(err)
			}
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					break
				}
			}
			deadline = time.Now().Add(2 * time.Second)
			for s.SessionCount() != 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if s.SessionCount() != 0 {
				t.Fatal("attached session remained active")
			}
		})
	}
}
