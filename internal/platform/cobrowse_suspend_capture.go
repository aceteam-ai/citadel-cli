package platform

import (
	"context"
	"errors"
	"math"
)

// No form values, storage, cookies, history or page text. The isolated world
// has clean intrinsics, and the script bounds strings before sending them over
// CDP. Byte/scheme/coordinate validation is repeated in Go before persistence.
const suspendTabScript = `(function(){
var u = String(location.href), t = String(document.title || "");
if (u.length > 8192 || t.length > 2048) return null;
return {url:u,title:t,x:window.scrollX,y:window.scrollY};
})()`

const maxSuspendCDPMessageBytes = 1 << 20

// openSuspendCaptureConn gives the entire read sequence one caller-owned
// context/deadline and one bounded CDP connection. It does not grant driver or
// page authority: the manager must validate its owner/state and browser/target
// identity before constructing p, and fence every await below.
func openSuspendCaptureConn(ctx context.Context, p pin) (*cdpConn, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("browser suspend capture requires a finite deadline")
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.New("browser suspend capture deadline expired")
	}
	c, err := openCDPConn(ctx, p)
	if err != nil {
		return nil, errors.New("connect to browser suspend capture")
	}
	c.ws.SetReadLimit(maxSuspendCDPMessageBytes)
	return c, nil
}

// readSuspendTabOn reads a single already-pinned page, including its full URL
// (the status projection may truncate it). fence must recheck session identity,
// generation, lifecycle state and the authorized driver after every await. No
// error includes a CDP/provider message or captured data.
func readSuspendTabOn(c cdpCaller, fence func() error) (suspendTab, error) {
	refuse := func() (suspendTab, error) { return suspendTab{}, errors.New("could not capture current browser tab") }
	if fence == nil || fence() != nil {
		return refuse()
	}
	tree, err := c.Call("Page.getFrameTree", nil)
	if err != nil || fence() != nil {
		return refuse()
	}
	frameID := rootFrameID(tree)
	if frameID == "" {
		return refuse()
	}
	world, err := c.Call("Page.createIsolatedWorld", map[string]any{"frameId": frameID, "worldName": "citadel-suspend"})
	if err != nil || fence() != nil {
		return refuse()
	}
	contextID, ok := world["executionContextId"].(float64)
	if !ok || contextID <= 0 || math.IsInf(contextID, 0) || math.IsNaN(contextID) || contextID != math.Trunc(contextID) || contextID > math.MaxInt32 {
		return refuse()
	}
	res, err := c.Call("Runtime.evaluate", map[string]any{"expression": suspendTabScript, "contextId": int(contextID), "returnByValue": true})
	if err != nil || fence() != nil {
		return refuse()
	}
	if exception, ok := res["exceptionDetails"]; ok && exception != nil {
		return refuse()
	}
	remote, _ := res["result"].(map[string]any)
	obj, _ := remote["value"].(map[string]any)
	url, urlOK := obj["url"].(string)
	title, titleOK := obj["title"].(string)
	x, xOK := obj["x"].(float64)
	y, yOK := obj["y"].(float64)
	if !urlOK || !titleOK || !xOK || !yOK {
		return refuse()
	}
	tab := suspendTab{URL: url, Title: title, ScrollX: x, ScrollY: y, Active: true}
	probe := suspendManifest{Version: suspendManifestVersion, SessionID: "cb-0", Generation: 1, Tabs: []suspendTab{tab}}
	if validateSuspendManifest(probe, probe.SessionID, probe.Generation) != nil {
		return refuse()
	}
	return tab, nil
}
