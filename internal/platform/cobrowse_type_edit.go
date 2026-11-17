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
	ErrTypeEmptyText     = errors.New("type with empty text requires clear")
	ErrSelectorTooLong   = fmt.Errorf("type selector is longer than %d bytes", MaxCobrowseSelectorLen)
	ErrModifiersOutRange = errors.New("modifiers must be an integer from 0 to 15")
)

// TypeOptions selects the targeted forms of type. The zero value inserts at the
// focused element's cursor, as type always has.
type TypeOptions struct {
	// Selector, when set, is resolved in the pinned page to one editable field
	// (input, textarea or contenteditable) that is focused before the insert.
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
// editable field (the contenteditable host for a descendant), or null.
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
  if(el.isContentEditable){var h=el;while(h.parentElement&&h.parentElement.isContentEditable)h=h.parentElement;return {el:h,kind:"contenteditable"};}
  return null;
}
`

// editFocusScript resolves selector (document.querySelector, as click does),
// checks it is editable, and focuses it. Nothing changes unless it is editable.
func editFocusScript(selector string) string {
	return `/*edit:focus*/(function(){` + editScriptLib + `
  var el=document.querySelector(` + jsStringLiteral(selector) + `);
  if(!el)return {error:"no element matches the selector"};
  var t=editTarget(el);
  if(!t)return {error:"the selector does not match an editable field"};
  t.el.focus();
  var a=deepActive();
  if(a!==t.el&&!(t.kind==="contenteditable"&&t.el.contains(a)))return {error:"the field did not take focus"};
  return {ok:true,tag:String(t.el.tagName||"").toLowerCase()};
})()`
}

// editSelectScript selects the whole content of the focused editable field.
const editSelectScript = `/*edit:select*/(function(){` + editScriptLib + `
  var t=editTarget(deepActive());
  if(!t)return {error:"the focused element is not an editable field"};
  if(t.kind==="contenteditable"){
    var s=window.getSelection(),r=document.createRange();
    r.selectNodeContents(t.el);s.removeAllRanges();s.addRange(r);
  }else{t.el.select();}
  return {ok:true};
})()`

// editEmptyScript reports whether the focused editable field holds no content.
// It returns a boolean only, never the content.
const editEmptyScript = `/*edit:empty*/(function(){` + editScriptLib + `
  var t=editTarget(deepActive());
  if(!t)return {error:"the focused element is not an editable field"};
  var v=t.kind==="contenteditable"?t.el.textContent:t.el.value;
  return {empty:String(v||"").length===0};
})()`

// editStep evaluates one edit script. A refusal the script reports in-band
// changed nothing, so the effects' Dispatched is restored; a transport error
// leaves it set, since the script may have run.
func (r *actionRun) editStep(script string) (map[string]any, error) {
	prev := r.eff.Dispatched
	r.eff.Dispatched = true
	val, err := evaluateOn(r.conn, script, false)
	if err != nil {
		return nil, err
	}
	obj, _ := val.(map[string]any)
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

// TypeWith is Type with a target and replace. With no selector and no clear it
// is exactly Type. Otherwise the whole of it is one action on one pinned page:
// resolve and focus the selector, select the content (clear), then insert (or
// delete, for empty text), with the live driver rechecked between the steps.
// Refusals (empty text without clear, an over-long selector, a selector that
// matches nothing or is not editable) send no text. The text is never reported.
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
		if o.Selector != "" {
			if err := between(); err != nil {
				return err
			}
			obj, err := r.editStep(editFocusScript(o.Selector))
			if err != nil {
				return err
			}
			r.eff.Focused = true
			r.eff.FocusTag, _ = obj["tag"].(string)
		}
		if o.Clear {
			if err := between(); err != nil {
				return err
			}
			if _, err := r.editStep(editSelectScript); err != nil {
				return err
			}
		}
		if text != "" {
			if err := between(); err != nil {
				return err
			}
			r.eff.Dispatched = true
			r.eff.TypedLength, r.eff.TypedUnconfirmed = typedLength(text), true
			if _, err := r.conn.Call("Input.insertText", map[string]any{"text": text}); err != nil {
				return err
			}
			r.eff.TypedUnconfirmed = false
			if o.Clear {
				r.eff.Cleared = true
			}
			return nil
		}
		// Empty clear: delete the selection with a real Backspace, then check
		// the field really is empty.
		if err := between(); err != nil {
			return err
		}
		if err := r.pressKey("Backspace", 0); err != nil {
			return err
		}
		if err := r.midActionFence(); err != nil {
			return err
		}
		obj, err := r.editStep(editEmptyScript)
		if err != nil {
			return err
		}
		if empty, _ := obj["empty"].(bool); !empty {
			return errors.New("the field was not emptied")
		}
		r.eff.Cleared = true
		return nil
	})
}

// KeyWith is Key with the CDP modifier bitmask (Alt=1, Ctrl=2, Meta=4,
// Shift=8). Anything outside 0..15 is refused before any dispatch.
func (m *CobrowseSessionManager) KeyWith(id, key string, modifiers int) (CobrowseSessionStatus, error) {
	if modifiers < 0 || modifiers > modMax {
		return CobrowseSessionStatus{}, ErrModifiersOutRange
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
