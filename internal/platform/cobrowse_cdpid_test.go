<<<<<<< HEAD
package platform

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeVersionServer answers /json/version like a browser with the given id.
func fakeVersionServer(t *testing.T, id string) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/browser/%s"}`, id)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

func TestAdoptCDPCheckFixedPortNoFile(t *testing.T) {
	port := fakeVersionServer(t, "guid-1")
	dir := t.TempDir() // Chrome writes no DevToolsActivePort with a fixed port
	if err := defaultAdoptCDPCheck(port, dir, "guid-1"); err != nil {
		t.Fatalf("matching id must be adopted: %v", err)
	}
	if err := defaultAdoptCDPCheck(port, dir, "guid-2"); err == nil {
		t.Fatal("id mismatch must be refused")
	}
	if err := defaultAdoptCDPCheck(port, dir, ""); err == nil {
		t.Fatal("missing stored id must be refused")
	}
}

func TestAdoptCDPCheckPortZeroFile(t *testing.T) {
	port := fakeVersionServer(t, "guid-1")
	dir := t.TempDir()
	write := func(id string) {
		data := fmt.Sprintf("%d\n/devtools/browser/%s\n", port, id)
		if err := os.WriteFile(filepath.Join(dir, devToolsActivePortFile), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("guid-1")
	if err := defaultAdoptCDPCheck(port, dir, "guid-1"); err != nil {
		t.Fatalf("file path: %v", err)
	}
	write("other")
	if err := defaultAdoptCDPCheck(port, dir, "guid-1"); err == nil {
		t.Fatal("file disagreeing with the record must be refused")
	}
}

func TestResolveBrowserIDFixedPortAndFile(t *testing.T) {
	port := fakeVersionServer(t, "guid-9")
	dir := t.TempDir()
	if id, err := resolveBrowserID(port, dir); err != nil || id != "guid-9" {
		t.Fatalf("got %q %v", id, err)
	}
	data := fmt.Sprintf("%d\n/devtools/browser/from-file\n", port)
	if err := os.WriteFile(filepath.Join(dir, devToolsActivePortFile), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if id, err := resolveBrowserID(port, dir); err != nil || id != "from-file" {
		t.Fatalf("got %q %v", id, err)
	}
}

func TestOldRecordVersionRefused(t *testing.T) {
	rec := sessionRecord{Version: 1, ID: "cb-1"}
	if err := verifyAdoptable("/x", "/x/cb-1", rec); err == nil {
		t.Fatal("version 1 record must fail closed")
	}
}
||||||| 5c9afea7
=======
package platform

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeVersionServer answers /json/version like a browser with the given id.
func fakeVersionServer(t *testing.T, id string) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/browser/%s"}`, id)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

func TestAdoptCDPCheckFixedPortNoFile(t *testing.T) {
	port := fakeVersionServer(t, "guid-1")
	dir := t.TempDir() // Chrome writes no DevToolsActivePort with a fixed port
	if err := defaultAdoptCDPCheck(port, dir, "guid-1"); err != nil {
		t.Fatalf("matching id must be adopted: %v", err)
	}
	if err := defaultAdoptCDPCheck(port, dir, "guid-2"); err == nil {
		t.Fatal("id mismatch must be refused")
	}
	if err := defaultAdoptCDPCheck(port, dir, ""); err == nil {
		t.Fatal("missing stored id must be refused")
	}
}

func TestAdoptCDPCheckPortZeroFile(t *testing.T) {
	port := fakeVersionServer(t, "guid-1")
	dir := t.TempDir()
	write := func(id string) {
		data := fmt.Sprintf("%d\n/devtools/browser/%s\n", port, id)
		if err := os.WriteFile(filepath.Join(dir, devToolsActivePortFile), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("guid-1")
	if err := defaultAdoptCDPCheck(port, dir, "guid-1"); err != nil {
		t.Fatalf("file path: %v", err)
	}
	write("other")
	if err := defaultAdoptCDPCheck(port, dir, "guid-1"); err == nil {
		t.Fatal("file disagreeing with the record must be refused")
	}
}

func TestResolveBrowserIDFixedPortAndFile(t *testing.T) {
	port := fakeVersionServer(t, "guid-9")
	dir := t.TempDir()
	if id, err := resolveBrowserID(port, dir); err != nil || id != "guid-9" {
		t.Fatalf("got %q %v", id, err)
	}
	data := fmt.Sprintf("%d\n/devtools/browser/from-file\n", port)
	if err := os.WriteFile(filepath.Join(dir, devToolsActivePortFile), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if id, err := resolveBrowserID(port, dir); err != nil || id != "from-file" {
		t.Fatalf("got %q %v", id, err)
	}
}

func TestOldRecordVersionRefused(t *testing.T) {
	rec := sessionRecord{Version: 1, ID: "cb-1"}
	if err := verifyAdoptable("/x", "/x/cb-1", rec); err == nil {
		t.Fatal("version 1 record must fail closed")
	}
}

func TestFetchCDPBrowserIDRefusesRedirect(t *testing.T) {
	target := fakeVersionServer(t, "guid-r")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("http://127.0.0.1:%d/json/version", target), http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	if id, err := fetchCDPBrowserID(port); err == nil {
		t.Fatalf("redirect must not yield an id, got %q", id)
	}
	if err := defaultAdoptCDPCheck(port, t.TempDir(), "guid-r"); err == nil {
		t.Fatal("redirecting endpoint must be refused")
	}
}

func TestPersistRecordWritesBrowserID(t *testing.T) {
	newAdoptEnv(t)
	port := fakeVersionServer(t, "guid-p")
	prev := launchCobrowseProc
	launchCobrowseProc = func(profileDir, startURL string, _ LaunchOptions) (*cobrowseProc, error) {
		return &cobrowseProc{
			debugPort: port, display: ":51", browserPID: 100001, xvfbPID: 200001,
			exited: make(chan struct{}), stop: func() error { return nil },
		}, nil
	}
	t.Cleanup(func() { launchCobrowseProc = prev })
	m := adoptMgr(trustedBaseDir(t))
	st, err := m.StartSession("https://a.example")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := loadRecord(m.registryDir(), st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.BrowserID != "guid-p" || rec.Version != sessionRecordVersion {
		t.Fatalf("record = %+v", rec)
	}
}
>>>>>>> origin/main
