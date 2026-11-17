package localcobrowse

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// The existing Linux and Windows browser-job selectors include this prefix.
// Never run a native browser unless the explicit browser-test opt-in is set.
func TestRealBrowserCommitActionsFileSnapshots(t *testing.T) {
	if os.Getenv("CITADEL_COBROWSE_BROWSER_IT") == "" && os.Getenv("CITADEL_REAL_BROWSER_TESTS") == "" {
		t.Skip("native browser fixture runs in browser CI")
	}
	posts := make(chan string, 1)
	var requests atomic.Int32
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.Method == http.MethodPost {
			requests.Add(1)
			if r.URL.Path == "/delete" {
				fmt.Fprint(w, "deleted")
				return
			}
			file, header, err := r.FormFile("att")
			if err != nil {
				http.Error(w, "fixture upload missing", 400)
				return
			}
			defer file.Close()
			data, err := io.ReadAll(io.LimitReader(file, 100))
			if err == nil && header.Filename == "second.txt" {
				posts <- string(data)
			}
			fmt.Fprint(w, `<p id="done">received</p>`)
			return
		}
		name := `name="att"`
		if r.URL.Query().Get("unnamed") != "" {
			name = ""
		}
		fmt.Fprintf(w, `<!doctype html><h1 id="ready">ready</h1>
<form method="post" action="/upload" enctype="multipart/form-data">
<input id="note" name="note" value="fixture"><input id="file" type="file" %s>
<label for="file" id="label"><span id="labeltext">Upload</span></label>
<label for="send" id="commitlabel" type="file">Next</label>
<label for="send" id="nextlabel"><span id="nexttext">Next</span></label>
<label for="send"><span id="nextpointer" style="cursor:pointer">Continue</span></label>
<label for="send"><span id="nexttab" tabindex="0">Go on</span></label>
<label for="send"><span id="nextrole" role="button">Continue</span></label>
<label for="delete" id="deletelabel">Continue</label>
<label for="file"><label for="send" id="nestedlabel">Go on</label></label>
<label for="absent" id="orphanlabel" type="file">Continue</label>
<label for="file"><span id="pointerchild" style="cursor:pointer">Submit</span></label>
<label for="file"><span id="tabchild" tabindex="0">Submit</span></label>
<label for="file"><input id="checkchild" type="checkbox">Choose</label>
<label for="file"><input id="radiochild" type="radio">Choose</label>
<label for="file" id="actionlabel" onclick="void 0">Submit</label>
<input id="actionfile" type="file" aria-label="Submit" onclick="void 0">
<button id="send" type="submit">Submit</button>
<button id="delete" type="button" onclick="fetch('/delete',{method:'POST'})">Delete</button></form>
<form id="customform" method="post" action="/custom">
<fixture-file id="customfile" name="custom"></fixture-file>
<label for="customfile" id="customlabel">Continue</label>
<button id="customsend" type="submit">Next</button>
<button id="replacecustom" type="button" onclick="document.querySelector('fixture-file').replaceFile()">Replace fixture</button>
</form>
<fixture-shadow id="shadowfile" style="display:block;width:250px;height:40px"></fixture-shadow>
<script>
customElements.define('fixture-file',class extends HTMLElement {
  static formAssociated=true;
  constructor(){super();this._internals=this.attachInternals();this.replaceFile();}
  replaceFile(){this._internals.setFormValue(new File(['fixture'],'custom-'+(this._version=(this._version||0)+1)+'.txt',{type:'text/plain',lastModified:100}));}
});
customElements.define('fixture-shadow',class extends HTMLElement {
  constructor(){super();const root=this.attachShadow({mode:'open'});root.innerHTML='<input type=file name=shadow style="width:100%%;height:40px">';const data=new DataTransfer();data.items.add(new File(['shadow'],'shadow.txt',{type:'text/plain',lastModified:100}));root.querySelector('input').files=data.files;}
});
Object.defineProperty(File.prototype,"name",{get(){return "spoofed.txt"}});
Object.defineProperty(File.prototype,"lastModified",{get(){return 999}});
Object.defineProperty(Blob.prototype,"size",{get(){return 999}});
Object.defineProperty(Blob.prototype,"type",{get(){return "spoof/type"}});
</script>`, name)
	}))
	defer page.Close()
	m := platform.NewCobrowseSessionManager(filepath.Join(t.TempDir(), "sessions"), 1)
	m.SetLaunchOptions(platform.LaunchOptions{Quiet: true})
	defer m.StopAll()
	svc := New(Config{Browser: m, URLPolicy: allowAll})
	st, err := svc.Start(ctx, page.URL+"/", "synthetic upload", "")
	if err != nil {
		t.Fatal(err)
	}
	id := st.SessionID
	waitReady := func() {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			if out, err := m.ExtractBounded(id, "#ready", nil, 20); err == nil && out.Text == "ready" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("fixture did not load")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	waitReady()
	if err := svc.ApproveDomain(id, RegistrableDomain(page.URL), 0); err != nil {
		t.Fatal(err)
	}
	// Baseline regression: named empty file controls used to make the whole
	// form unverifiable; unnamed controls did not. Both now permit a text fill.
	for _, query := range []string{"", "?unnamed=1"} {
		if _, err := m.Navigate(id, page.URL+"/"+query); err != nil {
			t.Fatal(err)
		}
		waitReady()
		ti, err := m.InspectTarget(id, platform.TargetProbe{Kind: "click", Selector: "#note"})
		if err != nil || !ti.Resolved || Classify("click", "", ti).Class != ClassFill {
			t.Fatalf("file-form fill: %+v %v", ti, err)
		}
	}
	if _, err := m.Navigate(id, page.URL+"/"); err != nil {
		t.Fatal(err)
	}
	waitReady()
	for _, selector := range []string{"#file", "#labeltext"} {
		ti, err := m.InspectTarget(id, platform.TargetProbe{Kind: "click", Selector: selector})
		if err != nil || !ti.FileChooser || ti.InputType != "file" || Classify("click", "", ti).Class != ClassFill {
			t.Fatalf("chooser click: %+v %v", ti, err)
		}
	}
	for _, selector := range []string{"#commitlabel", "#nextlabel", "#nexttext", "#nextpointer", "#nexttab", "#nextrole", "#deletelabel", "#nestedlabel", "#actionlabel", "#actionfile", "#pointerchild", "#tabchild", "#customlabel"} {
		ti, err := m.InspectTarget(id, platform.TargetProbe{Kind: "click", Selector: selector})
		if err != nil || Classify("click", "", ti).Class != ClassCommit {
			t.Fatalf("commit label/descendant: %+v %v", ti, err)
		}
	}
	for _, selector := range []string{"#commitlabel", "#nextlabel", "#nexttext", "#nextpointer", "#nexttab", "#nextrole", "#nestedlabel"} {
		ti, err := m.InspectTarget(id, platform.TargetProbe{Kind: "click", Selector: selector})
		if err != nil || ti.Tag != "button" || ti.InputType != "submit" || !ti.Actionable || !ti.InForm {
			t.Fatalf("submit label did not describe its control: %+v %v", ti, err)
		}
	}
	if ti, err := m.InspectTarget(id, platform.TargetProbe{Kind: "click", Selector: "#deletelabel"}); err != nil || ti.Name != "Delete" || ti.Tag != "button" {
		t.Fatalf("delete label hid its control: %+v %v", ti, err)
	}
	if ti, err := m.InspectTarget(id, platform.TargetProbe{Kind: "click", Selector: "#orphanlabel"}); err != nil || !ti.Resolved || ti.FileChooser || Classify("click", "", ti).Class != ClassFill {
		t.Fatalf("null-control label was not harmless: %+v %v", ti, err)
	}
	if ti, err := m.InspectTarget(id, platform.TargetProbe{Kind: "click", Selector: "#customlabel"}); err != nil || ti.Resolved || ti.FileChooser || Classify("click", "", ti).Class != ClassCommit {
		t.Fatalf("uninspectable associated custom control was allowed: %+v %v", ti, err)
	}
	// The coordinate probe follows the real hit through an open shadow root.
	// document.querySelector cannot select that root's private input directly.
	host, err := m.InspectTarget(id, platform.TargetProbe{Kind: "click", Selector: "#shadowfile"})
	if err != nil || host.Rect.W <= 0 || host.Rect.H <= 0 {
		t.Fatalf("shadow fixture has no hit rectangle: %+v %v", host, err)
	}
	x, y := host.Rect.X+host.Rect.W/2, host.Rect.Y+host.Rect.H/2
	if ti, err := m.InspectTarget(id, platform.TargetProbe{Kind: "click", X: &x, Y: &y}); err != nil || !ti.Resolved || !ti.FileChooser || len(ti.FormFiles) != 1 || ti.FormFiles[0].Name != "shadow.txt" || Classify("click", "", ti).Class != ClassFill {
		t.Fatalf("open-shadow file input was not inspected natively: %+v %v", ti, err)
	}
	for _, selector := range []string{"#pointerchild", "#tabchild", "#checkchild", "#radiochild"} {
		ti, err := m.InspectTarget(id, platform.TargetProbe{Kind: "click", Selector: selector})
		if err != nil || ti.Tag == "label" || ti.FileChooser {
			t.Fatalf("interactive child reclassified: %+v %v", ti, err)
		}
	}
	if _, err := svc.Click(ctx, id, "#commitlabel", nil, nil, "submit"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatalf("non-file label did not ask: %v", err)
	}
	if err := svc.DenyCommit(id, pendingView(t, svc, id).ID); err != nil {
		t.Fatal(err)
	}
	attach := func(name string) {
		t.Helper()
		if _, err := m.StageFile(id, name, []byte("fixture-bytes")); err != nil {
			t.Fatal(err)
		}
		if _, err := m.SetFileInput(id, "#file", 0, name, page.URL+"/"); err != nil {
			t.Fatal(err)
		}
	}
	attach("invoice.txt")
	// Exercise the real service, with a selected file present. Non-commit
	// label words must not authorize either multipart submit or Delete POST.
	for _, selector := range []string{"#commitlabel", "#nextlabel", "#nexttext", "#nextpointer", "#nexttab", "#nextrole", "#deletelabel", "#nestedlabel", "#customlabel"} {
		if _, err := svc.Click(ctx, id, selector, nil, nil, "continue"); !errors.Is(err, ErrCommitApprovalRequired) {
			t.Fatalf("label dispatched without approval: %s: %v", selector, err)
		}
		if err := svc.DenyCommit(id, pendingView(t, svc, id).ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Click(ctx, id, "#orphanlabel", nil, nil, "continue"); err != nil {
		t.Fatalf("null-control label click failed: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("an unapproved label caused a POST")
	}
	// A form-associated custom element contributes its native File entry,
	// even though the custom element's activation itself is unverifiable.
	if _, err := svc.Click(ctx, id, "#customsend", nil, nil, "submit"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatal(err)
	}
	customCard := pendingView(t, svc, id)
	if len(customCard.Files) != 1 || customCard.Files[0].Name != "custom-1.txt" || customCard.Files[0].Size != 7 {
		t.Fatalf("custom form File entry missing: %+v", customCard)
	}
	if _, err := m.Click(id, "#replacecustom", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.ApproveCommit(ctx, id, customCard.ID); err != nil {
		t.Fatal(err)
	}
	if got := svc.Sessions(id)[0].CommitResult; got == nil || got.State != "changed" || requests.Load() != 0 {
		t.Fatalf("changed custom File redeemed approval: %+v posts=%d", got, requests.Load())
	}
	if _, err := svc.Click(ctx, id, "#note", nil, nil, "fill"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Type(ctx, id, "x", "fill"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Click(ctx, id, "#send", nil, nil, "submit"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatal(err)
	}
	card := pendingView(t, svc, id)
	if len(card.Files) != 1 || card.Files[0].Name != "invoice.txt" || card.Files[0].Size != 13 || card.Files[0].Type != "text/plain" || card.Files[0].LastModified == 999 {
		t.Fatalf("main-world prototype spoof changed file metadata: %+v", card.Files)
	}
	attach("second.txt")
	if err := svc.ApproveCommit(ctx, id, card.ID); err != nil {
		t.Fatal(err)
	}
	if got := svc.Sessions(id)[0].CommitResult; got == nil || got.State != "changed" {
		t.Fatalf("file swap redeemed old approval: %+v", got)
	}
	select {
	case <-posts:
		t.Fatal("stale approval submitted")
	default:
	}
	if _, err := svc.Click(ctx, id, "#send", nil, nil, "submit"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatal(err)
	}
	card = pendingView(t, svc, id)
	if len(card.Files) != 1 || card.Files[0].Name != "second.txt" {
		t.Fatalf("missing current file: %+v", card.Files)
	}
	if err := svc.ApproveCommit(ctx, id, card.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-posts:
		if data != "fixture-bytes" {
			t.Fatal("uploaded bytes differ")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("approved upload did not arrive")
	}
}
