package platform

import (
	"errors"
	"strings"
	"testing"
)

type fenceCaller struct {
	href  string
	calls []string
}

func (f *fenceCaller) Call(method string, _ map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, method)
	if method == "Runtime.evaluate" {
		return map[string]any{"result": map[string]any{"value": f.href}}, nil
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
	f := &fenceCaller{href: u}
	var eff ActionEffects
	if err := setFilesIfPageIs(f, u, map[string]any{}, &eff); err != nil {
		t.Fatal(err)
	}
	if f.setCalls() != 1 || eff.FilesSet != 1 || !eff.Dispatched {
		t.Fatalf("calls=%v eff=%+v", f.calls, eff)
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
		f := &fenceCaller{href: cur}
		var eff ActionEffects
		err := setFilesIfPageIs(f, u, map[string]any{}, &eff)
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
