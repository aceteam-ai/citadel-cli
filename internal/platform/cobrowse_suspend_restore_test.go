package platform

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

type suspendRestoreCaller struct {
	calls               []string
	params              []map[string]any
	tree, world, result map[string]any
	fail                string
	after               func(string)
}

func (c *suspendRestoreCaller) Call(method string, params map[string]any) (map[string]any, error) {
	c.calls = append(c.calls, method)
	c.params = append(c.params, params)
	if c.after != nil {
		c.after(method)
	}
	if c.fail == method {
		return nil, errors.New("private-restore-sentinel")
	}
	switch method {
	case "Page.getFrameTree":
		return c.tree, nil
	case "Page.createIsolatedWorld":
		return c.world, nil
	default:
		return c.result, nil
	}
}

func suspendRestoreFixture() (*suspendRestoreCaller, suspendTab) {
	tab := suspendTab{URL: "https://example.invalid/page?private-restore-sentinel#part", ScrollX: -3.5, ScrollY: 25.25}
	c := &suspendRestoreCaller{
		tree:   map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "root-frame", "url": "https://example.invalid/page?private-restore-sentinel", "urlFragment": "#part"}}},
		world:  map[string]any{"executionContextId": float64(7)},
		result: map[string]any{"result": map[string]any{"type": "boolean", "value": true}},
	}
	return c, tab
}

func TestSuspendScrollRestorePinnedIsolatedAttempt(t *testing.T) {
	c, tab := suspendRestoreFixture()
	original := tab
	if err := restoreSuspendTabScrollOn(suspendPagesContext(t), c, tab, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.calls, []string{"Page.getFrameTree", "Page.createIsolatedWorld", "Runtime.callFunctionOn"}) {
		t.Fatal("restore performed an unexpected or missing browser effect")
	}
	if c.params[1]["frameId"] != "root-frame" || c.params[1]["worldName"] != "citadel-reopen-scroll" {
		t.Fatal("restore did not isolate the main frame")
	}
	args := []map[string]any{{"value": tab.URL}, {"value": tab.ScrollX}, {"value": tab.ScrollY}}
	if c.params[2]["executionContextId"] != 7 || c.params[2]["returnByValue"] != true || c.params[2]["functionDeclaration"] != suspendScrollRestoreFunction || !reflect.DeepEqual(c.params[2]["arguments"], args) {
		t.Fatal("restore lost typed arguments or its isolated-world result")
	}
	if tab != original {
		t.Fatal("restoring an inactive tab changed its selection metadata")
	}
}

func TestSuspendScrollRestoreFencesAndBudget(t *testing.T) {
	for failedCheck := 1; failedCheck <= 6; failedCheck++ {
		c, tab := suspendRestoreFixture()
		checks := 0
		err := restoreSuspendTabScrollOn(suspendPagesContext(t), c, tab, func() error {
			checks++
			if checks == failedCheck {
				return errors.New("private-restore-sentinel")
			}
			return nil
		})
		if err == nil || len(c.calls) != failedCheck/2 || strings.Contains(err.Error(), "private-restore-sentinel") {
			t.Fatal("restore continued or acknowledged after its incarnation fence failed")
		}
	}
	for _, method := range []string{"Page.getFrameTree", "Page.createIsolatedWorld", "Runtime.callFunctionOn"} {
		c, tab := suspendRestoreFixture()
		ctx, cancel := context.WithCancel(suspendPagesContext(t))
		c.after = func(m string) {
			if m == method {
				cancel()
			}
		}
		if err := restoreSuspendTabScrollOn(ctx, c, tab, func() error { return nil }); err == nil {
			t.Fatal("cancelled restoration acknowledged success")
		}
		cancel()
	}
	for _, kind := range []string{"nil-context", "no-deadline", "cancelled", "nil-caller", "nil-fence", "bad-tab"} {
		t.Run(kind, func(t *testing.T) {
			c, tab := suspendRestoreFixture()
			ctx, caller, fence := suspendPagesContext(t), cdpCaller(c), func() error { return nil }
			switch kind {
			case "nil-context":
				ctx = nil
			case "no-deadline":
				ctx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "nil-caller":
				caller = nil
			case "nil-fence":
				fence = nil
			case "bad-tab":
				tab.ScrollY = math.NaN()
			}
			if err := restoreSuspendTabScrollOn(ctx, caller, tab, fence); err == nil || len(c.calls) != 0 {
				t.Fatal("invalid restore admission performed browser I/O")
			}
		})
	}
}

func TestSuspendScrollRestoreRejectsUnreadyDocuments(t *testing.T) {
	for _, kind := range []string{"missing-root", "long-root", "other-url", "fragment", "query", "missing-world", "zero-world", "negative-world", "nan-world", "infinite-world", "string-world", "fractional-world", "large-world", "null-result", "exception", "false-result", "truthy-result", "wrong-type"} {
		t.Run(kind, func(t *testing.T) {
			c, tab := suspendRestoreFixture()
			frame := c.tree["frameTree"].(map[string]any)["frame"].(map[string]any)
			wantCalls := 3
			switch kind {
			case "missing-root":
				frame["id"] = ""
				wantCalls = 1
			case "long-root":
				frame["id"] = strings.Repeat("x", 129)
				wantCalls = 1
			case "other-url":
				frame["url"] = "https://other.invalid"
				wantCalls = 1
			case "fragment":
				frame["urlFragment"] = "#elsewhere"
				wantCalls = 1
			case "query":
				frame["url"] = "https://example.invalid/page?changed"
				wantCalls = 1
			case "missing-world":
				delete(c.world, "executionContextId")
				wantCalls = 2
			case "zero-world":
				c.world["executionContextId"] = float64(0)
				wantCalls = 2
			case "negative-world":
				c.world["executionContextId"] = float64(-1)
				wantCalls = 2
			case "nan-world":
				c.world["executionContextId"] = math.NaN()
				wantCalls = 2
			case "infinite-world":
				c.world["executionContextId"] = math.Inf(1)
				wantCalls = 2
			case "string-world":
				c.world["executionContextId"] = "7"
				wantCalls = 2
			case "fractional-world":
				c.world["executionContextId"] = 1.5
				wantCalls = 2
			case "large-world":
				c.world["executionContextId"] = float64(math.MaxInt32) + 1
				wantCalls = 2
			case "null-result":
				c.result = nil
			case "exception":
				c.result["exceptionDetails"] = map[string]any{"text": "private-restore-sentinel"}
			case "false-result":
				c.result["result"].(map[string]any)["value"] = false
			case "truthy-result":
				c.result["result"].(map[string]any)["value"] = "true"
			case "wrong-type":
				c.result["result"].(map[string]any)["type"] = "object"
			}
			if err := restoreSuspendTabScrollOn(suspendPagesContext(t), c, tab, func() error { return nil }); err == nil || len(c.calls) != wantCalls || strings.Contains(err.Error(), "private-restore-sentinel") {
				t.Fatal("restore acknowledged a changed/unready document, invalid world or partial result")
			}
		})
	}
	for _, method := range []string{"Page.getFrameTree", "Page.createIsolatedWorld", "Runtime.callFunctionOn"} {
		c, tab := suspendRestoreFixture()
		c.fail = method
		if err := restoreSuspendTabScrollOn(suspendPagesContext(t), c, tab, func() error { return nil }); err == nil || strings.Contains(err.Error(), "private-restore-sentinel") {
			t.Fatal("provider failure succeeded or exposed private page detail")
		}
	}
}

// Pure JavaScript with a synthetic document/window, not a browser or a page
// connection. Optional on Go-only hosts; the private proof environment and
// standard GitHub Windows/Linux runners provide Node.
func TestSuspendScrollRestoreScriptBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("pure scroll-script fixture needs Node.js")
	}
	source, err := json.Marshal(suspendScrollRestoreFunction)
	if err != nil {
		t.Fatal(err)
	}
	program := `const vm=require('node:vm'), assert=require('node:assert/strict');
const source=` + string(source) + `;
const cases=[
 {name:'exact',ready:'complete',ok:true,calls:1},
 {name:'fractional-rounding',ready:'complete',dx:0.75,dy:-0.75,ok:true,calls:1},
 {name:'one-pixel-boundary',ready:'complete',dx:1,dy:-1,ok:true,calls:1},
 {name:'loading',ready:'loading',ok:false,calls:0},
 {name:'interactive',ready:'interactive',ok:false,calls:0},
 {name:'different-document',ready:'complete',url:'https://other.invalid',ok:false,calls:0},
 {name:'clamped-x',ready:'complete',dx:1.01,ok:false,calls:1},
 {name:'clamped-y',ready:'complete',dy:1.01,ok:false,calls:1},
 {name:'navigation-during-scroll',ready:'complete',afterURL:'https://other.invalid',ok:false,calls:1},
 {name:'nonfinite-x',ready:'complete',badX:true,ok:false,calls:1},
 {name:'nonfinite-y',ready:'complete',badY:true,ok:false,calls:1}
];
for(const spec of cases){
 let calls=0;
 const context={expected:'https://example.invalid/page#part',x:-3.5,y:25.25,
  location:{href:spec.url||'https://example.invalid/page#part'}, document:{readyState:spec.ready},
  window:{scrollX:0,scrollY:0}};
 context.window.scrollTo=(options)=>{
  calls++;
  assert.equal(options.left,context.x,spec.name);
  assert.equal(options.top,context.y,spec.name);
  assert.equal(options.behavior,'instant',spec.name);
  context.window.scrollX=spec.badX?NaN:options.left+(spec.dx||0);
  context.window.scrollY=spec.badY?Infinity:options.top+(spec.dy||0);
  if(spec.afterURL)context.location.href=spec.afterURL;
 };
 const result=vm.runInNewContext('('+source+')(expected,x,y)',context,{timeout:1000});
 assert.equal(result,spec.ok,spec.name);
 assert.equal(calls,spec.calls,spec.name);
}
console.log('11 pure-document scroll cases passed');`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, node, "-e", program).CombinedOutput(); err != nil {
		t.Fatalf("pure scroll script failed: %v\n%s", err, out)
	}
}
