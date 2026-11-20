package platform

// Unwired, single-attempt scroll restoration for an already navigated, pinned
// tab. The manager still owns navigation policy, target/browser/generation
// fences, load waiting, full-manifest ordering and durable reopen completion.
// This does not open/activate a page or acknowledge the whole reopen.

import (
	"context"
	"errors"
	"math"
)

// Run in an isolated main-frame world, with all captured values passed as CDP
// arguments, not source. A loading/redirected document or a clamped position
// is not a completed restore. One CSS pixel accommodates fractional rounding;
// future layout changes remain the manager's load/stability responsibility.
const suspendScrollRestoreFunction = `function(expectedURL,x,y){
if(String(location.href)!==expectedURL || document.readyState!=="complete") return false;
window.scrollTo({left:x,top:y,behavior:"instant"});
return String(location.href)===expectedURL &&
 Number.isFinite(window.scrollX) && Number.isFinite(window.scrollY) &&
 Math.abs(window.scrollX-x)<=1 && Math.abs(window.scrollY-y)<=1;
}`

// Unlike the restore attempt, final observation must not repair a changed page
// and then report that it was already restored. It reads only location, load
// state and scroll coordinates, never page contents or form values.
const suspendScrollVerifyFunction = `function(expectedURL,x,y){
return String(location.href)===expectedURL && document.readyState==="complete" &&
 Number.isFinite(window.scrollX) && Number.isFinite(window.scrollY) &&
 Math.abs(window.scrollX-x)<=1 && Math.abs(window.scrollY-y)<=1;
}`

// c must use the same finite caller-owned context (the bounded connection used
// by openSuspendCaptureConn is suitable). fence is local and rechecks the
// pinned session/browser/target/generation before AND after every awaited call.
// A failure may follow a scroll effect; retries must use the same incarnation,
// recheck the document and remain within the original finite budget.
func restoreSuspendTabScrollOn(ctx context.Context, c cdpCaller, tab suspendTab, fence func() error) error {
	return suspendTabScrollOn(ctx, c, tab, fence, true)
}

func verifySuspendTabScrollOn(ctx context.Context, c cdpCaller, tab suspendTab, fence func() error) error {
	return suspendTabScrollOn(ctx, c, tab, fence, false)
}

func suspendTabScrollOn(ctx context.Context, c cdpCaller, tab suspendTab, fence func() error, restore bool) error {
	fail := func() error { return errors.New("could not confirm browser tab scroll position") }
	if ctx == nil || c == nil || fence == nil || ctx.Err() != nil {
		return fail()
	}
	if _, finite := ctx.Deadline(); !finite {
		return fail()
	}
	probe := tab
	probe.Active = true // a single tab is validated independently of selection
	m := suspendManifest{Version: suspendManifestVersion, SessionID: "cb-0", Generation: 1, Tabs: []suspendTab{probe}}
	if validateSuspendManifest(m, m.SessionID, m.Generation) != nil {
		return fail()
	}
	check := func() bool { return ctx.Err() == nil && fence() == nil }
	call := func(method string, params map[string]any) (map[string]any, error) {
		if !check() {
			return nil, fail()
		}
		result, err := c.Call(method, params)
		if err != nil || !check() {
			return nil, fail()
		}
		return result, nil
	}
	tree, err := call("Page.getFrameTree", nil)
	if err != nil {
		return fail()
	}
	frame := rootFrameID(tree)
	url, found := frameURLFromTree(tree, frame)
	if frame == "" || len(frame) > 128 || !found || url != tab.URL {
		return fail()
	}
	world, err := call("Page.createIsolatedWorld", map[string]any{"frameId": frame, "worldName": "citadel-reopen-scroll"})
	if err != nil {
		return fail()
	}
	worldID, ok := world["executionContextId"].(float64)
	if !ok || worldID <= 0 || math.IsNaN(worldID) || math.IsInf(worldID, 0) || worldID != math.Trunc(worldID) || worldID > math.MaxInt32 {
		return fail()
	}
	function := suspendScrollVerifyFunction
	if restore {
		function = suspendScrollRestoreFunction
	}
	result, err := call("Runtime.callFunctionOn", map[string]any{
		"functionDeclaration": function, "executionContextId": int(worldID), "returnByValue": true,
		"arguments": []map[string]any{{"value": tab.URL}, {"value": tab.ScrollX}, {"value": tab.ScrollY}},
	})
	if err != nil || result["exceptionDetails"] != nil {
		return fail()
	}
	remote, _ := result["result"].(map[string]any)
	ready, _ := remote["value"].(bool)
	if remote["type"] != "boolean" || !ready {
		return fail()
	}
	return nil
}
