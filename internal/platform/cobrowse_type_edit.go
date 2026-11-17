// Targeted replace for type, and modifiers for key (citadel-cli#1730, the node
// half of aceteam#13782). Both stay one runAction on one pinned page: resolving
// and focusing the target, selecting its content, and the insert or delete are
// steps of one action, with midActionFence between them and no retry.
package platform

import (
	"errors"
	"fmt"
	"strings"
)

// InputEditV1 is the status capability value for this contract. A platform must
// not send selector, clear or modifiers to a node whose status lacks it.
const InputEditV1 = "type_clear_key_modifiers_v1"

// MaxCobrowseSelectorLen bounds a type selector (bytes).
const MaxCobrowseSelectorLen = 1024

// Modifier bits (the CDP bitmask).
const (
	ModAlt   = 1
	ModCtrl  = 2
	ModMeta  = 4
	ModShift = 8
	modMax   = ModAlt | ModCtrl | ModMeta | ModShift
)

// Validation refusals. Each is returned before anything is dispatched.
var (
	ErrTypeEmptyText      = errors.New("type with empty text requires clear")
	ErrSelectorTooLong    = fmt.Errorf("type selector is longer than %d bytes", MaxCobrowseSelectorLen)
	ErrModifiersOutRange  = errors.New("modifiers must be an integer from 0 to 15")
	ErrKeyChordNotAllowed = errors.New("this key chord is not allowed: clipboard and editing chords are refused")
)

// CheckKeyChord is the allowlist for a key with modifiers; modifiers 0 is
// always allowed. It is the one decision both the job parser and KeyWith use,
// before any dispatch. Allowed:
//   - Shift alone with any key, except Delete (Shift+Delete is a clipboard edit);
//   - Ctrl or Meta alone with a, z or y;
//   - Ctrl or Alt alone with an arrow, Home, End, Backspace or Delete.
//
// Everything else is refused, in particular clipboard chords, and Alt with a
// letter (a page accesskey).
func CheckKeyChord(key string, mod int) error {
	if mod < 0 || mod > modMax {
		return ErrModifiersOutRange
	}
	if mod == 0 {
		return nil
	}
	switch mod {
	case ModShift:
		if key != "Delete" {
			return nil
		}
	case ModCtrl, ModMeta:
		switch strings.ToLower(key) {
		case "a", "z", "y":
			if len(key) == 1 {
				return nil
			}
		}
		if mod == ModCtrl && navOrDeleteKey(key) {
			return nil
		}
	case ModAlt:
		if navOrDeleteKey(key) {
			return nil
		}
	}
	return ErrKeyChordNotAllowed
}

func navOrDeleteKey(key string) bool {
	switch key {
	case "ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight", "Home", "End", "Backspace", "Delete":
		return true
	}
	return false
}

// TypeOptions selects the targeted forms of type. The zero value inserts at the
// focused element's cursor, as type always has.
type TypeOptions struct {
	// Selector, when set, is resolved in the pinned page to one editable field
	// (input, textarea or a contenteditable host) that is focused before the
	// insert.
	Selector string
	// Clear selects the target's whole content so the insert replaces it; with
	// empty text the content is deleted.
	Clear bool
}

func (o TypeOptions) validate(text string) error {
	if text == "" && !o.Clear {
		return ErrTypeEmptyText
	}
	if len(o.Selector) > MaxCobrowseSelectorLen {
		return ErrSelectorTooLong
	}
	return nil
}

// In-page helpers shared by the edit scripts. editTarget maps an element to an
// editable field or null. A contenteditable qualifies only as its own editing
// host, never a descendant of one, and never under designMode, so a clear can
// not widen beyond the element that was matched.
const editScriptLib = `
function deepActive(){var a=document.activeElement;while(a&&a.shadowRoot&&a.shadowRoot.activeElement)a=a.shadowRoot.activeElement;return a;}
function editTarget(el){
  if(!el||el.nodeType!==1)return null;
  if(el instanceof HTMLTextAreaElement)return (el.disabled||el.readOnly)?null:{el:el,kind:"textarea"};
  if(el instanceof HTMLInputElement){
    var t=String(el.type||"text").toLowerCase();
    if(["text","search","url","tel","email","password","number"].indexOf(t)<0||el.disabled||el.readOnly)return null;
    return {el:el,kind:"input"};
  }
  if(el.isContentEditable){
    if(document.designMode==="on")return null;
    if(el.parentElement&&el.parentElement.isContentEditable)return null;
    return {el:el,kind:"contenteditable"};
  }
  return null;
}
function isActive(t){var a=deepActive();return a===t.el||(t.kind==="contenteditable"&&t.el.contains(a));}
`

// editFocusScript resolves selector (document.querySelector, as click does),
// checks it is editable, and focuses it. It returns the element itself (so the
// action can bind to it), or a string: a refusal that changed nothing, or one
// prefixed "!" when focus() already ran.
func editFocusScript(selector string) string {
	return `/*edit:focus*/(function(){` + editScriptLib + `
  var el=document.querySelector(` + jsStringLiteral(selector) + `);
  if(!el)return "no element matches the selector";
  var t=editTarget(el);
  if(!t)return "the selector does not match an editable field";
  t.el.focus();
  if(!isActive(t))return "!the field did not take focus";
  return t.el;
})()`
}

// editActiveScript resolves the focused element as the target, without moving
// focus, returning it or a refusal string.
const editActiveScript = `/*edit:target*/(function(){` + editScriptLib + `
  var t=editTarget(deepActive());
  if(!t)return "the focused element is not an editable field";
  return t.el;
})()`

// The bound steps run on the resolved element as this.
const (
	editSelectFn = `/*edit:select*/function(){` + editScriptLib + `
  var t=editTarget(this);
  if(!t||t.el!==this)return {error:"the target is no longer an editable field"};
  if(!isActive(t))return {error:"the field lost focus"};
  var whole=false;
  if(t.kind==="contenteditable"){
    var s=window.getSelection(),r=document.createRange();
    r.selectNodeContents(t.el);s.removeAllRanges();s.addRange(r);
    var sr=s.rangeCount?s.getRangeAt(0):null;
    whole=!!sr&&sr.compareBoundaryPoints(Range.START_TO_START,r)===0&&sr.compareBoundaryPoints(Range.END_TO_END,r)===0;
  }else{
    t.el.select();
    var ss=null,se=null;
    try{ss=t.el.selectionStart;se=t.el.selectionEnd;}catch(e){}
    if(ss===null&&se===null){whole=["email","number"].indexOf(String(t.el.type).toLowerCase())>=0;}
    else{whole=ss===0&&se===t.el.value.length;}
  }
  if(!whole)return {error:"the selection does not span the whole field"};
  return {ok:true};
}`
	editVerifyFn = `/*edit:verify*/function(){` + editScriptLib + `
  var t=editTarget(this);
  return {ok:!!t&&t.el===this&&isActive(t)};
}`
	// editEmptyFn returns a boolean only, never the content.
	editEmptyFn = `/*edit:empty*/function(){` + editScriptLib + `
  var t=editTarget(this);
  if(!t||t.el!==this)return {error:"the target is no longer an editable field"};
  var v=t.kind==="contenteditable"?t.el.textContent:t.el.value;
  return {empty:String(v||"").length===0};
}`
)

// editBinding is the element an edit resolved, and the document it lived in.
type editBinding struct {
	objectID string
	loaderID string
}

// tagFromClass derives a tag name from a RemoteObject class name
// (HTMLInputElement -> input); the effect probe refines it.
func tagFromClass(class string) string {
	c := strings.TrimSuffix(strings.TrimPrefix(class, "HTML"), "Element")
	return strings.ToLower(c)
}

// resolveTarget evaluates a resolving script and binds to the element it
// returns. A string result is a refusal: it changed nothing unless prefixed "!"
// (focus ran). markDispatched is set before the call for the focusing script,
// which may change the page.
func (r *actionRun) resolveTarget(script string, markDispatched bool) (editBinding, string, error) {
	prev := r.eff.Dispatched
	if markDispatched {
		r.eff.Dispatched = true
	}
	res, err := r.conn.Call("Runtime.evaluate", map[string]any{
		"expression": script, "returnByValue": false, "awaitPromise": false,
	})
	if err != nil {
		return editBinding{}, "", err
	}
	if exc, ok := res["exceptionDetails"]; ok && exc != nil {
		r.eff.Dispatched = prev
		return editBinding{}, "", errors.New("the edit script failed in the page")
	}
	remote, _ := res["result"].(map[string]any)
	if typ, _ := remote["type"].(string); typ == "string" {
		msg, _ := remote["value"].(string)
		if strings.HasPrefix(msg, "!") {
			msg = msg[1:] // focus() ran: stays dispatched
		} else {
			r.eff.Dispatched = prev
		}
		return editBinding{}, "", errors.New(msg)
	}
	id, _ := remote["objectId"].(string)
	if id == "" {
		r.eff.Dispatched = prev
		return editBinding{}, "", errors.New("the page returned no element for the edit")
	}
	class, _ := remote["className"].(string)
	loader, err := r.loaderID()
	if err != nil {
		return editBinding{}, "", err
	}
	return editBinding{objectID: id, loaderID: loader}, tagFromClass(class), nil
}

// loaderID is the main frame's current loader id; it changes on navigation.
func (r *actionRun) loaderID() (string, error) {
	res, err := r.conn.Call("Page.getFrameTree", nil)
	if err != nil {
		return "", err
	}
	tree, _ := res["frameTree"].(map[string]any)
	frame, _ := tree["frame"].(map[string]any)
	id, _ := frame["loaderId"].(string)
	return id, nil
}

// boundStep runs fn on the bound element. A CDP-level failure (the object is
// gone, the page navigated) is ErrTargetChanged when mapGone is set; it is only
// set before anything destructive was sent, since a refusal hides effects.
func (r *actionRun) boundStep(b editBinding, fn string, mapGone bool) (map[string]any, error) {
	prev := r.eff.Dispatched
	r.eff.Dispatched = true
	res, err := r.conn.Call("Runtime.callFunctionOn", map[string]any{
		"objectId": b.objectID, "functionDeclaration": fn, "returnByValue": true,
	})
	if err != nil {
		if mapGone && strings.HasPrefix(err.Error(), "CDP error") {
			r.eff.Dispatched = prev
			return nil, ErrTargetChanged
		}
		return nil, err
	}
	if exc, ok := res["exceptionDetails"]; ok && exc != nil {
		r.eff.Dispatched = prev
		return nil, errors.New("the edit script failed in the page")
	}
	remote, _ := res["result"].(map[string]any)
	obj, _ := remote["value"].(map[string]any)
	if obj == nil {
		r.eff.Dispatched = prev
		return nil, errors.New("the page returned no result for the edit")
	}
	if msg, _ := obj["error"].(string); msg != "" {
		r.eff.Dispatched = prev
		return nil, errors.New(msg)
	}
	return obj, nil
}

// verifyBound re-checks, immediately before a destructive event, that the
// action's element is still the focused editable target in the same document.
func (r *actionRun) verifyBound(b editBinding) error {
	loader, err := r.loaderID()
	if err != nil {
		return err
	}
	if loader != b.loaderID {
		return ErrTargetChanged
	}
	obj, err := r.boundStep(b, editVerifyFn, true)
	if err != nil {
		return err
	}
	if ok, _ := obj["ok"].(bool); !ok {
		return ErrTargetChanged
	}
	return nil
}

// TypeWith is Type with a target and replace. With no selector and no clear it
// is exactly Type. Otherwise the whole of it is one action on one pinned page:
// resolve and focus the selector (or take the focused field), select the whole
// content (clear), then insert (or delete, for empty text), with the live
// driver rechecked between the steps and the resolved element and document
// re-verified right before the destructive event. Refusals (empty text without
// clear, an over-long selector, a selector that matches nothing or is not
// editable) send no text. The text is never reported.
func (m *CobrowseSessionManager) TypeWith(id, text string, o TypeOptions) (CobrowseSessionStatus, error) {
	if err := o.validate(text); err != nil {
		return CobrowseSessionStatus{}, err
	}
	s, err := m.sessionForAction(id)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	b, cancel := newActionBudget()
	defer cancel()
	p, err := s.pinActionCtx(b.op)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	return s.runAction(b, p, "type", func(r *actionRun) error {
		if err := r.fence(); err != nil {
			return err
		}
		ran := false
		between := func() error {
			if !ran {
				ran = true
				return nil
			}
			return r.midActionFence()
		}
		var bind *editBinding
		if o.Selector != "" || o.Clear {
			if err := between(); err != nil {
				return err
			}
			var nb editBinding
			var tag string
			var err error
			if o.Selector != "" {
				nb, tag, err = r.resolveTarget(editFocusScript(o.Selector), true)
				if err == nil {
					r.eff.Focused = true
					r.eff.FocusTag = tag
				}
			} else {
				nb, _, err = r.resolveTarget(editActiveScript, false)
			}
			if err != nil {
				return err
			}
			bind = &nb
		}
		if o.Clear {
			if err := between(); err != nil {
				return err
			}
			if _, err := r.boundStep(*bind, editSelectFn, true); err != nil {
				return err
			}
		}
		// Before anything destructive: still the same element, same document,
		// and the live driver, in that order, with the fence last.
		if err := between(); err != nil {
			return err
		}
		if bind != nil {
			if err := r.verifyBound(*bind); err != nil {
				return err
			}
			if err := r.midActionFence(); err != nil {
				return err
			}
		}
		if text != "" {
			r.eff.Dispatched = true
			r.eff.TypedLength, r.eff.TypedUnconfirmed = typedLength(text), true
			if o.Clear {
				r.eff.ClearedUnconfirmed = true
			}
			if _, err := r.conn.Call("Input.insertText", map[string]any{"text": text}); err != nil {
				return err
			}
			r.eff.TypedUnconfirmed = false
			if o.Clear {
				r.eff.Cleared, r.eff.ClearedUnconfirmed = true, false
			}
			return nil
		}
		// Empty clear: delete the selection with a real Backspace, then check
		// the field really is empty.
		r.eff.ClearedUnconfirmed = true
		if err := r.pressKey("Backspace", 0); err != nil {
			return err
		}
		if err := r.midActionFence(); err != nil {
			return err
		}
		obj, err := r.boundStep(*bind, editEmptyFn, false)
		if err != nil {
			return err
		}
		empty, _ := obj["empty"].(bool)
		r.eff.ClearedUnconfirmed = false
		if !empty {
			return errors.New("the field was not emptied")
		}
		r.eff.Cleared = true
		return nil
	})
}

// KeyWith is Key with the CDP modifier bitmask (Alt=1, Ctrl=2, Meta=4,
// Shift=8), limited to the CheckKeyChord allowlist. A refused chord sends
// nothing.
func (m *CobrowseSessionManager) KeyWith(id, key string, modifiers int) (CobrowseSessionStatus, error) {
	if err := CheckKeyChord(key, modifiers); err != nil {
		return CobrowseSessionStatus{}, err
	}
	s, err := m.sessionForAction(id)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	b, cancel := newActionBudget()
	defer cancel()
	p, err := s.pinActionCtx(b.op)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	if _, _, _, ok := browserKey(key); !ok {
		return CobrowseSessionStatus{}, fmt.Errorf("unsupported key")
	}
	return s.runAction(b, p, "key", func(r *actionRun) error {
		if err := r.fence(); err != nil {
			return err
		}
		return r.pressKey(key, modifiers)
	})
}

// keyEventParams builds one key event. With Ctrl, Meta or Alt held a printable
// key produces no text (so Ctrl+A never types "a") and a raw key down is sent;
// Ctrl/Meta+A also carries the selectAll editing command, which headless
// Chrome needs when the bare chord does not act. With modifiers 0 the event is
// exactly the one Key always sent.
func keyEventParams(kind, key string, mod int) map[string]any {
	code, vk, produced, _ := browserKey(key)
	down := kind == "down"
	chord := mod&(ModAlt|ModCtrl|ModMeta) != 0
	params := map[string]any{"key": key, "code": code, "windowsVirtualKeyCode": vk}
	if !down {
		params["type"] = "keyUp"
	} else if produced != "" && !chord {
		params["type"] = "keyDown"
		if mod&ModShift != 0 {
			produced = strings.ToUpper(produced)
		}
		params["text"] = produced
	} else {
		params["type"] = "rawKeyDown"
	}
	if mod != 0 {
		params["modifiers"] = mod
	}
	if down && mod&(ModCtrl|ModMeta) != 0 && strings.EqualFold(key, "a") {
		params["commands"] = []string{"selectAll"}
	}
	return params
}

// pressKey sends key-down then key-up with the driver rechecked between them,
// marking the action dispatched.
func (r *actionRun) pressKey(key string, mod int) error {
	r.eff.Dispatched = true
	if _, err := r.conn.Call("Input.dispatchKeyEvent", keyEventParams("down", key, mod)); err != nil {
		return err
	}
	// A human can take control while the key-down is awaited.
	if err := r.midActionFence(); err != nil {
		return err
	}
	_, err := r.conn.Call("Input.dispatchKeyEvent", keyEventParams("up", key, mod))
	return err
}
