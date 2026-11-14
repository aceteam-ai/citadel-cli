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
// classifier (localcobrowse.Classify) decides what it means. ContentHash binds
// the text of the form or composer the action would send, so the action
// fingerprint changes if any of it does; the text itself stays in the page and
// password and one-time-code fields are excluded.
type TargetInfo struct {
	// Resolved is false when the target could not be identified (nothing at
	// the point, a canvas, a nested frame). Callers must treat that as a
	// commit: an unknown target is never assumed harmless.
	Resolved   bool   `json:"resolved"`
	Reason     string `json:"reason,omitempty"`
	PageURL    string `json:"page_url,omitempty"`
	Tag        string `json:"tag,omitempty"`
	Role       string `json:"role,omitempty"`
	InputType  string `json:"input_type,omitempty"`
	Name       string `json:"name,omitempty"`
	Href       string `json:"href,omitempty"`
	Actionable bool   `json:"actionable,omitempty"`
	Entry      bool   `json:"entry,omitempty"`
	Composer   bool   `json:"composer,omitempty"`
	Search     bool   `json:"search,omitempty"`
	AutoSubmit bool   `json:"auto_submit,omitempty"`
	InForm     bool   `json:"in_form,omitempty"`
	FormMethod string `json:"form_method,omitempty"`
	FormAction string `json:"form_action,omitempty"`
	// FormEnctype and FormTarget are the effective submission encoding and
	// target, after any submitter override (formenctype, formtarget).
	FormEnctype string `json:"form_enctype,omitempty"`
	FormTarget  string `json:"form_target,omitempty"`
	// FormOverrides is the presence and raw value of every submission-affecting
	// attribute on the form and its submitter (present-but-empty differs from
	// absent). It is only hashed.
	FormOverrides string     `json:"form_overrides,omitempty"`
	FormHasSubmit bool       `json:"form_has_submit,omitempty"`
	Rect          TargetRect `json:"rect"`
	// ContentHash is the SHA-256 (hex) of the complete form and composer
	// content, computed in the page; the content itself never leaves it.
	ContentHash string `json:"content_hash,omitempty"`
	// Oversized is set when the content is too large to hash in full. Such a
	// target is a commit whose approval can never be redeemed.
	Oversized bool `json:"oversized,omitempty"`
}

// FormDigestScript is the in-page encoding and hash of a form's fields, shared
// with the tests that run it under Node. formDigest takes an ordered array of
// [name, id, type, value, checked] tuples and hashes its JSON encoding, which
// is unambiguous: no delimiter inside a name or value can move a field boundary
// (a textarea msg="hello\nrecipient=bob" differs from msg="hello" followed by a
// hidden recipient="bob").
const FormDigestScript = `
	// SHA-256 over the UTF-8 bytes of a string, synchronous (SubtleCrypto is
	// async and only on secure contexts). Constants are derived from primes.
	function sha256hex(str){
		var K = [], H = [], n = 2, i, j;
		function frac(x){ return ((x - Math.floor(x)) * 4294967296) | 0; }
		for (var c = 0; c < 64; n++) {
			var prime = true; for (j = 2; j * j <= n; j++) if (n % j === 0) { prime = false; break; }
			if (!prime) continue;
			if (c < 8) H[c] = frac(Math.pow(n, 1 / 2));
			K[c++] = frac(Math.pow(n, 1 / 3));
		}
		var data = new TextEncoder().encode(str), l = data.length;
		var total = (((l + 9) + 63) >> 6) << 6, buf = new Uint8Array(total);
		buf.set(data); buf[l] = 0x80;
		var dv = new DataView(buf.buffer);
		dv.setUint32(total - 8, Math.floor(l / 536870912)); dv.setUint32(total - 4, (l << 3) >>> 0);
		var w = new Array(64);
		function rr(x, k){ return (x >>> k) | (x << (32 - k)); }
		for (var off = 0; off < total; off += 64) {
			for (i = 0; i < 16; i++) w[i] = dv.getUint32(off + i * 4);
			for (i = 16; i < 64; i++) {
				var s0 = rr(w[i-15], 7) ^ rr(w[i-15], 18) ^ (w[i-15] >>> 3);
				var s1 = rr(w[i-2], 17) ^ rr(w[i-2], 19) ^ (w[i-2] >>> 10);
				w[i] = (w[i-16] + s0 + w[i-7] + s1) | 0;
			}
			var a = H[0], b = H[1], cc = H[2], d = H[3], e = H[4], f = H[5], g = H[6], h = H[7];
			for (i = 0; i < 64; i++) {
				var S1 = rr(e, 6) ^ rr(e, 11) ^ rr(e, 25), ch = (e & f) ^ (~e & g);
				var t1 = (h + S1 + ch + K[i] + w[i]) | 0;
				var S0 = rr(a, 2) ^ rr(a, 13) ^ rr(a, 22), mj = (a & b) ^ (a & cc) ^ (b & cc);
				var t2 = (S0 + mj) | 0;
				h = g; g = f; f = e; e = (d + t1) | 0; d = cc; cc = b; b = a; a = (t1 + t2) | 0;
			}
			H[0] = (H[0] + a) | 0; H[1] = (H[1] + b) | 0; H[2] = (H[2] + cc) | 0; H[3] = (H[3] + d) | 0;
			H[4] = (H[4] + e) | 0; H[5] = (H[5] + f) | 0; H[6] = (H[6] + g) | 0; H[7] = (H[7] + h) | 0;
		}
		return H.map(function(x){ return ("00000000" + (x >>> 0).toString(16)).slice(-8); }).join("");
	}
	function formDigest(tuples){ return sha256hex(JSON.stringify(tuples)); }
`

// FormSnapshotScript builds the ordered snapshot of everything a browser would
// submit for an action, shared with the tests that run it under Node against a
// fake env. The submitted entries come from the browser's own submission set,
// new FormData(form, submitter): that covers select-multiple, disabled controls,
// controls joined by form=, form-associated custom elements (whatever they pass
// to setFormValue) and the submitter's name and value by definition. Entries are
// encoded as [name, kind, value] tuples in order. A File entry, or a FormData
// constructor that throws, makes the target unverifiable (bad), and no approval
// of it is redeemable. Entries named like a password or one-time-code control are
// dropped, conservatively by name. Composers outside the form (the focused one,
// or the target) are added from their own text; one whose value cannot be read
// is bad too. env supplies the DOM reads at prototype level, so a control named
// "elements" or "getAttribute" cannot shadow them.
const FormSnapshotScript = `
	function buildSnapshot(env, form, sub, t, focus, tIsEntry){
		var tuples = [], bad = "";
		if (form) {
			var fd = null;
			try { fd = env.formData(form, sub); } catch (x) { bad = "the browser could not build this form's submission"; }
			if (!bad) {
				// Password and one-time-code controls are never bound, and their
				// entries are dropped by name. That is only safe when nothing else
				// in the submission shares the name: if any entry with such a name
				// is not accounted for by an enabled sensitive control, the target
				// is unverifiable (a hidden "to" beside a password "to" would
				// otherwise be invisible to the binding).
				var skip = {}, total = {};
				var els = env.elements(form);
				for (var i = 0; i < els.length; i++) {
					var ty = String(els[i].type || "").toLowerCase(), ac = String(env.attr(els[i], "autocomplete") || "");
					if ((ty === "password" || /one-time-code|password/i.test(ac)) && !env.disabled(els[i])) {
						if (!els[i].name) continue; // an unnamed control submits nothing
						var k = "n:" + String(els[i].name);
						skip[k] = (skip[k] || 0) + 1;
					}
				}
				var entries = env.entries(fd);
				for (var j = 0; j < entries.length; j++) { var kk = "n:" + String(entries[j][0]); total[kk] = (total[kk] || 0) + 1; }
				for (var key in skip) { if (total[key] !== skip[key]) { bad = "a password or one-time-code field shares its name with another field"; } }
				if (!bad) {
					for (var m2 = 0; m2 < entries.length; m2++) {
						var name = String(entries[m2][0]), val = entries[m2][1];
						if (env.isFile(val)) { bad = "the form submits a file"; break; }
						if (skip["n:" + name]) continue;
						tuples.push([name, "string", String(val)]);
					}
				}
			}
		}
		function add(c, mustRead){
			var tag = String(c.tagName || "").toLowerCase();
			if (env.disabled(c)) return;
			var ty = String(c.type || "").toLowerCase();
			if (ty === "file") { bad = "the page has a file input"; return; }
			if (/^(password|submit|button|reset|image)$/.test(ty)) return;
			var ac = String(env.attr(c, "autocomplete") || "");
			if (/one-time-code|password/i.test(ac)) return;
			var v;
			if (tag === "select") v = Array.prototype.map.call(c.selectedOptions || [], function(o){ return o.value; }).join("\u0000");
			else if (c.isContentEditable) v = env.text(c);
			else if (typeof c.value === "string") v = c.value;
			else { if (mustRead) bad = "a text control's value cannot be read"; return; }
			tuples.push(["(page)", ty || tag, v]);
		}
		if (!bad) {
			if (focus && focus !== t && (focus.tagName === "TEXTAREA" || focus.isContentEditable || focus.tagName === "INPUT") && !(form && env.inForm(form, focus))) add(focus, false);
			if (t && !(form && env.inForm(form, t)) && (tIsEntry || String(t.tagName || "").toLowerCase() === "select")) add(t, true);
		}
		return {tuples: tuples, bad: bad};
	}
	// normTarget keeps a browsing-context name exactly (names are case-sensitive)
	// and lower-cases only the reserved keywords.
	function normTarget(s){ s = String(s || ""); return /^_(self|blank|parent|top)$/i.test(s) ? s.toLowerCase() : s; }
	// inspectForm returns what an action would submit: the digest of the
	// submission snapshot and the effective submission (action, method, enctype,
	// target) after any override by the submitter, which is the clicked submit
	// button or, for Enter in a field, the form's default (first enabled submit)
	// button. The submitter's own name, id, type and value are folded into the
	// digest. bad or oversized means the target cannot be verified in full.
	function inspectForm(env, form, t, focus, tIsEntry, maxc){
		var r = {bad: "", oversized: false, digest: "", action: "", method: "", enctype: "", target: "", overrides: ""};
		var sub = null;
		if (form) {
			var els = env.elements(form);
			var isSub = function(c){ var y = String(c.type || "").toLowerCase(); return (String(c.tagName).toUpperCase() === "BUTTON" || String(c.tagName).toUpperCase() === "INPUT") && (y === "submit" || y === "image"); };
			if (t && isSub(t)) sub = t;
			if (!sub) { for (var q = 0; q < els.length; q++) { if (isSub(els[q]) && !env.disabled(els[q])) { sub = els[q]; break; } } }
			// Present-but-empty is not absent: an empty formaction resolves to the
			// document URL and an empty formtarget is still an override, so each
			// attribute's presence and raw value are bound alongside the effective
			// result. formnovalidate, novalidate and accept-charset change how the
			// submission behaves and are bound too.
			var pick = function(subAttr, formAttr){
				if (sub && env.has(sub, subAttr)) return String(env.attr(sub, subAttr) || "");
				return String(env.attr(form, formAttr) || "");
			};
			var pr = function(e, n){
				if (!e || !env.has(e, n)) return [n, 0, ""];
				var raw = String(env.attr(e, n) || "");
				if (/target$/.test(n)) raw = normTarget(raw);
				else if (/(method|enctype)$/.test(n)) raw = raw.toLowerCase();
				return [n, 1, raw];
			};
			var fa = sub && env.has(sub, "formaction") ? String(env.attr(sub, "formaction") || "") : String(env.attr(form, "action") || "");
			r.action = env.abs(fa || env.base());
			var m = pick("formmethod", "method").toLowerCase();
			r.method = (m === "post" || m === "dialog") ? m : "get";
			var en = pick("formenctype", "enctype").toLowerCase();
			r.enctype = (en === "multipart/form-data" || en === "text/plain") ? en : "application/x-www-form-urlencoded";
			r.target = normTarget(pick("formtarget", "target"));
			r.overrides = JSON.stringify([pr(sub, "formaction"), pr(sub, "formmethod"), pr(sub, "formenctype"), pr(sub, "formtarget"), pr(sub, "formnovalidate"),
				pr(form, "action"), pr(form, "method"), pr(form, "enctype"), pr(form, "target"), pr(form, "novalidate"), pr(form, "accept-charset")]);
		}
		var snap = buildSnapshot(env, form, sub, t, focus, tIsEntry);
		if (snap.bad) { r.bad = snap.bad; return r; }
		if (JSON.stringify(snap.tuples).length > maxc) { r.oversized = true; return r; }
		var d = formDigest(snap.tuples);
		if (sub) d = formDigest([d, [sub.name || "", sub.id || "", String(sub.type || "").toLowerCase(), sub.value === undefined ? "" : String(sub.value)]]);
		r.digest = d;
		return r;
	}
`

const targetProbeScript = `(function(kind, selector, px, py){
	var MAXC = 4000000;
` + FormDigestScript + FormSnapshotScript + `
	function deepActive(){ var a = document.activeElement; while (a && a.shadowRoot && a.shadowRoot.activeElement) a = a.shadowRoot.activeElement; return a; }
	function deepPoint(x, y){ var e = document.elementFromPoint(x, y); while (e && e.shadowRoot) { var i = e.shadowRoot.elementFromPoint(x, y); if (!i || i === e) break; e = i; } return e; }
	function txt(e){ return (e.innerText || e.textContent || "").replace(/\s+/g, " ").trim(); }
	function labelled(e){
		var ids = e.getAttribute("aria-labelledby");
		if (!ids) return "";
		return ids.split(/\s+/).map(function(i){ var n = document.getElementById(i); return n ? txt(n) : ""; }).join(" ").trim();
	}
	var ENTRY_SEL = "textarea,input,[contenteditable],[role=textbox],[role=searchbox],[role=combobox]";
	function isEntryEl(e){
		var t = (e.type || "").toLowerCase();
		var role = (e.getAttribute("role") || "").toLowerCase();
		return e.tagName === "TEXTAREA" || !!e.isContentEditable || role === "textbox" || role === "searchbox" || role === "combobox" ||
			(e.tagName === "INPUT" && !/^(checkbox|radio|button|submit|reset|image|file|hidden|range|color)$/.test(t));
	}
	// A control's label comes from its own attributes. For a text-entry element,
	// or anything that holds one, the visible text and value are what someone
	// typed, so they are never used (they would leak into cards and the audit).
	function nameOf(e){
		var n = e.getAttribute("aria-label") || labelled(e) || e.title || "";
		if (isEntryEl(e)) return String(n || e.placeholder || "").replace(/\s+/g, " ").trim().slice(0, 200);
		var holdsEntry = !!(e.querySelector && e.querySelector(ENTRY_SEL));
		var t = (e.type || "").toLowerCase();
		if (!n && e.tagName === "INPUT" && (t === "submit" || t === "button" || t === "reset" || t === "image")) n = e.value || e.alt || "";
		if (!n && !holdsEntry && e.tagName !== "SELECT") n = txt(e);
		if (!n && e.labels && e.labels.length && !holdsEntry) n = txt(e.labels[0]);
		if (!n) n = e.alt || "";
		if (!n && !holdsEntry) { var img = e.querySelector && e.querySelector("img[alt],svg title"); if (img) n = img.alt || txt(img); }
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
	// A custom element with no open shadow root and nothing inside it is most
	// likely the host of a closed shadow root, whose contents cannot be read.
	if (tag.indexOf("-") > 0 && !el.shadowRoot && el.childElementCount === 0 && !txt(el)) { out.reason = "target is inside a closed shadow root"; return out; }
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
	var HF = HTMLFormElement.prototype, EP = Element.prototype;
	var env = {
		attr: function(e, n){ try { return EP.getAttribute.call(e, n); } catch (x) { return null; } },
		elements: function(f){ try { return Array.prototype.slice.call(Object.getOwnPropertyDescriptor(HF, "elements").get.call(f)); } catch (x) { return []; } },
		disabled: function(e){ try { return EP.matches.call(e, ":disabled"); } catch (x) { return !!e.disabled; } },
		text: function(e){ return e.innerText || e.textContent || ""; },
		formData: function(f, sub){ return sub ? new FormData(f, sub) : new FormData(f); },
		entries: function(fd){ var out = []; fd.forEach(function(v, k){ out.push([k, v]); }); return out; },
		isFile: function(v){ return typeof v !== "string"; },
		inForm: function(f, c){ try { return Array.prototype.indexOf.call(Object.getOwnPropertyDescriptor(HF, "elements").get.call(f), c) >= 0; } catch (x) { return false; } },
		has: function(e, n){ try { return EP.hasAttribute.call(e, n); } catch (x) { return false; } },
		abs: abs,
		base: function(){ return location.href; }
	};
	var fi = inspectForm(env, t.form || (t.closest ? t.closest("form") : null), t, deepActive(), (function(){
		var ty0 = (t.type || t.getAttribute("type") || "").toLowerCase(), r0 = (t.getAttribute("role") || "").toLowerCase();
		return t.tagName.toLowerCase() === "textarea" || t.isContentEditable || r0 === "textbox" || r0 === "searchbox" || r0 === "combobox" ||
			(t.tagName.toLowerCase() === "input" && !/^(checkbox|radio|button|submit|reset|image|file|hidden|range|color)$/.test(ty0));
	})(), MAXC);
	var form = t.form || (t.closest ? t.closest("form") : null);
	var attrSearch = /\bsearch\b/i.test((t.getAttribute("aria-label") || "") + " " + (t.id || "") + " " + (t.getAttribute("name") || ""));
	out.search = type === "search" || role === "searchbox" || attrSearch || !!(t.closest && t.closest("[role=search]"));
	if (form) {
		out.in_form = true;
		var formEls = env.elements(form);
		out.form_action = fi.action; out.form_method = fi.method; out.form_enctype = fi.enctype; out.form_target = fi.target; out.form_overrides = fi.overrides;
		out.form_has_submit = !!EP.querySelector.call(form, "button:not([type]),button[type=submit],input[type=submit],input[type=image]");
		if (env.attr(form, "role") === "search") out.search = true;
		var controls = 0;
		for (var i = 0; i < formEls.length; i++) { if ((formEls[i].type || "") !== "hidden") controls++; }
		if (tag === "select") out.auto_submit = t.hasAttribute("onchange") || (controls === 1 && !out.form_has_submit);
	} else if (tag === "select") { out.auto_submit = t.hasAttribute("onchange"); }
	var r = t.getBoundingClientRect();
	out.rect = {x: r.x, y: r.y, w: r.width, h: r.height, vw: window.innerWidth, vh: window.innerHeight};
	if (fi.bad) { out.reason = fi.bad; return out; }
	if (fi.oversized) { out.oversized = true; } else { out.content_hash = fi.digest; }
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
	return info, nil
}
