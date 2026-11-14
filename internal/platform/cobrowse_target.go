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

const targetProbeScript = `(function(kind, selector, px, py){
	var MAXC = 4000000;
` + FormDigestScript + `
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
	var tuples = [], used = 0, tooBig = false;
	function field(c){
		if (tooBig) return;
		var ty = (c.type || "").toLowerCase();
		// Hidden fields carry recipients and amounts, so they are bound too.
		if (/^(password|file|submit|button|reset|image)$/.test(ty)) return;
		var ac = (c.getAttribute && c.getAttribute("autocomplete")) || "";
		if (/one-time-code|password/i.test(ac)) return;
		var v = c.isContentEditable ? (c.innerText || c.textContent || "") : (c.value !== undefined ? c.value : "");
		var nm = c.name || "", idv = c.id || "";
		used += nm.length + idv.length + ty.length + v.length + 16;
		if (used > MAXC) { tooBig = true; return; }
		tuples.push([nm, idv, ty, v, !!c.checked]);
	}
	if (form) { for (var j = 0; j < form.elements.length; j++) field(form.elements[j]); }
	var focus = deepActive();
	if (focus && focus !== t && (focus.tagName === "TEXTAREA" || focus.isContentEditable || focus.tagName === "INPUT")) field(focus);
	if (isEntry) field(t);
	if (tooBig) { out.oversized = true; } else { out.content_hash = formDigest(tuples); }
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
