package platform

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// runFileUploadSpoofRealBrowser is the negative control for the page fence
// (citadel-cli#1721). The page at /spoof?real=1 replaces the in-page getters a
// naive fence would read (Document.prototype.URL, Node.prototype.ownerDocument,
// location where the browser allows it) so they all claim the URL the caller
// expects, while the page really is at a different URL. The fence must go by
// the browser's own frame URL, so the upload is refused and the server
// receives nothing. The benign upload test is the positive control.
func runFileUploadSpoofRealBrowser(t *testing.T, m *CobrowseSessionManager) {
	t.Helper()
	var mu sync.Mutex
	received := 0
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.Method == http.MethodPost && r.URL.Path == "/upload" {
			if err := r.ParseMultipartForm(8 << 20); err == nil {
				if f, h, err := r.FormFile("doc"); err == nil {
					b, _ := io.ReadAll(f)
					f.Close()
					if h.Filename != "" && len(b) > 0 {
						mu.Lock()
						received++
						mu.Unlock()
					}
				}
			}
			fmt.Fprint(w, `<!doctype html><title>done</title>`)
			return
		}
		// The page claims to be at /spoof (the URL the caller expects) while
		// it is really at /spoof?real=1.
		fmt.Fprintf(w, `<!doctype html><title>spoof</title>
<script>
var CLAIMED = %q;
function lie(proto, name) { try { Object.defineProperty(proto, name, {configurable: true, get: function() { return CLAIMED; }}); } catch (e) {} }
lie(Document.prototype, "URL");
lie(Document.prototype, "documentURI");
try { Object.defineProperty(Node.prototype, "ownerDocument", {configurable: true, get: function() { return {URL: CLAIMED}; }}); } catch (e) {}
try { Object.defineProperty(window, "location", {configurable: true, get: function() { return {href: CLAIMED}; }}); } catch (e) {}
try { Object.defineProperty(Location.prototype, "href", {configurable: true, get: function() { return CLAIMED; }}); } catch (e) {}
</script>
<form method="post" action="/upload" enctype="multipart/form-data">
<input id="f" name="doc" type="file"><button id="go" type="submit">Send</button></form>`, srvURL+"/spoof")
	}))
	t.Cleanup(srv.Close)
	srvURL = srv.URL
	claimed := srv.URL + "/spoof"

	st, err := m.StartSession(srv.URL + "/spoof?real=1")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = m.StopAll() })
	waitCond(t, 30*time.Second, "the spoof page", func() bool {
		_, err := m.Extract(st.ID, "#f", nil)
		return err == nil
	})
	if _, err := m.StageFile(st.ID, "doc.bin", []byte("secret-contents")); err != nil {
		t.Fatalf("StageFile: %v", err)
	}
	// The page script really did replace the getters (so the test would catch
	// a fence that trusted them).
	if _, err := m.SetFileInput(st.ID, "#f", 0, "doc.bin", claimed); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("a spoofing page must be refused with ErrTargetChanged, got %v", err)
	}
	if _, err := m.Click(st.ID, "#go", nil, nil); err != nil {
		t.Fatalf("click submit: %v", err)
	}
	time.Sleep(2 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	if received != 0 {
		t.Fatalf("the server received %d uploads from a refused set_file_input", received)
	}
}
