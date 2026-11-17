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
<button id="b" type="button">go</button>
<input id="em" type="email" value="old@example.com">
<input id="num" type="number" value="123">
<input id="col" value="collapse me">
<input id="steal" value="steal">
<input id="late" value="late microtask">
<input id="late2" value="late timeout">
<input id="asyncsel" value="async select">
<input id="rewrite" value="old">
<div id="host" contenteditable="true"><p id="child">child text</p></div>
<script>
document.getElementById("col").select = function(){ HTMLInputElement.prototype.select.call(this); this.setSelectionRange(0, 0); };
document.getElementById("late").select = function(){ var s = this; HTMLInputElement.prototype.select.call(this); queueMicrotask(function(){ s.setSelectionRange(0, 0); }); };
document.getElementById("late2").select = function(){ var s = this; HTMLInputElement.prototype.select.call(this); setTimeout(function(){ s.setSelectionRange(0, 0); }, 0); };
document.getElementById("asyncsel").addEventListener("select", function(){ this.setSelectionRange(0, 0); });
document.getElementById("rewrite").addEventListener("input", function(){ this.value += "!"; });
document.getElementById("steal").addEventListener("focus", function(){ document.getElementById("i").focus(); });
</script>`

// designModePage turns designMode on, which makes everything editable.
const designModePage = `<!doctype html><title>dm</title>
<div id="c" contenteditable="true">dm text</div>
<script>document.designMode = "on";</script>`

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
	document.getElementById("em").value = "old@example.com";
	document.getElementById("num").value = "123";
	document.getElementById("col").value = "collapse me";
	document.getElementById("steal").value = "steal";
	document.getElementById("late").value = "late microtask";
	document.getElementById("late2").value = "late timeout";
	document.getElementById("asyncsel").value = "async select";
	document.getElementById("rewrite").value = "old";
	document.getElementById("host").innerHTML = '<p id="child">child text</p>';
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
		if r.URL.Path == "/dm" {
			fmt.Fprint(w, designModePage)
			return
		}
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

	// 6. The in-page guards: each refuses (or reports) and the page is as it
	// was.
	t.Run("a field that collapses the selection on select", func(t *testing.T) {
		reset()
		if _, err := m.TypeWith(st.ID, "intruder", TypeOptions{Selector: "#col", Clear: true}); err == nil {
			t.Fatal("want the edit refused: the selection no longer spans the content")
		}
		if got := read(`document.getElementById("col").value`); got != "collapse me" {
			t.Fatalf("content = %q", got)
		}
	})
	// The select step passes (the selection really spans everything), then the
	// page collapses it. Only the last verify can catch that.
	for _, c := range []struct{ name, id, want string }{
		{"microtask", "late", "late microtask"},
		{"timeout", "late2", "late timeout"},
	} {
		t.Run("a selection collapsed after select passes ("+c.name+")", func(t *testing.T) {
			reset()
			_, err := m.TypeWith(st.ID, "intruder", TypeOptions{Selector: "#" + c.id, Clear: true})
			if !errors.Is(err, errEditSelectionChanged) {
				t.Fatalf("err = %v, want the final verify to refuse on the selection", err)
			}
			if got := read(`document.getElementById("` + c.id + `").value`); got != c.want {
				t.Fatalf("content = %q, want it unchanged", got)
			}
		})
	}
	// A select event listener collapses the selection at a time the page
	// picks. Whatever happens, the report must match the page: cleared only
	// when the field holds exactly the typed text.
	t.Run("a selection collapsed by an async select event", func(t *testing.T) {
		reset()
		out, err := m.TypeWith(st.ID, "X", TypeOptions{Selector: "#asyncsel", Clear: true})
		got := read(`document.getElementById("asyncsel").value`)
		var eff *ActionEffects
		if err == nil {
			eff = out.Effects
		} else if ae, ok := AsActionIncomplete(err); ok {
			eff = &ae.Effects
		}
		t.Logf("async select fixture: err=%v content=%q effects=%+v", err, got, eff)
		if eff != nil && eff.Cleared && got != "X" {
			t.Fatalf("reported cleared, but the field holds %q", got)
		}
		if err == nil && got != "X" && (eff == nil || !eff.ClearedUnconfirmed) {
			t.Fatalf("the field holds %q and the action claims success without flagging it", got)
		}
	})
	// The page rewrites the field on input, so what it holds is not what was
	// typed: the exact-match read-back must refuse to call that a clear.
	t.Run("a field that rewrites what is typed", func(t *testing.T) {
		reset()
		out, err := m.TypeWith(st.ID, "X", TypeOptions{Selector: "#rewrite", Clear: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := read(`document.getElementById("rewrite").value`); got != "X!" {
			t.Fatalf("content = %q, want the page's rewrite", got)
		}
		if e := out.Effects; e == nil || e.Cleared || !e.ClearedUnconfirmed {
			t.Fatalf("effects = %+v, want cleared false and unconfirmed", out.Effects)
		}
	})
	t.Run("a field that steals focus", func(t *testing.T) {
		reset()
		if _, err := m.TypeWith(st.ID, "intruder", TypeOptions{Selector: "#steal", Clear: true}); err == nil {
			t.Fatal("want the edit refused: the field did not take focus")
		}
		if got := read(`document.getElementById("steal").value + "|" + document.getElementById("i").value`); got != "steal|old input" {
			t.Fatalf("contents = %q", got)
		}
	})
	t.Run("a child inside a contenteditable host", func(t *testing.T) {
		reset()
		if _, err := m.TypeWith(st.ID, "intruder", TypeOptions{Selector: "#child", Clear: true}); err == nil {
			t.Fatal("want the edit refused: only the host itself is a target")
		}
		if got := read(`document.getElementById("host").textContent`); got != "child text" {
			t.Fatalf("content = %q", got)
		}
	})
	t.Run("email and number fields", func(t *testing.T) {
		reset()
		for _, c := range []struct{ sel, read, text string }{
			{"#em", `document.getElementById("em").value`, "new@example.com"},
			{"#num", `document.getElementById("num").value`, "456"},
			{"#em", `document.getElementById("em").value`, ""},
			{"#num", `document.getElementById("num").value`, ""},
		} {
			out, err := m.TypeWith(st.ID, c.text, TypeOptions{Selector: c.sel, Clear: true})
			if err != nil {
				t.Fatalf("%s %q: %v", c.sel, c.text, err)
			}
			if got := read(c.read); got != c.text {
				t.Fatalf("%s content = %q, want %q", c.sel, got, c.text)
			}
			if e := out.Effects; e == nil || !e.Cleared || e.ClearedUnconfirmed {
				t.Fatalf("%s effects = %+v", c.sel, out.Effects)
			}
		}
	})
	t.Run("designMode on", func(t *testing.T) {
		if _, err := m.Navigate(st.ID, srv.URL+"/dm"); err != nil {
			t.Fatalf("navigate: %v", err)
		}
		waitCond(t, 30*time.Second, "the designMode page", func() bool {
			_, err := m.Extract(st.ID, "#c", nil)
			return err == nil
		})
		if _, err := m.TypeWith(st.ID, "intruder", TypeOptions{Selector: "#c", Clear: true}); err == nil {
			t.Fatal("want the edit refused under designMode")
		}
		if _, err := m.TypeWith(st.ID, "intruder", TypeOptions{Selector: "body", Clear: true}); err == nil {
			t.Fatal("want the body refused under designMode")
		}
		if got := read(`document.getElementById("c").textContent`); got != "dm text" {
			t.Fatalf("content = %q", got)
		}
	})
}
