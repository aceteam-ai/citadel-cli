package cobrowsestream

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/gorilla/websocket"
)

// fakeAttachSession is a FileAttachSession over fakeSession.
type fakeAttachSession struct {
	*fakeSession
	amu      sync.Mutex
	offer    FileChooserMessage
	offering bool
	reason   string
	attached []string
	block    chan struct{}
}

func (s *fakeAttachSession) FileChooser(id string) (FileChooserMessage, bool) {
	s.amu.Lock()
	defer s.amu.Unlock()
	return s.offer, s.offering
}

func (s *fakeAttachSession) setOffer(m FileChooserMessage, on bool) {
	s.amu.Lock()
	s.offer, s.offering = m, on
	s.amu.Unlock()
}

func (s *fakeAttachSession) AttachStagedFile(ctx context.Context, id string, node int, name string) string {
	s.amu.Lock()
	s.attached = append(s.attached, name)
	reason, block := s.reason, s.block
	s.amu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
		}
	}
	return reason
}

// One attach at a time per viewer: a second while one is in flight is answered
// busy and never reaches the session. The attach gets the stream's context.
func TestFileAttachBusyWhileOneIsInFlight(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	sess := &fakeAttachSession{fakeSession: &fakeSession{port: 9222, attachable: true, humanControl: true}, block: make(chan struct{})}
	ts := newHarness(t, sess, f)
	conn, _, err := dialViewer(t, ts, "cb-1")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	readFileChooser(t, conn, false)
	sendAttach(t, conn, 7, "a.pdf")
	deadline := time.Now().Add(3 * time.Second)
	for len(sess.attachCalls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	sendAttach(t, conn, 7, "b.pdf")
	var res FileAttachResult
	for {
		res = FileAttachResult{}
		if json.Unmarshal(readText(t, conn), &res) == nil && res.Type == FileAttachResultType {
			break
		}
	}
	if res.OK || res.Error != "busy" {
		t.Fatalf("second attach = %+v, want busy", res)
	}
	if got := sess.attachCalls(); len(got) != 1 || got[0] != "a.pdf" {
		t.Fatalf("the busy attach reached the session: %v", got)
	}
	close(sess.block)
}

func (s *fakeAttachSession) attachCalls() []string {
	s.amu.Lock()
	defer s.amu.Unlock()
	return append([]string(nil), s.attached...)
}

// readText returns the next TEXT frame, skipping binary frames.
func readText(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if mt == websocket.TextMessage {
			return data
		}
	}
}

func readFileChooser(t *testing.T, conn *websocket.Conn, wantPending bool) FileChooserMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var m FileChooserMessage
		if json.Unmarshal(readText(t, conn), &m) == nil && m.Type == FileChooserType && m.Pending == wantPending {
			return m
		}
	}
	t.Fatalf("no file_chooser frame with pending=%v", wantPending)
	return FileChooserMessage{}
}

func sendAttach(t *testing.T, conn *websocket.Conn, node int, name string) {
	t.Helper()
	data, _ := json.Marshal(InputMessage{Type: InputTypeFileAttach, BackendNodeID: node, StagedName: name})
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}
}

func TestFileAttachOfferedAndHandledWhenSupported(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	sess := &fakeAttachSession{fakeSession: &fakeSession{port: 9222, attachable: true, humanControl: true}}
	ts := newHarness(t, sess, f)
	conn, _, err := dialViewer(t, ts, "cb-1")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// A supporting session says so right away: no chooser, nothing staged.
	first := readFileChooser(t, conn, false)
	if len(first.Staged) != 0 {
		t.Fatalf("idle frame = %+v", first)
	}
	// A pending chooser arrives with the staged list: names and sizes only.
	sess.setOffer(FileChooserMessage{Type: FileChooserType, Pending: true, BackendNodeID: 7, Multiple: false,
		Staged: []StagedFileInfo{{Name: "a.pdf", Size: 12}}}, true)
	m := readFileChooser(t, conn, true)
	if m.BackendNodeID != 7 || len(m.Staged) != 1 || m.Staged[0].Name != "a.pdf" || m.Staged[0].Size != 12 {
		t.Fatalf("pending frame = %+v", m)
	}
	raw, _ := json.Marshal(m)
	if strings.Contains(string(raw), "sha256") || strings.Contains(string(raw), "path") {
		t.Fatalf("frame carries more than names and sizes: %s", raw)
	}

	sendAttach(t, conn, 7, "a.pdf")
	var res FileAttachResult
	for {
		if json.Unmarshal(readText(t, conn), &res) == nil && res.Type == FileAttachResultType {
			break
		}
	}
	if !res.OK || res.StagedName != "a.pdf" || res.Error != "" {
		t.Fatalf("result = %+v", res)
	}
	if got := sess.attachCalls(); len(got) != 1 || got[0] != "a.pdf" {
		t.Fatalf("attach calls = %v", got)
	}

	// A refusal carries only the fixed reason.
	sess.amu.Lock()
	sess.reason = "page_changed"
	sess.amu.Unlock()
	sendAttach(t, conn, 7, "a.pdf")
	for {
		res = FileAttachResult{}
		if json.Unmarshal(readText(t, conn), &res) == nil && res.Type == FileAttachResultType {
			break
		}
	}
	if res.OK || res.Error != "page_changed" || res.StagedName != "" {
		t.Fatalf("refusal = %+v", res)
	}
}

func TestFileAttachIgnoredWithoutControl(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	sess := &fakeAttachSession{fakeSession: &fakeSession{port: 9222, attachable: true}}
	ts := newHarness(t, sess, f)
	conn, _, err := dialViewer(t, ts, "cb-1")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	readFileChooser(t, conn, false)
	sendAttach(t, conn, 7, "a.pdf")
	sess.waitInputChecks(t, 1)
	// A later accepted input orders the assertion after the attach was seen.
	sess.setHumanControl(true)
	data, _ := json.Marshal(InputMessage{Type: "paste", Text: "marker"})
	_ = conn.WriteMessage(websocket.TextMessage, data)
	sess.waitInputChecks(t, 2)
	if got := sess.attachCalls(); len(got) != 0 {
		t.Fatalf("an attach without control reached the session: %v", got)
	}
}

// A session that does not implement FileAttachSession sends no file_chooser
// frame and ignores file_attach, even from a viewer in control.
func TestFileAttachIgnoredWhenManagerLacksSupport(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	sess := &fakeSession{port: 9222, attachable: true, humanControl: true}
	ts := newHarness(t, sess, f)
	conn, _, err := dialViewer(t, ts, "cb-1")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sendAttach(t, conn, 7, "a.pdf")
	sess.waitInputChecks(t, 1)
	data, _ := json.Marshal(InputMessage{Type: "paste", Text: "marker"})
	_ = conn.WriteMessage(websocket.TextMessage, data)
	sess.waitInputChecks(t, 2)
	// Everything the server sent so far: init and frames, never file frames.
	_ = conn.SetReadDeadline(time.Now().Add(1200 * time.Millisecond))
	for {
		mt, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.TextMessage && (strings.Contains(string(raw), FileChooserType) || strings.Contains(string(raw), FileAttachResultType)) {
			t.Fatalf("unsupported session sent a file frame: %s", raw)
		}
	}
}

func TestParseFileAttachInput(t *testing.T) {
	ok := `{"type":"file_attach","backend_node_id":7,"staged_name":"a.pdf"}`
	if m, good := parseInputMessage([]byte(ok)); !good || m.BackendNodeID != 7 || m.StagedName != "a.pdf" {
		t.Fatalf("parse = %+v %v", m, good)
	}
	for _, bad := range []string{
		`{"type":"file_attach","staged_name":"a.pdf"}`,
		`{"type":"file_attach","backend_node_id":0,"staged_name":"a.pdf"}`,
		`{"type":"file_attach","backend_node_id":7}`,
		`{"type":"file_attach","backend_node_id":7,"staged_name":"` + strings.Repeat("a", 129) + `"}`,
	} {
		if _, good := parseInputMessage([]byte(bad)); good {
			t.Errorf("%s should be rejected", bad)
		}
	}
}

// Only the node-wide manager, wired with a Files source, is offered attach:
// never a tracking (local-mode) manager, never an unwired one.
func TestManagerSessionOffersFileAttachOnlyToWiredNodeWideManager(t *testing.T) {
	plain := platform.NewCobrowseSessionManager(t.TempDir(), 1)
	if _, ok := NewManagerSessionFrom(plain).(FileAttachSession); ok {
		t.Fatal("an unwired manager must not offer file attach")
	}
	plain.SetFilesAllowed(func() bool { return true })
	if _, ok := NewManagerSessionFrom(plain).(FileAttachSession); !ok {
		t.Fatal("a wired node-wide manager offers file attach")
	}
	local := platform.NewCobrowseSessionManager(t.TempDir(), 1)
	local.EnableTargetTracking(nil, nil)
	local.SetFilesAllowed(func() bool { return true })
	if _, ok := NewManagerSessionFrom(local).(FileAttachSession); ok {
		t.Fatal("a tracking (local mode) manager must never offer file attach")
	}
}

// The viewer stream is the only caller of the human attach: no job action, MCP
// tool or other package reaches HumanAttachStagedFile. The callers outside the
// platform package are exactly the stream adapter.
func TestOnlyTheStreamAdapterCallsHumanAttach(t *testing.T) {
	root := filepath.Join("..", "..")
	var callers, streamSide []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if n := info.Name(); n == ".git" || n == "node_modules" || n == ".claude" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		src := string(b)
		if strings.Contains(src, "HumanAttachStagedFile(") || strings.Contains(src, "HumanFileAttachOffer(") {
			callers = append(callers, filepath.ToSlash(p))
		}
		// The stream-side interface must not be reachable from a job or tool
		// either: it is named only inside this package's stream files.
		if strings.Contains(src, "AttachStagedFile(") || strings.Contains(src, "FileAttachSession") {
			streamSide = append(streamSide, filepath.ToSlash(p))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(callers)
	sort.Strings(streamSide)
	want := []string{"../../internal/cobrowsestream/adapter.go", "../../internal/platform/cobrowse_human_attach.go"}
	if strings.Join(callers, ",") != strings.Join(want, ",") {
		t.Fatalf("human attach is referenced from %v, want only %v", callers, want)
	}
	// platform defines HumanAttachStagedFile (matched above); the interface
	// lives in cobrowsestream.
	wantStream := []string{"../../internal/cobrowsestream/adapter.go", "../../internal/cobrowsestream/handler.go", "../../internal/cobrowsestream/server.go", "../../internal/cobrowsestream/wire.go"}
	var gotStream []string
	for _, p := range streamSide {
		if p != "../../internal/platform/cobrowse_human_attach.go" {
			gotStream = append(gotStream, p)
		}
	}
	if strings.Join(gotStream, ",") != strings.Join(wantStream, ",") {
		t.Fatalf("the stream attach interface is referenced from %v, want only %v", gotStream, wantStream)
	}
}
