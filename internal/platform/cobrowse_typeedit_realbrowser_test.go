package platform

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// typeEditPage has one prefilled field of each editable kind, plus fields the
// edit must refuse.
const typeEditPage = `<!doctype html><title>edit</title>
<style>input,textarea,#c{display:block;width:300px;margin:6px;font:16px sans-serif}#c{min-height:40px;border:1px solid #888}</style>
<input id="i" value="old input">
<textarea id="t">old text</textarea>
<div id="c" contenteditable="true">old editable</div>
<input id="ro" value="locked" readonly>
<input id="cb" type="checkbox">
<input id="sa" value="sentinel-A">
<input id="sb" value="">
<button id="b" type="button">go</button>`

// typeEditFields maps each field's selector to the JS expression that reads its
// content back, so the assertions look at the page, not at the action's report.
var typeEditFields = []struct {
	name, selector, read, old string
}{
	{"input", "#i", `document.getElementById("i").value`, "old input"},
	{"textarea", "#t", `document.getElementById("t").value`, "old text"},
	{"contenteditable", "#c", `document.getElementById("c").textContent`, "old editable"},
}

const typeEditReset = `(function(){
	document.getElementById("i").value = "old input";
	document.getElementById("t").value = "old text";
	document.getElementById("c").textContent = "old editable";
	document.getElementById("ro").value = "locked";
	return true;
})()`

// runTypeEditRealBrowser is the shared real-browser scenario (citadel-cli#1730):
// type with selector and clear replaces the prefilled contents of an input, a
// textarea and a contenteditable; Ctrl+A followed by type replaces; an empty
// clear empties; and the refusals leave the page as it was. The OS-specific
// tests supply the manager.
func runTypeEditRealBrowser(t *testing.T, m *CobrowseSessionManager) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, typeEditPage)
	}))
	t.Cleanup(srv.Close)

	st, err := m.StartSession(srv.URL + "/")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = m.StopAll() })
	waitCond(t, 30*time.Second, "the edit page", func() bool {
		_, err := m.Extract(st.ID, "#c", nil)
		return err == nil
	})
	if st.InputEdit != InputEditV1 {
		t.Fatalf("status input_edit = %q", st.InputEdit)
	}
	pg := pin{port: st.DebugPort}
	read := func(expr string) string {
		t.Helper()
		v, err := runtimeEvaluate(pg, expr)
		if err != nil {
			t.Fatalf("read %s: %v", expr, err)
		}
		s, _ := v.(string)
		return s
	}
	reset := func() {
		t.Helper()
		if _, err := runtimeEvaluate(pg, typeEditReset); err != nil {
			t.Fatal(err)
		}
	}
	// Ctrl on Linux and Windows; Cmd+A on darwin is not tested in CI.
	const selectAll = ModCtrl

	for _, f := range typeEditFields {
		// 1. selector + clear replaces.
		t.Run(f.name+"/selector+clear replaces", func(t *testing.T) {
			reset()
			out, err := m.TypeWith(st.ID, "fresh "+f.name, TypeOptions{Selector: f.selector, Clear: true})
			if err != nil {
				t.Fatal(err)
			}
			if got := read(f.read); got != "fresh "+f.name {
				t.Fatalf("content = %q, want the replacement", got)
			}
			if e := out.Effects; e == nil || !e.Focused || !e.Cleared || e.TypedLength != len("fresh "+f.name) {
				t.Fatalf("effects = %+v", out.Effects)
			}
		})
		// 2. Ctrl+A then type replaces; Ctrl+A alone types nothing.
		t.Run(f.name+"/ctrl+a then type replaces", func(t *testing.T) {
			reset()
			if _, err := m.Click(st.ID, f.selector, nil, nil); err != nil {
				t.Fatalf("click: %v", err)
			}
			if _, err := m.KeyWith(st.ID, "a", selectAll); err != nil {
				t.Fatalf("ctrl+a: %v", err)
			}
			if got := read(f.read); got != f.old {
				t.Fatalf("ctrl+a changed the content to %q (it must type nothing)", got)
			}
			if _, err := m.Type(st.ID, "second "+f.name); err != nil {
				t.Fatalf("type: %v", err)
			}
			if got := read(f.read); got != "second "+f.name {
				t.Fatalf("content = %q, want the replacement of the selected text", got)
			}
		})
		// 3. An empty clear empties.
		t.Run(f.name+"/empty clear empties", func(t *testing.T) {
			reset()
			out, err := m.TypeWith(st.ID, "", TypeOptions{Selector: f.selector, Clear: true})
			if err != nil {
				t.Fatal(err)
			}
			if got := read(f.read); got != "" {
				t.Fatalf("content = %q, want empty", got)
			}
			if e := out.Effects; e == nil || !e.Cleared || e.TypedLength != 0 {
				t.Fatalf("effects = %+v", out.Effects)
			}
		})
	}

	// 5. Clipboard and editing chords are not allowed: they are refused, and
	// nothing reaches the second field.
	t.Run("clipboard chords refused", func(t *testing.T) {
		if _, err := m.TypeWith(st.ID, "sentinel-A", TypeOptions{Selector: "#sa", Clear: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := m.KeyWith(st.ID, "a", ModCtrl); err != nil {
			t.Fatalf("ctrl+a: %v", err)
		}
		if _, err := m.KeyWith(st.ID, "c", ModCtrl); !errors.Is(err, ErrKeyChordNotAllowed) {
			t.Fatalf("ctrl+c: err = %v, want a refusal", err)
		}
		if _, err := m.Click(st.ID, "#sb", nil, nil); err != nil {
			t.Fatalf("click: %v", err)
		}
		for _, c := range []struct {
			key string
			mod int
		}{{"v", ModCtrl}, {"v", ModMeta}, {"x", ModCtrl}, {"Delete", ModShift}} {
			if _, err := m.KeyWith(st.ID, c.key, c.mod); !errors.Is(err, ErrKeyChordNotAllowed) {
				t.Fatalf("%s+%d: err = %v, want a refusal", c.key, c.mod, err)
			}
		}
		if got := read(`document.getElementById("sb").value`); got != "" {
			t.Fatalf("the second field holds %q, want it empty", got)
		}
		if got := read(`document.getElementById("sa").value`); got != "sentinel-A" {
			t.Fatalf("the first field holds %q", got)
		}
	})

	// 4. Refusals leave the page as it was.
	t.Run("refusals", func(t *testing.T) {
		reset()
		for name, o := range map[string]TypeOptions{
			"matches nothing":   {Selector: "#nothing", Clear: true},
			"a button":          {Selector: "#b", Clear: true},
			"a checkbox":        {Selector: "#cb", Clear: true},
			"a read-only field": {Selector: "#ro", Clear: true},
		} {
			if _, err := m.TypeWith(st.ID, "intruder", o); err == nil {
				t.Fatalf("%s: want a refusal", name)
			}
		}
		if _, err := m.TypeWith(st.ID, "", TypeOptions{Selector: "#i"}); err == nil {
			t.Fatal("empty text without clear must be refused")
		}
		for _, f := range typeEditFields {
			if got := read(f.read); got != f.old {
				t.Fatalf("%s changed to %q by a refusal", f.name, got)
			}
		}
		if got := read(`document.getElementById("ro").value`); got != "locked" {
			t.Fatalf("read-only field changed to %q", got)
		}
	})
}
