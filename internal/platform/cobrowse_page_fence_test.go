package platform

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

type fakeFrame struct{ ID, URL, Fragment string }

func frameTreeResult(frames []fakeFrame) map[string]any {
	if len(frames) == 0 {
		return map[string]any{}
	}
	frame := func(f fakeFrame) map[string]any {
		m := map[string]any{"id": f.ID, "url": f.URL}
		if f.Fragment != "" {
			m["urlFragment"] = f.Fragment
		}
		return m
	}
	var kids []any
	for _, f := range frames[1:] {
		kids = append(kids, map[string]any{"frame": frame(f)})
	}
	return map[string]any{"frameTree": map[string]any{"frame": frame(frames[0]), "childFrames": kids}}
}

// fenceCaller is a cdpCaller for the pure setFilesIfPageIs step.
type fenceCaller struct {
	frames []fakeFrame
	calls  []string
	params map[string]map[string]any
}

func (f *fenceCaller) Call(method string, params map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, method)
	if f.params == nil {
		f.params = map[string]map[string]any{}
	}
	f.params[method] = params
	if method == "Page.getFrameTree" {
		return frameTreeResult(f.frames), nil
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
	// url and urlFragment are joined: the fragment includes its '#'.
	f := &fenceCaller{frames: []fakeFrame{{ID: "R", URL: "https://portal.example/file?x=1", Fragment: "#step2"}}}
	var eff ActionEffects
	if err := setFilesIfPageIs(f, "R", "obj1", "https://portal.example/file?x=1#step2", []string{"/p/a.pdf"}, nil, &eff); err != nil {
		t.Fatal(err)
	}
	if f.setCalls() != 1 || eff.FilesSet != 1 || !eff.Dispatched {
		t.Fatalf("calls=%v eff=%+v", f.calls, eff)
	}
	if f.params["DOM.setFileInputFiles"]["objectId"] != "obj1" {
		t.Fatalf("dispatch params = %v", f.params["DOM.setFileInputFiles"])
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "Runtime.") {
			t.Fatalf("the fence ran page script: %v", f.calls)
		}
	}
}

func TestFileUploadPageFenceMismatchRefusesWithNothingDispatched(t *testing.T) {
	const want = "https://portal.example/file?x=1#step2"
	for _, cur := range []fakeFrame{
		{ID: "R", URL: "https://portal.example/file?x=1", Fragment: "#step3"}, // fragment only
		{ID: "R", URL: "https://portal.example/file?x=1"},
		{ID: "R", URL: "https://portal.example/file?x=2", Fragment: "#step2"},
		{ID: "R", URL: "https://portal.example/file?x=1", Fragment: "#step2/"},
		{ID: "R", URL: "https://other.example/file?x=1", Fragment: "#step2"},
		{ID: "R", URL: ""},
		{ID: "OTHER", URL: "https://portal.example/file?x=1", Fragment: "#step2"}, // frame absent
	} {
		f := &fenceCaller{frames: []fakeFrame{cur}}
		var eff ActionEffects
		err := setFilesIfPageIs(f, "R", "obj1", want, []string{"/p/a.pdf"}, nil, &eff)
		if !errors.Is(err, ErrTargetChanged) {
			t.Fatalf("%+v: got %v, want ErrTargetChanged", cur, err)
		}
		if f.setCalls() != 0 || eff.Dispatched || eff.FilesSet != 0 || eff.anyEffect() {
			t.Fatalf("%+v: something was done: calls=%v eff=%+v", cur, f.calls, eff)
		}
		if strings.Contains(err.Error(), "portal.example") || strings.Contains(err.Error(), "other.example") {
			t.Fatalf("error carries a URL: %v", err)
		}
	}
}

func TestFileUploadPageFenceDriverFenceRunsAfterTheComparison(t *testing.T) {
	const u = "https://portal.example/f"
	f := &fenceCaller{frames: []fakeFrame{{ID: "R", URL: u}}}
	var eff ActionEffects
	err := setFilesIfPageIs(f, "R", "obj1", u, nil, func() error { return ErrHandedOff }, &eff)
	if !errors.Is(err, ErrHandedOff) || f.setCalls() != 0 || eff.Dispatched {
		t.Fatalf("err=%v calls=%v eff=%+v", err, f.calls, eff)
	}
	var at string
	f2 := &fenceCaller{frames: []fakeFrame{{ID: "R", URL: u}}}
	_ = setFilesIfPageIs(f2, "R", "obj1", u, nil, func() error { at = strings.Join(f2.calls, ","); return nil }, &eff)
	if at != "Page.getFrameTree" {
		t.Fatalf("driver fence ran at %q, want right after the URL read", at)
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

// fileInputFake is a page whose real frame URLs are `frames` (the browser's
// view). A hostile page is modelled by spoofURL: any page-world read of
// ownerDocument or location would return it, so a fence that used page script
// would be fooled. The node only exists in isolated world 42.
type fileInputFake struct {
	mu     sync.Mutex
	frames []fakeFrame
	// spoofURL is what page script would claim.
	spoofURL string
	// onFrameTree runs on every Page.getFrameTree.
	onFrameTree func(n int)
	trees       int
}

func (ff *fileInputFake) install(f *fakeCDPServer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evaluateValue = func(expr string) any {
		if expr == "location.href" {
			return ff.spoofURL
		}
		return nil
	}
	f.custom = func(method string, params map[string]any) (map[string]any, bool) {
		ff.mu.Lock()
		frames, spoof, hook := ff.frames, ff.spoofURL, ff.onFrameTree
		ff.mu.Unlock()
		switch method {
		case "Page.getFrameTree":
			ff.mu.Lock()
			ff.trees++
			n := ff.trees
			ff.mu.Unlock()
			if hook != nil {
				hook(n)
			}
			ff.mu.Lock()
			frames = ff.frames
			ff.mu.Unlock()
			return frameTreeResult(frames), true
		case "Page.createIsolatedWorld":
			return map[string]any{"executionContextId": float64(42)}, true
		case "Runtime.evaluate":
			expr, _ := params["expression"].(string)
			if strings.Contains(expr, "querySelector") {
				if params["contextId"] != float64(42) {
					return map[string]any{}, true // not in the isolated world: nothing
				}
				return map[string]any{"result": map[string]any{"type": "object", "objectId": "obj1"}}, true
			}
		case "Runtime.callFunctionOn":
			decl, _ := params["functionDeclaration"].(string)
			switch {
			case strings.Contains(decl, "ownerDocument"):
				return map[string]any{"result": map[string]any{"value": spoof}}, true
			case strings.Contains(decl, "HTMLInputElement"):
				return map[string]any{"result": map[string]any{"value": map[string]any{"file": true, "disabled": false}}}, true
			}
		case "DOM.describeNode":
			return map[string]any{"node": map[string]any{"backendNodeId": float64(7)}}, true
		case "DOM.resolveNode":
			if params["executionContextId"] != float64(42) {
				return map[string]any{}, true
			}
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

// Nothing the fence relies on is read by page script.
func noPageWorldFenceReads(t *testing.T, f *fakeCDPServer) {
	t.Helper()
	for _, c := range f.callsFor("Runtime.callFunctionOn") {
		if d, _ := c.Params["functionDeclaration"].(string); strings.Contains(d, "ownerDocument") || strings.Contains(d, "location") {
			t.Fatalf("the fence evaluated page script: %v", d)
		}
	}
	for _, c := range f.callsFor("Runtime.evaluate") {
		e, _ := c.Params["expression"].(string)
		if e == "location.href" || strings.Contains(e, "ownerDocument") {
			t.Fatalf("the fence evaluated page script: %v", e)
		}
		if strings.Contains(e, "querySelector") && c.Params["contextId"] != float64(42) {
			t.Fatalf("the target was looked up in the page's own world: %v", c.Params)
		}
	}
}

const portalURL = "https://portal.example/file?x=1#step2"

var portalFrame = fakeFrame{ID: "R", URL: "https://portal.example/file?x=1", Fragment: "#step2"}

func TestSetFileInputUsesFrameTreeAndAnIsolatedWorld(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	(&fileInputFake{frames: []fakeFrame{portalFrame}, spoofURL: portalURL}).install(f)
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
	if w := f.callsFor("Page.createIsolatedWorld"); len(w) != 1 || w[0].Params["frameId"] != "R" {
		t.Fatalf("isolated world calls = %+v", w)
	}
	noPageWorldFenceReads(t, f)
	if out.Effects == nil || out.Effects.FilesSet != 1 {
		t.Fatalf("effects = %+v", out.Effects)
	}
}

// A hostile page whose script claims the expected URL while the browser says
// otherwise is refused: only the browser's frame URL counts. Differing only in
// the fragment is a mismatch.
func TestSetFileInputSpoofedPageScriptCannotPassTheFence(t *testing.T) {
	for _, real := range []fakeFrame{
		{ID: "R", URL: "https://portal.example/file?x=1", Fragment: "#step3"},
		{ID: "R", URL: "https://evil.example/", Fragment: ""},
	} {
		m, st, f := startSessionOnFakeCDP(t)
		(&fileInputFake{frames: []fakeFrame{real}, spoofURL: portalURL}).install(f)
		if _, err := m.StageFile(st.ID, "a.pdf", []byte("a")); err != nil {
			t.Fatal(err)
		}
		out, err := m.SetFileInput(st.ID, "#f", 0, "a.pdf", portalURL)
		if !errors.Is(err, ErrTargetChanged) {
			t.Fatalf("%+v: err = %v, want ErrTargetChanged", real, err)
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
		noPageWorldFenceReads(t, f)
	}
}

// The URL is read again right before the dispatch: a page that navigates
// between the two reads is refused.
func TestSetFileInputRereadsFrameURLBeforeDispatch(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	ff := &fileInputFake{frames: []fakeFrame{portalFrame}}
	// Reads: 1 root lookup, 2 early check, 3 the re-read before dispatch.
	ff.onFrameTree = func(n int) {
		if n >= 3 {
			ff.mu.Lock()
			ff.frames = []fakeFrame{{ID: "R", URL: "https://evil.example/"}}
			ff.mu.Unlock()
		}
	}
	ff.install(f)
	if _, err := m.StageFile(st.ID, "a.pdf", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetFileInput(st.ID, "#f", 0, "a.pdf", portalURL); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.callsFor("DOM.setFileInputFiles")); n != 0 {
		t.Fatalf("%d dispatches after the page moved", n)
	}
}

// A human taking control during the final URL read stops the dispatch.
func TestSetFileInputHandoffDuringURLReadStopsDispatch(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	ff := &fileInputFake{frames: []fakeFrame{portalFrame}}
	ff.onFrameTree = func(n int) {
		if n >= 3 { // the re-read right before the dispatch
			_, _ = m.Handoff(st.ID)
		}
	}
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

// The chooser path honours the frame the chooser opened in.
func TestSetFileInputChooserFrameIDIsHonoured(t *testing.T) {
	childURL := "https://child.example/form"
	frames := []fakeFrame{portalFrame, {ID: "F2", URL: childURL}}
	m, st, f := startSessionOnFakeCDP(t)
	(&fileInputFake{frames: frames, spoofURL: portalURL}).install(f)
	if _, err := m.StageFile(st.ID, "a.pdf", []byte("a")); err != nil {
		t.Fatal(err)
	}
	s, _ := m.sessionForAction(st.ID)
	open := func() {
		s.handleChooserEvent("Page.fileChooserOpened", map[string]any{"backendNodeId": float64(9), "mode": "selectSingle", "frameId": "F2"})
	}
	// The main frame's URL does not pass for the child frame's chooser.
	open()
	if _, err := m.SetFileInput(st.ID, "", 9, "a.pdf", portalURL); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("main frame URL for a child-frame chooser: %v", err)
	}
	if n := len(f.callsFor("DOM.setFileInputFiles")); n != 0 {
		t.Fatalf("%d dispatches", n)
	}
	// The child frame's own URL does.
	open()
	if _, err := m.SetFileInput(st.ID, "", 9, "a.pdf", childURL); err != nil {
		t.Fatal(err)
	}
	if w := f.callsFor("Page.createIsolatedWorld"); len(w) == 0 || w[len(w)-1].Params["frameId"] != "F2" {
		t.Fatalf("isolated world frame = %+v", w)
	}
	rn := f.callsFor("DOM.resolveNode")
	if len(rn) == 0 || rn[len(rn)-1].Params["backendNodeId"] != float64(9) || rn[len(rn)-1].Params["executionContextId"] != float64(42) {
		t.Fatalf("resolveNode calls = %+v", rn)
	}
	if sc := f.callsFor("DOM.setFileInputFiles"); len(sc) != 1 || sc[0].Params["objectId"] != "obj1" {
		t.Fatalf("setFileInputFiles calls = %+v", sc)
	}
	noPageWorldFenceReads(t, f)
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
