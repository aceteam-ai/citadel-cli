package platform

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

// fenceCaller is a cdpCaller for the pure setFilesIfPageIs step.
type fenceCaller struct {
	ownerURL string
	calls    []string
	params   map[string]map[string]any
}

func (f *fenceCaller) Call(method string, params map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, method)
	if f.params == nil {
		f.params = map[string]map[string]any{}
	}
	f.params[method] = params
	if method == "Runtime.callFunctionOn" {
		return map[string]any{"result": map[string]any{"value": f.ownerURL}}, nil
	}
	return map[string]any{}, nil
}

func (f *fenceCaller) setCalls() int {
	n := 0
	for _, c := range f.calls {
		if c == "DOM.setFileInputFiles" {
			n++
		}
	}
	return n
}

func TestFileUploadPageFenceMatchAttaches(t *testing.T) {
	const u = "https://portal.example/file?x=1#step2"
	f := &fenceCaller{ownerURL: u}
	var eff ActionEffects
	if err := setFilesIfPageIs(f, "obj1", u, []string{"/p/a.pdf"}, nil, &eff); err != nil {
		t.Fatal(err)
	}
	if f.setCalls() != 1 || eff.FilesSet != 1 || !eff.Dispatched {
		t.Fatalf("calls=%v eff=%+v", f.calls, eff)
	}
	// The files go to the very object whose document was checked.
	if f.params["DOM.setFileInputFiles"]["objectId"] != "obj1" {
		t.Fatalf("dispatch params = %v", f.params["DOM.setFileInputFiles"])
	}
}

func TestFileUploadPageFenceMismatchRefusesWithNothingDispatched(t *testing.T) {
	const u = "https://portal.example/file?x=1#step2"
	for _, cur := range []string{
		"https://portal.example/file?x=1#step3", // fragment only
		"https://portal.example/file?x=1",
		"https://portal.example/file?x=2#step2",
		"https://portal.example/file?x=1#step2/",
		"https://other.example/file?x=1#step2",
		"",
	} {
		f := &fenceCaller{ownerURL: cur}
		var eff ActionEffects
		err := setFilesIfPageIs(f, "obj1", u, []string{"/p/a.pdf"}, nil, &eff)
		if !errors.Is(err, ErrTargetChanged) {
			t.Fatalf("%q: got %v, want ErrTargetChanged", cur, err)
		}
		if f.setCalls() != 0 || eff.Dispatched || eff.FilesSet != 0 || eff.anyEffect() {
			t.Fatalf("%q: something was done: calls=%v eff=%+v", cur, f.calls, eff)
		}
		if strings.Contains(err.Error(), "portal.example") || strings.Contains(err.Error(), "other.example") {
			t.Fatalf("error carries a URL: %v", err)
		}
	}
}

func TestFileUploadPageFenceDriverFenceRunsAfterTheComparison(t *testing.T) {
	const u = "https://portal.example/f"
	f := &fenceCaller{ownerURL: u}
	var eff ActionEffects
	err := setFilesIfPageIs(f, "obj1", u, nil, func() error { return ErrHandedOff }, &eff)
	if !errors.Is(err, ErrHandedOff) || f.setCalls() != 0 || eff.Dispatched {
		t.Fatalf("err=%v calls=%v eff=%+v", err, f.calls, eff)
	}
	// Called after the URL read, not before.
	order := []string{}
	f2 := &fenceCaller{ownerURL: u}
	_ = setFilesIfPageIs(f2, "obj1", u, nil, func() error { order = append(order, strings.Join(f2.calls, ",")); return nil }, &eff)
	if len(order) != 1 || order[0] != "Runtime.callFunctionOn" {
		t.Fatalf("driver fence ran at %v, want right after the URL read", order)
	}
}

func TestSetFileInputRequiresExpectedURL(t *testing.T) {
	m, st := startFakeSession(t)
	if _, err := m.StageFile(st.ID, "a.pdf", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetFileInput(st.ID, "#f", 0, "a.pdf", ""); err == nil || !strings.Contains(err.Error(), "expected_url") {
		t.Fatalf("missing expected_url: %v", err)
	}
}

func TestStatusReportsFileUploadPageFence(t *testing.T) {
	_, st := startFakeSession(t)
	if st.FileUploadPageFence != "expected_url_v1" {
		t.Fatalf("status fence = %q", st.FileUploadPageFence)
	}
	s := &cobrowseSession{id: "t", tracking: true}
	if got, _ := s.statusBase(); got.FileUploadPageFence != "" {
		t.Fatal("page-tracking sessions have no set_file_input")
	}
}

// ---- SetFileInput over a fake CDP page ----

type fileInputFake struct {
	mu       sync.Mutex
	ownerURL string
	mainURL  string
	// onOwnerURL runs when the owner document's URL is read.
	onOwnerURL func()
}

func (ff *fileInputFake) install(f *fakeCDPServer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evaluateValue = func(expr string) any { return nil }
	f.custom = func(method string, params map[string]any) (map[string]any, bool) {
		ff.mu.Lock()
		owner, main, hook := ff.ownerURL, ff.mainURL, ff.onOwnerURL
		ff.mu.Unlock()
		switch method {
		case "Runtime.evaluate":
			expr, _ := params["expression"].(string)
			switch {
			case strings.Contains(expr, "querySelector"):
				return map[string]any{"result": map[string]any{"type": "object", "objectId": "obj1"}}, true
			case expr == "location.href":
				return map[string]any{"result": map[string]any{"value": main}}, true
			}
		case "Runtime.callFunctionOn":
			decl, _ := params["functionDeclaration"].(string)
			switch {
			case strings.Contains(decl, "ownerDocument"):
				if hook != nil {
					hook()
				}
				return map[string]any{"result": map[string]any{"value": owner}}, true
			case strings.Contains(decl, "HTMLInputElement"):
				return map[string]any{"result": map[string]any{"value": map[string]any{"file": true, "disabled": false}}}, true
			}
		case "DOM.describeNode":
			return map[string]any{"node": map[string]any{"backendNodeId": float64(7)}}, true
		case "DOM.resolveNode":
			return map[string]any{"object": map[string]any{"objectId": "obj1"}}, true
		}
		return nil, false
	}
}

func probeCount(f *fakeCDPServer) int {
	n := 0
	for _, c := range f.callsFor("Runtime.evaluate") {
		if e, _ := c.Params["expression"].(string); strings.Contains(e, "scrollX") {
			n++
		}
	}
	return n
}

const portalURL = "https://portal.example/file?x=1#step2"

func TestSetFileInputFenceMatchDispatchesOnTheCheckedObject(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	(&fileInputFake{ownerURL: portalURL, mainURL: portalURL}).install(f)
	if _, err := m.StageFile(st.ID, "a.pdf", []byte("a")); err != nil {
		t.Fatal(err)
	}
	out, err := m.SetFileInput(st.ID, "#f", 0, "a.pdf", portalURL)
	if err != nil {
		t.Fatal(err)
	}
	calls := f.callsFor("DOM.setFileInputFiles")
	if len(calls) != 1 || calls[0].Params["objectId"] != "obj1" {
		t.Fatalf("setFileInputFiles calls = %+v", calls)
	}
	if out.Effects == nil || out.Effects.FilesSet != 1 {
		t.Fatalf("effects = %+v", out.Effects)
	}
}

// A mismatch is returned as ErrTargetChanged itself (not a wrapped copy), so
// the action machinery treats it as a refusal: nothing dispatched, no
// read-back probe, and no URL in the result.
func TestSetFileInputFenceMismatchIsARefusalWithNoReadBack(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	// Differs from the expectation only in the fragment.
	(&fileInputFake{ownerURL: "https://portal.example/file?x=1#step3", mainURL: portalURL}).install(f)
	if _, err := m.StageFile(st.ID, "a.pdf", []byte("a")); err != nil {
		t.Fatal(err)
	}
	out, err := m.SetFileInput(st.ID, "#f", 0, "a.pdf", portalURL)
	if !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("err = %v, want ErrTargetChanged", err)
	}
	if _, partial := AsActionIncomplete(err); partial {
		t.Fatal("a refusal must not become a partial-effect report")
	}
	if n := len(f.callsFor("DOM.setFileInputFiles")); n != 0 {
		t.Fatalf("%d setFileInputFiles calls on a mismatch", n)
	}
	if n := probeCount(f); n != 1 {
		t.Fatalf("%d page probes: only the before-probe may run, no read-back", n)
	}
	if out.URL != "" || out.Effects != nil || strings.Contains(err.Error(), "portal.example") {
		t.Fatalf("the refusal carries page data: %+v / %v", out, err)
	}
}

// The check is on the document that owns the input, not the main frame: a
// matching main frame does not rescue a differing owner document.
func TestSetFileInputFenceUsesOwnerDocumentNotMainFrame(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	(&fileInputFake{ownerURL: "https://frame.example/child", mainURL: portalURL}).install(f)
	if _, err := m.StageFile(st.ID, "a.pdf", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetFileInput(st.ID, "#f", 0, "a.pdf", portalURL); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.callsFor("DOM.setFileInputFiles")); n != 0 {
		t.Fatalf("%d dispatches for a differing owner document", n)
	}
}

// A human taking control during the URL read stops the dispatch.
func TestSetFileInputHandoffDuringURLReadStopsDispatch(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	ff := &fileInputFake{ownerURL: portalURL, mainURL: portalURL}
	ff.onOwnerURL = func() { _, _ = m.Handoff(st.ID) }
	ff.install(f)
	if _, err := m.StageFile(st.ID, "a.pdf", []byte("a")); err != nil {
		t.Fatal(err)
	}
	_, err := m.SetFileInput(st.ID, "#f", 0, "a.pdf", portalURL)
	if !errors.Is(err, ErrHandedOff) {
		t.Fatalf("err = %v, want ErrHandedOff unchanged", err)
	}
	if n := len(f.callsFor("DOM.setFileInputFiles")); n != 0 {
		t.Fatalf("%d dispatches after the human took control", n)
	}
}

// The chooser path resolves the node and dispatches on that resolved object.
func TestSetFileInputByChooserNodeResolvesAndFencesTheNode(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	(&fileInputFake{ownerURL: portalURL, mainURL: portalURL}).install(f)
	if _, err := m.StageFile(st.ID, "a.pdf", []byte("a")); err != nil {
		t.Fatal(err)
	}
	s, _ := m.sessionForAction(st.ID)
	s.handleChooserEvent("Page.fileChooserOpened", map[string]any{"backendNodeId": float64(9), "mode": "selectSingle"})
	if _, err := m.SetFileInput(st.ID, "", 9, "a.pdf", portalURL); err != nil {
		t.Fatal(err)
	}
	if rc := f.callsFor("DOM.resolveNode"); len(rc) != 1 || rc[0].Params["backendNodeId"] != float64(9) {
		t.Fatalf("resolveNode calls = %+v", rc)
	}
	if sc := f.callsFor("DOM.setFileInputFiles"); len(sc) != 1 || sc[0].Params["objectId"] != "obj1" {
		t.Fatalf("setFileInputFiles calls = %+v", sc)
	}
	if got, _ := m.SessionStatus(st.ID); got.FileChooser != nil {
		t.Fatalf("the chooser should clear: %+v", got.FileChooser)
	}
}

func TestRedactPathKeepsRefusalsAndIdentity(t *testing.T) {
	for _, e := range []error{ErrTargetChanged, ErrHandedOff, ErrNotStarted, ErrPageRefused} {
		if got := redactPath(e, "/x", "a"); got != e {
			t.Fatalf("%v was rewrapped", e)
		}
	}
	base := errors.New("CDP error: cannot read /secret/dir/a.pdf")
	got := redactPath(base, "/secret/dir/a.pdf", "a.pdf")
	if strings.Contains(got.Error(), "/secret/dir") || !errors.Is(got, base) {
		t.Fatalf("redaction broke: %v", got)
	}
	if same := errors.New("no path here"); redactPath(same, "/secret", "a") != same {
		t.Fatal("an error without the path must be returned as is")
	}
}

func TestResolveStagedFileDoesNotCreateStagingDir(t *testing.T) {
	m, st := startFakeSession(t)
	s, _ := m.sessionForAction(st.ID)
	if err := os.RemoveAll(s.stagingDirPath()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.resolveStagedFile("a.pdf"); !errors.Is(err, ErrStagedPathRefused) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(s.stagingDirPath()); err == nil {
		t.Fatal("resolveStagedFile created the staging dir")
	}
}
