package localcobrowse

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

const commitFixturePage = `<!doctype html><title>Synthetic</title><h1 id="t">Commit page</h1>
<form id="f" method="post" action="/send"><input type="hidden" name="to" value="bob"><textarea id="msg" name="msg">hello</textarea><button id="send" type="submit">Send</button></form>
<button id="expand" type="button">Expand menu</button>
<form role="search" action="/s"><input id="search" type="search" name="q"><button id="go" type="submit">Go</button></form>
<a id="same" href="/next">Next page</a><a id="out" href="http://other.test/x">Partner</a>
<iframe id="frame" src="about:blank"></iframe>
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
	if send.Name != "Send" || !send.InForm || send.FormMethod != "post" || !strings.Contains(send.Content, "to=bob") || !strings.Contains(send.Content, "msg=hello") {
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
	_ = before
}
