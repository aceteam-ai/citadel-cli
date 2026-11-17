package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// uploadPage is a synthetic loopback page: a file input, a text input and a
// form that posts to /upload.
const uploadPage = `<!doctype html><title>upload</title>
<form method="post" action="/upload" enctype="multipart/form-data">
<input id="f" name="doc" type="file">
<input id="txt" name="note" type="text" value="n">
<button id="go" type="submit">Send</button>
</form>`

type receivedUpload struct {
	filename string
	body     []byte
}

// runFileUploadRealBrowser is the shared real-browser scenario (citadel-cli#1721):
// stage a file, fill the input with set_file_input, submit, and the server
// receives the exact bytes; then a page-opened file chooser is intercepted,
// reported pending, and answered through its backend node id. The OS-specific
// tests supply the manager.
func runFileUploadRealBrowser(t *testing.T, m *CobrowseSessionManager) {
	t.Helper()
	var mu sync.Mutex
	var got []receivedUpload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.Method == http.MethodPost && r.URL.Path == "/upload" {
			if err := r.ParseMultipartForm(8 << 20); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			f, h, err := r.FormFile("doc")
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			defer f.Close()
			b, _ := io.ReadAll(f)
			mu.Lock()
			got = append(got, receivedUpload{filename: h.Filename, body: b})
			mu.Unlock()
			fmt.Fprint(w, `<!doctype html><title>done</title><p id="ok">received</p>`)
			return
		}
		fmt.Fprint(w, uploadPage)
	}))
	t.Cleanup(srv.Close)
	pageURL := srv.URL + "/"

	// Binary content that is not valid UTF-8, so a text round trip would show.
	data := make([]byte, 64*1024)
	for i := range data {
		data[i] = byte(i * 7)
	}

	st, err := m.StartSession(srv.URL + "/")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = m.StopAll() })
	waitCond(t, 30*time.Second, "page with the file input", func() bool {
		_, err := m.Extract(st.ID, "#f", nil)
		return err == nil
	})
	stage := filepath.Join(st.Profile, cobrowseStagingDirName)
	if fi, err := os.Stat(stage); err != nil || !fi.IsDir() {
		t.Fatalf("staging dir missing after start: %v", err)
	}

	// 1. Stage, set by selector, submit.
	if _, err := m.StageFile(st.ID, "doc.bin", data); err != nil {
		t.Fatalf("StageFile: %v", err)
	}
	// Refusals reach no browser state.
	if _, err := m.SetFileInput(st.ID, "#f", 0, "../doc.bin", pageURL); err == nil {
		t.Fatal("a traversal name must be refused")
	}
	if _, err := m.SetFileInput(st.ID, "#txt", 0, "doc.bin", pageURL); err == nil {
		t.Fatal("a non-file input must be refused")
	}
	if _, err := m.SetFileInput(st.ID, "#nothing", 0, "doc.bin", pageURL); err == nil {
		t.Fatal("a selector matching nothing must be refused")
	}
	out, err := m.SetFileInput(st.ID, "#f", 0, "doc.bin", pageURL)
	if err != nil {
		t.Fatalf("SetFileInput: %v", err)
	}
	if out.Effects == nil || out.Effects.FilesSet != 1 || !out.Effects.Dispatched {
		t.Fatalf("effects = %+v", out.Effects)
	}
	if _, err := m.Click(st.ID, "#go", nil, nil); err != nil {
		t.Fatalf("click submit: %v", err)
	}
	waitCond(t, 20*time.Second, "the server to receive the upload", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})
	mu.Lock()
	if got[0].filename != "doc.bin" || !bytes.Equal(got[0].body, data) {
		t.Fatalf("server received %q (%d bytes), want doc.bin (%d bytes) with identical content", got[0].filename, len(got[0].body), len(data))
	}
	mu.Unlock()

	// 2. A chooser the page opens is intercepted, reported, and answered.
	if _, err := m.Navigate(st.ID, srv.URL+"/"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	waitCond(t, 30*time.Second, "the file input again", func() bool {
		_, err := m.Extract(st.ID, "#f", nil)
		return err == nil
	})
	if _, err := m.Click(st.ID, "#f", nil, nil); err != nil {
		t.Fatalf("click file input: %v", err)
	}
	var pending *FileChooserPending
	waitCond(t, 20*time.Second, "the chooser to be reported pending", func() bool {
		s, ok := m.SessionStatus(st.ID)
		if ok && s.FileChooser != nil {
			pending = s.FileChooser
			return true
		}
		return false
	})
	if pending.BackendNodeID <= 0 || pending.Multiple {
		t.Fatalf("pending chooser = %+v", pending)
	}
	if _, err := m.StageFile(st.ID, "doc2.bin", data); err != nil {
		t.Fatalf("StageFile 2: %v", err)
	}
	if _, err := m.SetFileInput(st.ID, "", pending.BackendNodeID, "doc2.bin", pageURL); err != nil {
		t.Fatalf("SetFileInput by backend node: %v", err)
	}
	if s, _ := m.SessionStatus(st.ID); s.FileChooser != nil {
		t.Fatalf("the chooser should clear once its input is filled: %+v", s.FileChooser)
	}
	if _, err := m.Click(st.ID, "#go", nil, nil); err != nil {
		t.Fatalf("click submit 2: %v", err)
	}
	waitCond(t, 20*time.Second, "the second upload", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 2
	})
	mu.Lock()
	if got[1].filename != "doc2.bin" || !bytes.Equal(got[1].body, data) {
		t.Fatalf("second upload: %q, %d bytes", got[1].filename, len(got[1].body))
	}
	mu.Unlock()

	// 2b. The human path (citadel-cli#1723): a page-opened chooser is answered
	// by the viewer's attach with a staged file, while the agent is refused.
	m.SetFilesAllowed(func() bool { return true })
	if _, err := m.Navigate(st.ID, srv.URL+"/"); err != nil {
		t.Fatalf("navigate 3: %v", err)
	}
	waitCond(t, 30*time.Second, "the file input a third time", func() bool {
		_, err := m.Extract(st.ID, "#f", nil)
		return err == nil
	})
	if _, err := m.Click(st.ID, "#f", nil, nil); err != nil {
		t.Fatalf("click file input 3: %v", err)
	}
	var pending3 *FileChooserPending
	waitCond(t, 20*time.Second, "the third chooser to be reported pending", func() bool {
		s, ok := m.SessionStatus(st.ID)
		if ok && s.FileChooser != nil {
			pending3 = s.FileChooser
			return true
		}
		return false
	})
	if _, err := m.StageFile(st.ID, "doc3.bin", data); err != nil {
		t.Fatalf("StageFile 3: %v", err)
	}
	// Without control the human path refuses and the browser is untouched.
	if _, err := m.HumanAttachStagedFile(context.Background(), st.ID, pending3.BackendNodeID, "doc3.bin"); !errors.Is(err, ErrViewerNoControl) {
		t.Fatalf("attach without control: %v", err)
	}
	if _, err := m.Handoff(st.ID); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if !m.MarkAttached(st.ID) {
		t.Fatal("MarkAttached")
	}
	// The agent's set_file_input is refused while the human drives.
	if _, err := m.SetFileInput(st.ID, "", pending3.BackendNodeID, "doc3.bin", pageURL); !errors.Is(err, ErrHandedOff) {
		t.Fatalf("agent set_file_input during handoff: %v", err)
	}
	// The browser-reported frame URL arrives shortly after the chooser opens.
	waitCond(t, 20*time.Second, "the chooser to be offered to the viewer", func() bool {
		_, ok := m.HumanFileAttachOffer(st.ID)
		return ok
	})
	if _, err := m.HumanAttachStagedFile(context.Background(), st.ID, pending3.BackendNodeID+1, "doc3.bin"); !errors.Is(err, ErrChooserMismatch) {
		t.Fatalf("attach to another node: %v", err)
	}
	if _, err := m.HumanAttachStagedFile(context.Background(), st.ID, pending3.BackendNodeID, "doc3.bin"); err != nil {
		t.Fatalf("human attach: %v", err)
	}
	if s, _ := m.SessionStatus(st.ID); s.FileChooser != nil {
		t.Fatalf("the chooser should clear after the human attach: %+v", s.FileChooser)
	}
	if _, err := m.Resume(st.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := m.Click(st.ID, "#go", nil, nil); err != nil {
		t.Fatalf("click submit 3: %v", err)
	}
	waitCond(t, 20*time.Second, "the human-attached upload", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 3
	})
	mu.Lock()
	if got[2].filename != "doc3.bin" || !bytes.Equal(got[2].body, data) {
		t.Fatalf("human upload: %q, %d bytes", got[2].filename, len(got[2].body))
	}
	mu.Unlock()

	// The viewer detaches, as a real one does before the session ends.
	m.MarkDetached(st.ID)

	// 3. Stop removes the staging dir with the session.
	if err := m.Stop(st.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("staging dir survived stop: %v", err)
	}
}

func waitCond(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("timed out waiting for %s", what)
	}
}
