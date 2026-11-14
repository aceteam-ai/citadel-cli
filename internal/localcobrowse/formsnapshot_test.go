package localcobrowse

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// fakeEl is a form control in the fake DOM the Node-backed tests run the page's
// own inspectForm against.
type fakeEl struct {
	TagName         string            `json:"tagName"`
	Type            string            `json:"type,omitempty"`
	Name            string            `json:"name,omitempty"`
	ID              string            `json:"id,omitempty"`
	Value           *string           `json:"value,omitempty"`
	Checked         bool              `json:"checked,omitempty"`
	Disabled        bool              `json:"disabled,omitempty"`
	ContentEditable bool              `json:"isContentEditable,omitempty"`
	Selected        []map[string]any  `json:"selectedOptions,omitempty"`
	Attrs           map[string]string `json:"attrs,omitempty"`
}

type fakeForm struct {
	Attrs    map[string]string `json:"attrs,omitempty"`
	Elements []fakeEl          `json:"elements"`
}

// scenario is one action: the form, which element is the target (-1 for none
// in the form), and whether the target is a text-entry element.
type scenario struct {
	Form    *fakeForm `json:"form"`
	T       int       `json:"t"`
	TEntry  bool      `json:"tEntry"`
	TOutsid *fakeEl   `json:"tOutside,omitempty"`
}

type inspected struct {
	Bad       string `json:"bad"`
	Oversized bool   `json:"oversized"`
	Digest    string `json:"digest"`
	Action    string `json:"action"`
	Method    string `json:"method"`
	Enctype   string `json:"enctype"`
	Target    string `json:"target"`
}

func sp(s string) *string { return &s }

func fel(tag, typ, name, val string) fakeEl {
	return fakeEl{TagName: tag, Type: typ, Name: name, Value: sp(val)}
}

func button(name, val string, attrs map[string]string) fakeEl {
	e := fel("BUTTON", "submit", name, val)
	e.Attrs = attrs
	return e
}

// runInspect runs inspectForm, the code embedded in the inspector, under Node.
// It skips where Node is absent; the real-browser jobs cover the same cases
// against a live DOM.
func runInspect(t *testing.T, ss ...scenario) []inspected {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the real-browser job covers the inspector")
	}
	script := platform.FormDigestScript + platform.FormSnapshotScript + `
const env = {
  attr: (e, n) => (e.attrs && Object.prototype.hasOwnProperty.call(e.attrs, n)) ? e.attrs[n] : null,
  elements: (f) => f.elements,
  disabled: (e) => !!e.disabled,
  text: (e) => (typeof e.value === "string" ? e.value : ""),
  abs: (u) => new URL(u, "http://a.test/page").href,
  base: () => "http://a.test/page",
};
const input = JSON.parse(require("fs").readFileSync(0, "utf8"));
const out = input.map((s) => {
  const form = s.form;
  const t = s.tOutside ? s.tOutside : (s.t >= 0 ? form.elements[s.t] : null);
  return inspectForm(env, form, t, null, !!s.tEntry, 4000000);
});
process.stdout.write(JSON.stringify(out));`
	path := filepath.Join(t.TempDir(), "inspect.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(ss)
	cmd := exec.Command(node, path)
	cmd.Stdin = bytes.NewReader(in)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("node: %v %s", err, errb.String())
	}
	var res []inspected
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || len(res) != len(ss) {
		t.Fatalf("results: %q %v", out.String(), err)
	}
	return res
}

// targetFrom is the TargetInfo the inspector would report for a result.
func targetFrom(r inspected) platform.TargetInfo {
	t := *sendTarget()
	t.ContentHash, t.Oversized = r.Digest, r.Oversized
	t.FormAction, t.FormMethod, t.FormEnctype, t.FormTarget = r.Action, r.Method, r.Enctype, r.Target
	if r.Bad != "" {
		t = platform.TargetInfo{Reason: r.Bad}
	}
	return t
}

// redeem asks for approval of the first state, then presents the second at
// approval time. It returns ApproveCommit's error and whether anything ran.
func redeem(t *testing.T, first, second platform.TargetInfo) (error, bool, string) {
	t.Helper()
	svc, fb, _, id := startGranted(t, nil)
	fb.target = &first
	if _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatalf("must ask: %v", err)
	}
	cv := pendingView(t, svc, id)
	fb.target = &second
	err := svc.ApproveCommit(ctx, id, cv.ID)
	state := ""
	if r := svc.Sessions(id)[0].CommitResult; r != nil {
		state = r.State
	}
	return err, clicks(fb) != 0, state
}

// Each case is a state change the lanes found (or that a browser submission
// would carry). The snapshot must change, or the target must be unverifiable,
// and in no case may the earlier approval run the changed action.
func TestFormSnapshotBindsEverythingSubmitted(t *testing.T) {
	multi := func(vals ...string) fakeEl {
		e := fakeEl{TagName: "SELECT", Type: "select-multiple", Name: "to"}
		for _, v := range vals {
			e.Selected = append(e.Selected, map[string]any{"value": v})
		}
		return e
	}
	form := func(attrs map[string]string, els ...fakeEl) *fakeForm { return &fakeForm{Attrs: attrs, Elements: els} }
	send := button("go", "1", nil)
	cases := map[string]struct{ a, b scenario }{
		"later selected option of a select-multiple": {
			scenario{Form: form(nil, multi("alice", "bob"), send), T: 1},
			scenario{Form: form(nil, multi("alice", "mallory"), send), T: 1},
		},
		"extra selected option": {
			scenario{Form: form(nil, multi("alice"), send), T: 1},
			scenario{Form: form(nil, multi("alice", "mallory"), send), T: 1},
		},
		"submitter formaction override": {
			scenario{Form: form(map[string]string{"action": "/send"}, fel("INPUT", "text", "m", "hi"), send), T: 1},
			scenario{Form: form(map[string]string{"action": "/send"}, fel("INPUT", "text", "m", "hi"), button("go", "1", map[string]string{"formaction": "/exfil"})), T: 1},
		},
		"submitter formmethod override": {
			scenario{Form: form(map[string]string{"method": "post"}, fel("INPUT", "text", "m", "hi"), send), T: 1},
			scenario{Form: form(map[string]string{"method": "post"}, fel("INPUT", "text", "m", "hi"), button("go", "1", map[string]string{"formmethod": "get"})), T: 1},
		},
		"submitter formenctype override": {
			scenario{Form: form(nil, fel("INPUT", "text", "m", "hi"), send), T: 1},
			scenario{Form: form(nil, fel("INPUT", "text", "m", "hi"), button("go", "1", map[string]string{"formenctype": "text/plain"})), T: 1},
		},
		"submitter formtarget override": {
			scenario{Form: form(nil, fel("INPUT", "text", "m", "hi"), send), T: 1},
			scenario{Form: form(nil, fel("INPUT", "text", "m", "hi"), button("go", "1", map[string]string{"formtarget": "_blank"})), T: 1},
		},
		"submitter value": {
			scenario{Form: form(nil, fel("INPUT", "text", "m", "hi"), button("go", "send", nil)), T: 1},
			scenario{Form: form(nil, fel("INPUT", "text", "m", "hi"), button("go", "send-all", nil)), T: 1},
		},
		"default button override on Enter in a field": {
			scenario{Form: form(nil, fel("INPUT", "text", "m", "hi"), send), T: 0, TEntry: true},
			scenario{Form: form(nil, fel("INPUT", "text", "m", "hi"), button("go", "1", map[string]string{"formaction": "/exfil"})), T: 0, TEntry: true},
		},
		"control outside the form element associated by form=": {
			// form.elements includes it once present; the earlier state had none.
			scenario{Form: form(nil, fel("INPUT", "text", "m", "hi"), send), T: 1},
			scenario{Form: form(nil, fel("INPUT", "text", "m", "hi"), send, fel("INPUT", "hidden", "to", "mallory")), T: 1},
		},
		"disabled control becomes enabled": {
			scenario{Form: form(nil, func() fakeEl { e := fel("INPUT", "hidden", "to", "bob"); e.Disabled = true; return e }(), send), T: 1},
			scenario{Form: form(nil, fel("INPUT", "hidden", "to", "bob"), send), T: 1},
		},
		"checkbox checked state": {
			scenario{Form: form(nil, fakeEl{TagName: "INPUT", Type: "checkbox", Name: "agree", Value: sp("on")}, send), T: 1},
			scenario{Form: form(nil, fakeEl{TagName: "INPUT", Type: "checkbox", Name: "agree", Value: sp("on"), Checked: true}, send), T: 1},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := runInspect(t, c.a, c.b)
			fa, fb2 := targetFrom(r[0]), targetFrom(r[1])
			if fingerprint(actionRef{Kind: "click", Selector: "#send"}, fa) == fingerprint(actionRef{Kind: "click", Selector: "#send"}, fb2) {
				t.Fatalf("the two states bind identically: %+v vs %+v", r[0], r[1])
			}
			err, ran, state := redeem(t, fa, fb2)
			if err != nil || ran || state != "changed" {
				t.Fatalf("must resolve as changed with nothing run: err=%v ran=%v state=%q", err, ran, state)
			}
		})
	}

	// Not part of a submission, so not bound: a disabled control's value and a
	// password field. (Excluding them is deliberate; see the snapshot script.)
	t.Run("excluded state does not change the binding", func(t *testing.T) {
		dis := func(v string) fakeEl { e := fel("INPUT", "hidden", "to", v); e.Disabled = true; return e }
		r := runInspect(t,
			scenario{Form: form(nil, dis("a"), fel("INPUT", "password", "pw", "one"), send), T: 2},
			scenario{Form: form(nil, dis("b"), fel("INPUT", "password", "pw", "two"), send), T: 2})
		if r[0].Digest != r[1].Digest {
			t.Fatal("disabled and password fields must not be part of the digest")
		}
	})

	// State the snapshot cannot capture: the target is unverifiable and an
	// approval of it can never be redeemed.
	unverifiable := map[string]scenario{
		"file input present": {Form: form(nil, fel("INPUT", "text", "m", "hi"), fakeEl{TagName: "INPUT", Type: "file", Name: "att", Value: sp("")}, send), T: 2},
		"form-associated custom element without a readable value": {
			Form: form(nil, fakeEl{TagName: "MY-FIELD", Name: "to"}, send), T: 1},
		"composer whose value cannot be read": {
			Form: nil, T: -1, TEntry: true, TOutsid: &fakeEl{TagName: "DIV", Attrs: map[string]string{"role": "textbox"}}},
	}
	for name, sc := range unverifiable {
		t.Run("unverifiable/"+name, func(t *testing.T) {
			r := runInspect(t, sc)[0]
			if r.Bad == "" {
				t.Fatalf("must be marked unverifiable: %+v", r)
			}
			tgt := targetFrom(r)
			svc, fb, _, id := startGranted(t, nil)
			fb.target = &tgt
			if _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); !errors.Is(err, ErrCommitApprovalRequired) {
				t.Fatalf("must ask: %v", err)
			}
			if cv := pendingView(t, svc, id); !cv.Unverifiable {
				t.Fatal("the card must say it cannot be verified")
			}
			if err := svc.ApproveCommit(ctx, id, pendingView(t, svc, id).ID); !errors.Is(err, ErrTargetUnverifiable) || clicks(fb) != 0 {
				t.Fatalf("approval must not run it: %v clicks=%d", err, clicks(fb))
			}
		})
	}
}
