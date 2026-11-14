package localcobrowse

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

const commitFixturePage = `<!doctype html><title>Synthetic</title><h1 id="t">Commit page</h1>
<form id="f" method="post" action="/send"><input type="hidden" name="to" value="bob"><textarea id="msg" name="msg">hello SECRET-TA-TEXT</textarea><button id="send" type="submit">Send</button></form>
<button id="expand" type="button">Expand menu</button>
<form role="search" action="/s"><input id="search" type="search" name="q"><button id="go" type="submit">Go</button></form>
<a id="same" href="/next">Next page</a><a id="out" href="http://other.test/x">Partner</a>
<iframe id="frame" src="about:blank"></iframe>
<div id="ce" contenteditable="true">SECRET-CE-TEXT</div>
<closed-host id="ch" style="display:block;width:100px;height:30px"></closed-host>
<script>customElements.define("closed-host", class extends HTMLElement { constructor(){ super(); this.attachShadow({mode:"closed"}).innerHTML = "<button>Send</button>"; } });</script>
<div id="plain">just text</div>
<div id="panel" style="display:flex;width:240px;height:60px"><button id="panelsend" type="button" style="flex:1">Send</button></div>`

// TestRealBrowserCommitActions runs the in-page target probe and the commit
// classifier in a real browser (Windows CI: Chrome and Edge) on a synthetic
// local page: Send and a form Enter ask, harmless controls and a search box do
// not, an iframe target is unknown and so asks, and the fingerprint follows the
// composer's content.
func TestRealBrowserCommitActions(t *testing.T) {
	if os.Getenv("CITADEL_COBROWSE_BROWSER_IT") == "" && os.Getenv("CITADEL_REAL_BROWSER_TESTS") == "" {
		t.Skip("set CITADEL_COBROWSE_BROWSER_IT=1 (or CITADEL_REAL_BROWSER_TESTS=1) to run the real-browser commit test")
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, commitFixturePage)
	}))
	defer page.Close()

	m := platform.NewCobrowseSessionManager(filepath.Join(t.TempDir(), "sessions"), 1)
	m.SetLaunchOptions(platform.LaunchOptions{Quiet: true})
	defer m.StopAll()
	st, err := m.StartSession(page.URL + "/")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		res, err := m.ExtractBounded(st.ID, "#t", nil, 100)
		if err == nil && res.Text == "Commit page" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("page never loaded: %+v %v", res, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	probe := func(kind, key, sel string) (Decision, platform.TargetInfo) {
		t.Helper()
		ti, err := m.InspectTarget(st.ID, platform.TargetProbe{Kind: kind, Selector: sel})
		if err != nil {
			t.Fatalf("inspect %s %s: %v", kind, sel, err)
		}
		return Classify(kind, key, ti), ti
	}
	want := func(name string, got Decision, c Class) {
		t.Helper()
		if got.Class != c {
			t.Errorf("%s: got %s (%s), want %s", name, got.Class, got.Reason, c)
		}
	}

	d, send := probe("click", "", "#send")
	want("Send button", d, ClassCommit)
	if send.Name != "Send" || !send.InForm || send.FormMethod != "post" || send.ContentHash == "" || send.Oversized {
		t.Errorf("send target not read: %+v", send)
	}
	d, _ = probe("click", "", "#expand")
	want("Expand menu", d, ClassFill)
	d, _ = probe("click", "", "#go")
	want("search submit", d, ClassFill)
	d, _ = probe("click", "", "#plain")
	want("plain text", d, ClassFill)
	d, _ = probe("click", "", "#msg")
	want("focus the composer", d, ClassFill)
	d, ti := probe("click", "", "#same")
	want("same-site link", d, ClassNavigate)
	if d.Domain != RegistrableDomain(ti.PageURL) {
		t.Errorf("same-site link domain %q vs page %q", d.Domain, RegistrableDomain(ti.PageURL))
	}
	d, _ = probe("click", "", "#out")
	want("other-site link", d, ClassNavigate)
	if d.Domain != "other.test" {
		t.Errorf("other-site link domain = %q", d.Domain)
	}
	// A selector for a container whose centre is the Send button is the Send.
	d, panel := probe("click", "", "#panel")
	want("container whose centre is Send", d, ClassCommit)
	if panel.Name != "Send" {
		t.Errorf("the probe must resolve the element the click lands on: %+v", panel)
	}
	d, _ = probe("click", "", "#frame")
	want("iframe target", d, ClassCommit)
	d, _ = probe("click", "", "#missing")
	want("no such element", d, ClassCommit)

	// Enter in the composer sends; Enter in the search box does not.
	if _, err := m.Click(st.ID, "#msg", nil, nil); err != nil {
		t.Fatal(err)
	}
	d, before := probe("key", "Enter", "")
	want("Enter in the composer", d, ClassCommit)
	if strings.Contains(before.Name, "SECRET") || strings.Contains(before.Name, "hello") {
		t.Errorf("the composer's text must never become its label: %q", before.Name)
	}
	if _, err := m.Click(st.ID, "#ce", nil, nil); err != nil {
		t.Fatal(err)
	}
	d, ce := probe("key", "Enter", "")
	want("Enter in a contenteditable", d, ClassCommit)
	if strings.Contains(ce.Name, "SECRET") {
		t.Errorf("a contenteditable's text must never become its label: %q", ce.Name)
	}
	if _, ce2 := probe("click", "", "#ce"); strings.Contains(ce2.Name, "SECRET") {
		t.Errorf("selector probe leaked contenteditable text: %q", ce2.Name)
	}
	d, _ = probe("click", "", "#ch")
	want("closed shadow root host", d, ClassCommit)
	if _, err := m.Click(st.ID, "#plain", nil, nil); err != nil {
		t.Fatal(err)
	}
	d, _ = probe("key", "Delete", "")
	want("Delete outside a field", d, ClassCommit)
	if _, err := m.Click(st.ID, "#search", nil, nil); err != nil {
		t.Fatal(err)
	}
	d, _ = probe("key", "Enter", "")
	want("Enter in the search box", d, ClassFill)

	// The fingerprint follows what is about to be sent.
	_, a := probe("click", "", "#send")
	fa := fingerprint(actionRef{Kind: "click", Selector: "#send"}, a)
	if _, err := m.Click(st.ID, "#msg", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Type(st.ID, " plus more"); err != nil {
		t.Fatal(err)
	}
	_, b := probe("click", "", "#send")
	if fb := fingerprint(actionRef{Kind: "click", Selector: "#send"}, b); fa == fb {
		t.Error("editing the composer must change the fingerprint")
	}
}

// TestRealBrowserCommitActionsLongContent checks, in a real browser, that the
// content digest covers everything: a hidden recipient or message suffix beyond
// the old 16,000 character cut changes the digest, and a form too large to hash
// is reported oversized.
func TestRealBrowserCommitActionsLongContent(t *testing.T) {
	if os.Getenv("CITADEL_COBROWSE_BROWSER_IT") == "" && os.Getenv("CITADEL_REAL_BROWSER_TESTS") == "" {
		t.Skip("set CITADEL_COBROWSE_BROWSER_IT=1 (or CITADEL_REAL_BROWSER_TESTS=1) to run the real-browser content test")
	}
	var mu sync.Mutex
	version, recipient, suffix := 1, "bob", "end-a"
	body := strings.Repeat("x", 20000)
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		v, rc, sf := version, recipient, suffix
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/amb" {
			// Field snapshots whose naive "name=value" lines would be identical.
			var fields string
			switch r.URL.Query().Get("v") {
			case "1":
				fields = "<textarea name=\"msg\">hello\nrecipient=bob</textarea>"
			case "2":
				fields = "<textarea name=\"msg\">hello</textarea><input type=\"hidden\" name=\"recipient\" value=\"bob\">"
			case "3":
				fields = "<input type=\"text\" name=\"a=b\" value=\"c\">"
			case "4":
				fields = "<input type=\"text\" name=\"a\" value=\"b=c\">"
			}
			fmt.Fprintf(w, `<!doctype html><h1 id="t">amb%s</h1><form method="post" action="/send">%s<button id="send" type="submit">Send</button></form>`, r.URL.Query().Get("v"), fields)
			return
		}
		if r.URL.Path == "/huge" {
			fmt.Fprintf(w, `<!doctype html><h1 id="t">huge</h1><form method="post" action="/send"><textarea name="msg">%s</textarea><button id="send" type="submit">Send</button></form>`, strings.Repeat("y", 5_000_000))
			return
		}
		fmt.Fprintf(w, `<!doctype html><h1 id="t">v%d</h1><form method="post" action="/send"><textarea name="msg">%s%s</textarea><input type="hidden" name="to" value="%s"><button id="send" type="submit">Send</button></form>`, v, body, sf, rc)
	}))
	defer page.Close()

	m := platform.NewCobrowseSessionManager(filepath.Join(t.TempDir(), "sessions"), 1)
	m.SetLaunchOptions(platform.LaunchOptions{Quiet: true})
	defer m.StopAll()
	st, err := m.StartSession(page.URL + "/long")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor := func(text string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			res, err := m.ExtractBounded(st.ID, "#t", nil, 100)
			if err == nil && res.Text == text {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("page never showed %q: %+v %v", text, res, err)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	digest := func() platform.TargetInfo {
		t.Helper()
		ti, err := m.InspectTarget(st.ID, platform.TargetProbe{Kind: "click", Selector: "#send"})
		if err != nil || !ti.Resolved {
			t.Fatalf("inspect: %+v %v", ti, err)
		}
		return ti
	}
	waitFor("v1")
	base := digest()
	if base.ContentHash == "" || base.Oversized {
		t.Fatalf("a 20,000 character form must be hashed in full: %+v", base)
	}
	reload := func(v int, rc, sf string) {
		mu.Lock()
		version, recipient, suffix = v, rc, sf
		mu.Unlock()
		if _, err := m.Navigate(st.ID, page.URL+"/long"); err != nil {
			t.Fatal(err)
		}
		waitFor(fmt.Sprintf("v%d", v))
	}
	reload(2, "mallory", "end-a") // only the hidden recipient, beyond the old cut, changes
	if d := digest(); d.ContentHash == base.ContentHash {
		t.Error("a changed hidden recipient beyond 16,000 characters must change the digest")
	}
	reload(3, "bob", "end-b") // only the message suffix changes
	if d := digest(); d.ContentHash == base.ContentHash {
		t.Error("a changed message suffix beyond 16,000 characters must change the digest")
	}
	reload(4, "bob", "end-a") // same content as the base again
	if d := digest(); d.ContentHash != base.ContentHash {
		t.Error("the digest must be stable for identical content")
	}
	// Delimiters inside names and values must not move field boundaries: these
	// pairs are identical as "name=value" lines but submit different payloads.
	ambDigest := func(v string) string {
		t.Helper()
		if _, err := m.Navigate(st.ID, page.URL+"/amb?v="+v); err != nil {
			t.Fatal(err)
		}
		waitFor("amb" + v)
		d := digest()
		if d.ContentHash == "" || d.Oversized {
			t.Fatalf("variant %s: %+v", v, d)
		}
		return d.ContentHash
	}
	if ambDigest("1") == ambDigest("2") {
		t.Error("a newline inside a value must not collide with a following hidden field")
	}
	if ambDigest("3") == ambDigest("4") {
		t.Error("an equals sign inside a name must not collide with one inside a value")
	}
	if _, err := m.Navigate(st.ID, page.URL+"/huge"); err != nil {
		t.Fatal(err)
	}
	waitFor("huge")
	if d := digest(); !d.Oversized || d.ContentHash != "" {
		t.Errorf("a form too large to hash must be oversized: %+v", d)
	}
}
