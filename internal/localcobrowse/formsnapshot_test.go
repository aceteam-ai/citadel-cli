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
	// Submitted stands in for the browser's own submission set, new
	// FormData(form, submitter): [name, value] pairs, or [name, {"file":true}].
	Submitted [][]any `json:"submitted,omitempty"`
	Throws    bool    `json:"throws,omitempty"`
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
	Overrides string `json:"overrides"`
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
  has: (e, n) => !!(e.attrs && Object.prototype.hasOwnProperty.call(e.attrs, n)),
  formData: (f, sub) => { if (f.throws) throw new TypeError("submitter"); return f; },
  entries: (fd) => fd.submitted || [],
  isFile: (v) => v !== null && typeof v === "object" && v.file === true,
  inForm: (f, c) => f.elements.indexOf(c) >= 0,
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
	t.FormAction, t.FormMethod, t.FormEnctype, t.FormTarget, t.FormOverrides = r.Action, r.Method, r.Enctype, r.Target, r.Overrides
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
	// form builds a fake form: its elements (for submitter and password lookup)
	// and the entries the browser's FormData would produce for it.
	form := func(attrs map[string]string, submitted [][]any, els ...fakeEl) *fakeForm {
		return &fakeForm{Attrs: attrs, Elements: els, Submitted: submitted}
	}
	ent := func(kv ...string) [][]any {
		var out [][]any
		for i := 0; i+1 < len(kv); i += 2 {
			out = append(out, []any{kv[i], kv[i+1]})
		}
		return out
	}
	send := button("go", "1", nil)
	m := fel("INPUT", "text", "m", "hi")
	cases := map[string]struct{ a, b scenario }{
		"later selected option of a select-multiple": {
			scenario{Form: form(nil, ent("to", "alice", "to", "bob", "go", "1"), multi("alice", "bob"), send), T: 1},
			scenario{Form: form(nil, ent("to", "alice", "to", "mallory", "go", "1"), multi("alice", "mallory"), send), T: 1},
		},
		"extra selected option": {
			scenario{Form: form(nil, ent("to", "alice"), multi("alice"), send), T: 1},
			scenario{Form: form(nil, ent("to", "alice", "to", "mallory"), multi("alice", "mallory"), send), T: 1},
		},
		"custom element whose setFormValue changes while .value is fixed": {
			// The element reports the same value property; the browser's
			// submission set carries what it passed to setFormValue.
			scenario{Form: form(nil, ent("to", "alice"), fakeEl{TagName: "X-FIELD", Name: "to", Value: sp("fixed")}, send), T: 1},
			scenario{Form: form(nil, ent("to", "mallory"), fakeEl{TagName: "X-FIELD", Name: "to", Value: sp("fixed")}, send), T: 1},
		},
		"submitter formaction override": {
			scenario{Form: form(map[string]string{"action": "/send"}, ent("m", "hi"), m, send), T: 1},
			scenario{Form: form(map[string]string{"action": "/send"}, ent("m", "hi"), m, button("go", "1", map[string]string{"formaction": "/exfil"})), T: 1},
		},
		"submitter formmethod override": {
			scenario{Form: form(map[string]string{"method": "post"}, ent("m", "hi"), m, send), T: 1},
			scenario{Form: form(map[string]string{"method": "post"}, ent("m", "hi"), m, button("go", "1", map[string]string{"formmethod": "get"})), T: 1},
		},
		"submitter formenctype override": {
			scenario{Form: form(nil, ent("m", "hi"), m, send), T: 1},
			scenario{Form: form(nil, ent("m", "hi"), m, button("go", "1", map[string]string{"formenctype": "text/plain"})), T: 1},
		},
		"submitter formtarget override": {
			scenario{Form: form(nil, ent("m", "hi"), m, send), T: 1},
			scenario{Form: form(nil, ent("m", "hi"), m, button("go", "1", map[string]string{"formtarget": "_blank"})), T: 1},
		},
		"case-only change of a submitter formtarget": {
			scenario{Form: form(nil, ent("m", "hi"), m, button("go", "1", map[string]string{"formtarget": "Panel"})), T: 1},
			scenario{Form: form(nil, ent("m", "hi"), m, button("go", "1", map[string]string{"formtarget": "panel"})), T: 1},
		},
		"case-only change of a form-level target": {
			scenario{Form: form(map[string]string{"target": "Panel"}, ent("m", "hi"), m, send), T: 1},
			scenario{Form: form(map[string]string{"target": "panel"}, ent("m", "hi"), m, send), T: 1},
		},
		"empty formaction added to the submitter": {
			scenario{Form: form(map[string]string{"action": "/send"}, ent("m", "hi"), m, send), T: 1},
			scenario{Form: form(map[string]string{"action": "/send"}, ent("m", "hi"), m, button("go", "1", map[string]string{"formaction": ""})), T: 1},
		},
		"empty formtarget added to the submitter": {
			scenario{Form: form(map[string]string{"target": "Panel"}, ent("m", "hi"), m, send), T: 1},
			scenario{Form: form(map[string]string{"target": "Panel"}, ent("m", "hi"), m, button("go", "1", map[string]string{"formtarget": ""})), T: 1},
		},
		"empty formtarget added when the form has none": {
			scenario{Form: form(nil, ent("m", "hi"), m, send), T: 1},
			scenario{Form: form(nil, ent("m", "hi"), m, button("go", "1", map[string]string{"formtarget": ""})), T: 1},
		},
		"empty form action added": {
			scenario{Form: form(nil, ent("m", "hi"), m, send), T: 1},
			scenario{Form: form(map[string]string{"action": ""}, ent("m", "hi"), m, send), T: 1},
		},
		"empty form target added": {
			scenario{Form: form(nil, ent("m", "hi"), m, send), T: 1},
			scenario{Form: form(map[string]string{"target": ""}, ent("m", "hi"), m, send), T: 1},
		},
		"empty formmethod and formenctype added": {
			scenario{Form: form(nil, ent("m", "hi"), m, send), T: 1},
			scenario{Form: form(nil, ent("m", "hi"), m, button("go", "1", map[string]string{"formmethod": "", "formenctype": ""})), T: 1},
		},
		"formnovalidate added": {
			scenario{Form: form(nil, ent("m", "hi"), m, send), T: 1},
			scenario{Form: form(nil, ent("m", "hi"), m, button("go", "1", map[string]string{"formnovalidate": ""})), T: 1},
		},
		"novalidate added to the form": {
			scenario{Form: form(nil, ent("m", "hi"), m, send), T: 1},
			scenario{Form: form(map[string]string{"novalidate": ""}, ent("m", "hi"), m, send), T: 1},
		},
		"accept-charset changed": {
			scenario{Form: form(map[string]string{"accept-charset": "utf-8"}, ent("m", "hi"), m, send), T: 1},
			scenario{Form: form(map[string]string{"accept-charset": "iso-8859-1"}, ent("m", "hi"), m, send), T: 1},
		},
		"submitter value": {
			scenario{Form: form(nil, ent("m", "hi", "go", "send"), m, button("go", "send", nil)), T: 1},
			scenario{Form: form(nil, ent("m", "hi", "go", "send-all"), m, button("go", "send-all", nil)), T: 1},
		},
		"default button override on Enter in a field": {
			scenario{Form: form(nil, ent("m", "hi"), m, send), T: 0, TEntry: true},
			scenario{Form: form(nil, ent("m", "hi"), m, button("go", "1", map[string]string{"formaction": "/exfil"})), T: 0, TEntry: true},
		},
		"control outside the form element associated by form=": {
			scenario{Form: form(nil, ent("m", "hi"), m, send), T: 1},
			scenario{Form: form(nil, ent("m", "hi", "to", "mallory"), m, send, fel("INPUT", "hidden", "to", "mallory")), T: 1},
		},
		"disabled control becomes enabled": {
			scenario{Form: form(nil, ent(), func() fakeEl { e := fel("INPUT", "hidden", "to", "bob"); e.Disabled = true; return e }(), send), T: 1},
			scenario{Form: form(nil, ent("to", "bob"), fel("INPUT", "hidden", "to", "bob"), send), T: 1},
		},
		"checkbox becomes checked": {
			scenario{Form: form(nil, ent(), fakeEl{TagName: "INPUT", Type: "checkbox", Name: "agree", Value: sp("on")}, send), T: 1},
			scenario{Form: form(nil, ent("agree", "on"), fakeEl{TagName: "INPUT", Type: "checkbox", Name: "agree", Value: sp("on"), Checked: true}, send), T: 1},
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

	// Reserved target keywords are case-insensitive, so a case-only change of
	// one is not a change.
	t.Run("reserved target keyword case is normalized", func(t *testing.T) {
		r := runInspect(t,
			scenario{Form: form(map[string]string{"target": "_BLANK"}, ent("m", "hi"), m, send), T: 1},
			scenario{Form: form(map[string]string{"target": "_blank"}, ent("m", "hi"), m, send), T: 1})
		if r[0].Target != "_blank" || r[0] != r[1] {
			t.Fatalf("%+v vs %+v", r[0], r[1])
		}
	})

	// Password and one-time-code controls are dropped by name; a disabled
	// control is absent from the submission set by definition.
	t.Run("excluded state does not change the binding", func(t *testing.T) {
		pw := func(v string) fakeEl { return fel("INPUT", "password", "pw", v) }
		r := runInspect(t,
			scenario{Form: form(nil, ent("pw", "one", "m", "hi"), pw("one"), m, send), T: 2},
			scenario{Form: form(nil, ent("pw", "two", "m", "hi"), pw("two"), m, send), T: 2})
		if r[0].Digest != r[1].Digest {
			t.Fatal("a password field must not be part of the digest")
		}
	})

	t.Run("a unique-named password field, unnamed or disabled one does not affect the binding", func(t *testing.T) {
		un := fel("INPUT", "password", "", "x")
		dis := fel("INPUT", "password", "pw2", "x")
		dis.Disabled = true
		r := runInspect(t,
			scenario{Form: form(nil, ent("m", "hi"), un, dis, m, send), T: 3},
			scenario{Form: form(nil, ent("m", "hi"), un, dis, m, send), T: 3})
		if r[0].Bad != "" || r[0].Digest == "" || r[0].Digest != r[1].Digest {
			t.Fatalf("%+v %+v", r[0], r[1])
		}
	})

	// State the snapshot cannot capture: the target is unverifiable and an
	// approval of it can never be redeemed.
	unverifiable := map[string]scenario{
		"file entry": {Form: form(nil, [][]any{{"att", map[string]any{"file": true}}}, send), T: 0},
		"password field sharing its name with a hidden field": {
			Form: form(nil, ent("to", "alice", "to", "secret"), fel("INPUT", "hidden", "to", "alice"), fel("INPUT", "password", "to", "secret"), send), T: 2},
		"one-time-code field sharing its name with a text field": {
			Form: form(nil, ent("code", "1", "code", "2"), fel("INPUT", "text", "code", "1"),
				func() fakeEl {
					e := fel("INPUT", "text", "code", "2")
					e.Attrs = map[string]string{"autocomplete": "one-time-code"}
					return e
				}(), send), T: 2},
		"unchecked one-time-code checkbox sharing its name with a hidden field": {
			Form: form(nil, ent("to", "alice"),
				func() fakeEl {
					e := fakeEl{TagName: "INPUT", Type: "checkbox", Name: "to", Value: sp("on")}
					e.Attrs = map[string]string{"autocomplete": "one-time-code"}
					return e
				}(), fel("INPUT", "hidden", "to", "alice"), send), T: 2},
		"disabled password sharing its name with a text field": {
			Form: form(nil, ent("to", "alice"),
				func() fakeEl { e := fel("INPUT", "password", "to", "x"); e.Disabled = true; return e }(), fel("INPUT", "text", "to", "alice"), send), T: 2},
		"current-password autocomplete sharing a name": {
			Form: form(nil, ent("to", "alice", "to", "s"),
				func() fakeEl {
					e := fel("INPUT", "text", "to", "s")
					e.Attrs = map[string]string{"autocomplete": "current-password"}
					return e
				}(), fel("INPUT", "hidden", "to", "alice"), send), T: 2},
		"disabled password plus an entry a formdata handler added under its name": {
			Form: form(nil, ent("to", "mallory"),
				func() fakeEl { e := fel("INPUT", "password", "to", "x"); e.Disabled = true; return e }(), send), T: 1},
		"unchecked one-time-code checkbox plus an entry a formdata handler added": {
			Form: form(nil, ent("to", "mallory"),
				func() fakeEl {
					e := fakeEl{TagName: "INPUT", Type: "checkbox", Name: "to", Value: sp("on")}
					e.Attrs = map[string]string{"autocomplete": "one-time-code"}
					return e
				}(), send), T: 1},
		"entry under a password name whose value does not match the control": {
			Form: form(nil, ent("pw", "other"), fel("INPUT", "password", "pw", "secret"), send), T: 1},
		"submitter named like a password field": {
			Form: form(nil, ent("pw", "x", "pw", "go"), fel("INPUT", "password", "pw", "x"), button("pw", "go", nil)), T: 1},
		"FormData constructor throws": {Form: func() *fakeForm { f := form(nil, nil, send); f.Throws = true; return f }(), T: 0},
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
