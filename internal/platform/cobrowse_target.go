package platform

import (
	"encoding/json"
	"fmt"
)

// TargetProbe names the action whose target InspectTarget describes.
type TargetProbe struct {
	// Kind is "click", "key", "select" or "type". Key and type describe the
	// focused element; click and select describe the selector or point.
	Kind     string
	Selector string
	X, Y     *float64
}

// TargetRect is the target's box in CSS pixels, with the viewport size.
type TargetRect struct {
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
	W  float64 `json:"w"`
	H  float64 `json:"h"`
	VW float64 `json:"vw"`
	VH float64 `json:"vh"`
}

// TargetInfo describes what an action is about to act on. It is read inside
// the page and decoded without interpretation; the local-mode commit
// classifier (localcobrowse.Classify) decides what it means. Content holds the
// text of the form or composer the action would send; it exists only so the
// action fingerprint can bind to it. It never goes into a log, a result or an
// approval card, and password and one-time-code fields are excluded.
type TargetInfo struct {
	// Resolved is false when the target could not be identified (nothing at
	// the point, a canvas, a nested frame). Callers must treat that as a
	// commit: an unknown target is never assumed harmless.
	Resolved      bool       `json:"resolved"`
	Reason        string     `json:"reason,omitempty"`
	PageURL       string     `json:"page_url,omitempty"`
	Tag           string     `json:"tag,omitempty"`
	Role          string     `json:"role,omitempty"`
	InputType     string     `json:"input_type,omitempty"`
	Name          string     `json:"name,omitempty"`
	Href          string     `json:"href,omitempty"`
	Actionable    bool       `json:"actionable,omitempty"`
	Entry         bool       `json:"entry,omitempty"`
	Composer      bool       `json:"composer,omitempty"`
	Search        bool       `json:"search,omitempty"`
	AutoSubmit    bool       `json:"auto_submit,omitempty"`
	InForm        bool       `json:"in_form,omitempty"`
	FormMethod    string     `json:"form_method,omitempty"`
	FormAction    string     `json:"form_action,omitempty"`
	FormHasSubmit bool       `json:"form_has_submit,omitempty"`
	Rect          TargetRect `json:"rect"`
	Content       string     `json:"content,omitempty"`
}

// maxTargetContent bounds Content so one probe stays small.
const maxTargetContent = 16000

const targetProbeScript = `(function(kind, selector, px, py){
	var MAXC = 16000;
	function deepActive(){ var a = document.activeElement; while (a && a.shadowRoot && a.shadowRoot.activeElement) a = a.shadowRoot.activeElement; return a; }
	function deepPoint(x, y){ var e = document.elementFromPoint(x, y); while (e && e.shadowRoot) { var i = e.shadowRoot.elementFromPoint(x, y); if (!i || i === e) break; e = i; } return e; }
	function txt(e){ return (e.innerText || e.textContent || "").replace(/\s+/g, " ").trim(); }
	function labelled(e){
		var ids = e.getAttribute("aria-labelledby");
		if (!ids) return "";
		return ids.split(/\s+/).map(function(i){ var n = document.getElementById(i); return n ? txt(n) : ""; }).join(" ").trim();
	}
	function nameOf(e){
		var t = (e.type || "").toLowerCase();
		var n = e.getAttribute("aria-label") || labelled(e);
		if (!n && e.tagName === "INPUT" && (t === "submit" || t === "button" || t === "reset" || t === "image")) n = e.value || e.alt || "";
		if (!n && !/^(INPUT|TEXTAREA|SELECT)$/.test(e.tagName)) n = txt(e);
		if (!n && e.labels && e.labels.length) n = txt(e.labels[0]);
		if (!n) n = e.title || e.alt || e.placeholder || e.getAttribute("name") || "";
		if (!n) { var img = e.querySelector && e.querySelector("img[alt],svg title"); if (img) n = img.alt || txt(img); }
		return String(n).replace(/\s+/g, " ").trim().slice(0, 200);
	}
	function abs(u){ try { return new URL(u, document.baseURI).href; } catch (e) { return ""; } }
	var out = {resolved: false, page_url: location.href};
	var el = null;
	if (kind === "key" || kind === "type") {
		el = deepActive();
		if (!el || el === document.body || el === document.documentElement) {
			out.resolved = true; out.tag = "body"; return out;
		}
	} else if (selector) {
		el = document.querySelector(selector);
		if (!el) { out.reason = "no element matches"; return out; }
		if (kind === "click") {
			// Mirror what the real click does: scroll to the element, then click its
			// centre, which lands on whatever is there (a container's Send button).
			try { el.scrollIntoView({block: "center", inline: "center", behavior: "instant"}); } catch (e) {}
			var br = el.getBoundingClientRect();
			var cx = br.x + br.width / 2, cy = br.y + br.height / 2;
			if (br.width > 0 && br.height > 0 && cx >= 0 && cx < window.innerWidth && cy >= 0 && cy < window.innerHeight) {
				var hit = deepPoint(cx, cy);
				if (hit && (hit === el || el.contains(hit))) el = hit;
			}
		}
	} else if (typeof px === "number" && typeof py === "number") {
		el = deepPoint(px, py);
		if (!el) { out.reason = "nothing at that point"; return out; }
	} else { out.reason = "no target"; return out; }
	var tag = el.tagName ? el.tagName.toLowerCase() : "";
	if (/^(iframe|frame|object|embed|canvas)$/.test(tag)) { out.reason = "target is inside " + tag; return out; }
	var actSel = "button,a[href],input[type=submit],input[type=image],input[type=button],input[type=reset],[role=button],[role=link],[role=menuitem],[role=tab],summary,[onclick]";
	var act = (kind === "click" && el.closest) ? el.closest(actSel) : (el.matches && el.matches(actSel) ? el : null);
	var t = act || el;
	tag = t.tagName.toLowerCase();
	var cs = null; try { cs = getComputedStyle(t); } catch (e) {}
	var role = (t.getAttribute("role") || "").toLowerCase();
	var type = (t.type || t.getAttribute("type") || "").toLowerCase();
	var isEntry = tag === "textarea" || t.isContentEditable || role === "textbox" || role === "searchbox" || role === "combobox" ||
		(tag === "input" && !/^(checkbox|radio|button|submit|reset|image|file|hidden|range|color)$/.test(type));
	out.tag = tag; out.role = role; out.input_type = type;
	out.name = nameOf(t);
	out.actionable = !!act || (!isEntry && ((cs && cs.cursor === "pointer") || t.hasAttribute("tabindex")));
	out.entry = isEntry;
	out.composer = tag === "textarea" || !!t.isContentEditable || role === "textbox";
	if (tag === "a" && t.href) out.href = abs(t.getAttribute("href") || t.href);
	var form = t.form || (t.closest ? t.closest("form") : null);
	var attrSearch = /\bsearch\b/i.test((t.getAttribute("aria-label") || "") + " " + (t.id || "") + " " + (t.getAttribute("name") || ""));
	out.search = type === "search" || role === "searchbox" || attrSearch || !!(t.closest && t.closest("[role=search]"));
	if (form) {
		out.in_form = true;
		var fa = t.getAttribute && t.getAttribute("formaction");
		var fm = t.getAttribute && t.getAttribute("formmethod");
		out.form_action = abs(fa || (form.getAttribute("action") || location.href));
		out.form_method = String(fm || form.getAttribute("method") || "get").toLowerCase();
		out.form_has_submit = !!form.querySelector("button:not([type]),button[type=submit],input[type=submit],input[type=image]");
		if (form.getAttribute("role") === "search") out.search = true;
		var controls = 0;
		for (var i = 0; i < form.elements.length; i++) { var c = form.elements[i]; if ((c.type || "") !== "hidden") controls++; }
		if (tag === "select") out.auto_submit = t.hasAttribute("onchange") || (controls === 1 && !out.form_has_submit);
	} else if (tag === "select") { out.auto_submit = t.hasAttribute("onchange"); }
	var r = t.getBoundingClientRect();
	out.rect = {x: r.x, y: r.y, w: r.width, h: r.height, vw: window.innerWidth, vh: window.innerHeight};
	var parts = [];
	function field(c){
		var ty = (c.type || "").toLowerCase();
		// Hidden fields carry recipients and amounts, so they are bound too.
		if (/^(password|file|submit|button|reset|image)$/.test(ty)) return;
		var ac = (c.getAttribute && c.getAttribute("autocomplete")) || "";
		if (/one-time-code|password/i.test(ac)) return;
		var v = c.isContentEditable ? txt(c) : (c.value !== undefined ? c.value : "");
		if (ty === "checkbox" || ty === "radio") v = (c.checked ? "on:" : "off:") + v;
		parts.push((c.name || c.id || "") + "=" + v);
	}
	if (form) { for (var j = 0; j < form.elements.length; j++) field(form.elements[j]); }
	var focus = deepActive();
	if (focus && focus !== t && (focus.tagName === "TEXTAREA" || focus.isContentEditable || focus.tagName === "INPUT")) field(focus);
	if (isEntry) field(t);
	out.content = parts.join("\n").slice(0, MAXC);
	out.resolved = true;
	return out;
})`

// InspectTarget describes the element the given action would act on, without
// acting. It is a read of the live page, refused while a human drives like any
// other scripted CDP action.
func (m *CobrowseSessionManager) InspectTarget(id string, p TargetProbe) (TargetInfo, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return TargetInfo{}, err
	}
	port, err := s.requireDrivablePort()
	if err != nil {
		return TargetInfo{}, err
	}
	var px, py any = nil, nil
	if p.X != nil && p.Y != nil {
		px, py = *p.X, *p.Y
	}
	pxj, _ := json.Marshal(px)
	pyj, _ := json.Marshal(py)
	expr := fmt.Sprintf("%s(%s,%s,%s,%s)", targetProbeScript,
		jsStringLiteral(p.Kind), jsStringLiteral(p.Selector), pxj, pyj)
	val, err := runtimeEvaluate(port, expr)
	if err != nil {
		return TargetInfo{}, err
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return TargetInfo{}, err
	}
	var info TargetInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return TargetInfo{}, err
	}
	if len(info.Content) > maxTargetContent {
		info.Content = info.Content[:maxTargetContent]
	}
	return info, nil
}
