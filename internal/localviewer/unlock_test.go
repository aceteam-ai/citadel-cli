package localviewer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse/fakebrowser"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

const viewerTestPassphrase = "lantern orbit pebble saffron"

type stubProfile struct{}

func (stubProfile) Materialize(string) error { return nil }
func (stubProfile) Persist(string) error     { return nil }
func (stubProfile) Close() error             { return nil }

// stubStore keeps a passphrase in memory and records nothing else.
type stubStore struct {
	mu   sync.Mutex
	pass string
}

func (s *stubStore) ValidateName(string) error { return nil }
func (s *stubStore) State() (localcobrowse.ProfileVaultState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return localcobrowse.ProfileVaultState{Configured: s.pass != "", Strong: s.pass != ""}, nil
}
func (s *stubStore) SetPassphrase(p string, ack bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ack || len(p) < 16 {
		return errors.New("refused")
	}
	s.pass = p
	return nil
}
func (s *stubStore) Open(_, p string) (platform.SessionProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pass == "" || p != s.pass {
		return nil, errors.New("that passphrase is not right")
	}
	return stubProfile{}, nil
}

func newProfileHarness(t *testing.T, store *stubStore) *harness {
	t.Helper()
	h := &harness{fake: fakebrowser.New(), dir: t.TempDir()}
	h.svc = localcobrowse.New(localcobrowse.Config{
		Browser:   h.fake,
		URLPolicy: func(context.Context, string) error { return nil },
		Profiles:  store,
	})
	h.srv = New(Config{Service: h.svc, OpenURL: func(string) error { return nil }, TokenDir: h.dir})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.srv.Serve(ln); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.srv.Close)
	h.base = "http://127.0.0.1:" + strconv.Itoa(h.srv.Port())
	return h
}

func (h *harness) postJSON(t *testing.T, path string, body any, csrf bool) (int, string) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", h.base+path, strings.NewReader(string(data)))
	req.Header.Set("Origin", h.origin())
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set(csrfHeader, h.srv.bearer)
	}
	req.AddCookie(&http.Cookie{Name: h.srv.cookieName(), Value: h.srv.cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// TestUnlockFromViewer: the passphrase reaches the store only through the
// guarded viewer route, a wrong one is refused without being echoed, and the
// right one starts the browser under the agent's session id.
func TestUnlockFromViewer(t *testing.T) {
	h := newProfileHarness(t, &stubStore{pass: viewerTestPassphrase})
	st, err := h.svc.Start(context.Background(), "", "g", "default")
	if err != nil || st.State != localcobrowse.StateLocked {
		t.Fatalf("start = %+v %v", st, err)
	}
	unlock := "/api/sessions/" + st.SessionID + "/unlock"

	if code, _ := h.postJSON(t, unlock, map[string]string{"passphrase": viewerTestPassphrase}, false); code != http.StatusForbidden {
		t.Fatalf("unlock without the CSRF header = %d, want 403", code)
	}
	const wrong = "lantern orbit pebble saffrom"
	code, body := h.postJSON(t, unlock, map[string]string{"passphrase": wrong}, true)
	if code != http.StatusConflict || strings.Contains(body, wrong) {
		t.Fatalf("wrong passphrase = %d %q", code, body)
	}
	if code, body := h.postJSON(t, unlock, map[string]string{"passphrase": viewerTestPassphrase}, true); code != http.StatusOK {
		t.Fatalf("unlock = %d %s", code, body)
	}
	found := false
	for _, c := range h.fake.Calls {
		if c == "start-profile "+st.SessionID {
			found = true
		}
	}
	if !found {
		t.Fatalf("browser not started with the profile: %v", h.fake.Calls)
	}
}

func TestSetupPassphraseFromViewer(t *testing.T) {
	store := &stubStore{}
	h := newProfileHarness(t, store)
	st, _ := h.svc.Start(context.Background(), "", "g", "default")
	path := "/api/sessions/" + st.SessionID + "/setup-passphrase"
	if code, _ := h.postJSON(t, path, map[string]any{"passphrase": viewerTestPassphrase, "confirm": "different words here now", "ack": true}, true); code != http.StatusBadRequest {
		t.Fatalf("mismatched confirmation = %d, want 400", code)
	}
	if store.pass != "" {
		t.Fatal("a mismatched confirmation set the passphrase")
	}
	code, body := h.postJSON(t, path, map[string]any{"passphrase": viewerTestPassphrase, "confirm": viewerTestPassphrase, "ack": true}, true)
	if code != http.StatusOK || strings.Contains(body, viewerTestPassphrase) {
		t.Fatalf("setup = %d %q", code, body)
	}
	if store.pass != viewerTestPassphrase {
		t.Fatal("setup did not set the passphrase")
	}
}

func TestStateReportsProfileVault(t *testing.T) {
	h := newProfileHarness(t, &stubStore{})
	resp := h.do(t, "GET", "/api/state", map[string]string{csrfHeader: h.srv.bearer}, true)
	var st State
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.ProfileVault == nil || st.ProfileVault.Configured {
		t.Fatalf("profile_vault = %+v", st.ProfileVault)
	}
}
