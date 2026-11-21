package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cdpdiscovery"
)

type suspendRestoredFixture struct {
	plan     suspendRestoredPagePlan
	targets  []cdpdiscovery.Target
	expected []suspendTab
	calls    []string
	activeID string
	stale    bool
	after    func(string)
}

func newSuspendRestoredFixture(n int) *suspendRestoredFixture {
	f := &suspendRestoredFixture{plan: suspendRestoredPagePlan{
		manifest: suspendManifest{Version: suspendManifestVersion, SessionID: "cb-123", Generation: 3},
		port:     9876, browserID: "browser-a",
	}}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("page-%d", i)
		tab := suspendTab{URL: fmt.Sprintf("https://example.invalid/%d?private-restored-sentinel#part", i),
			ScrollX: -3.5, ScrollY: float64(i + 10), Active: i == n-1}
		f.plan.targetIDs = append(f.plan.targetIDs, id)
		f.plan.manifest.Tabs = append(f.plan.manifest.Tabs, tab)
		f.expected = append(f.expected, tab)
		f.targets = append(f.targets, cdpdiscovery.Target{ID: id, Type: "page", URL: tab.URL,
			WebSocketDebuggerURL: "ws://127.0.0.1:9876/devtools/page/" + id})
		if tab.Active {
			f.activeID = id
		}
	}
	return f
}

func (f *suspendRestoredFixture) active() (string, error) { return f.activeID, nil }

func (f *suspendRestoredFixture) fence() error {
	if f.stale {
		return errors.New("private-restored-sentinel")
	}
	return nil
}

func (f *suspendRestoredFixture) source(t *testing.T) suspendRestoredPageSource {
	t.Helper()
	step := func(name string) {
		f.calls = append(f.calls, name)
		if f.after != nil {
			f.after(name)
		}
	}
	return suspendRestoredPageSource{
		verify: func(ctx context.Context, port int, id string) error {
			if _, finite := ctx.Deadline(); !finite || port != 9876 || id != "browser-a" {
				t.Error("restoration observation lost its budget or browser pin")
			}
			step("verify")
			return nil
		},
		list: func(context.Context, int) ([]cdpdiscovery.Target, error) {
			step("list")
			return append([]cdpdiscovery.Target(nil), f.targets...), nil
		},
		position: func(ctx context.Context, p pin, tab suspendTab, fence func() error) error {
			step(p.id)
			var expected *suspendTab
			for i := range f.expected {
				if p.id == fmt.Sprintf("page-%d", i) {
					expected = &f.expected[i]
				}
			}
			if expected == nil || tab != *expected || !p.background || p.port != 9876 || p.ws != "ws://127.0.0.1:9876/devtools/page/"+p.id {
				t.Error("observation lost its ordered tab or exact target pin")
				return errors.New("private-restored-sentinel")
			}
			c, _ := suspendRestoreFixture()
			c.tree = map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "root-frame", "url": tab.URL}}}
			err := verifySuspendTabScrollOn(ctx, c, tab, fence)
			if err == nil && c.params[2]["functionDeclaration"] != suspendScrollVerifyFunction {
				t.Error("final observation repaired the page instead of reading it")
			}
			return err
		},
	}
}

func TestSuspendRestoredPagesCompleteOrderedObservation(t *testing.T) {
	for _, duplicateURL := range []bool{false, true} {
		f := newSuspendRestoredFixture(2)
		if duplicateURL {
			f.plan.manifest.Tabs[1].URL = f.plan.manifest.Tabs[0].URL
			f.expected[1].URL = f.expected[0].URL
			f.targets[1].URL = f.targets[0].URL
		}
		// Discovery ordering is unrelated to capture/restore ordering.
		f.targets[0], f.targets[1] = f.targets[1], f.targets[0]
		if err := verifySuspendRestoredPagesWith(suspendPagesContext(t), f.plan, f.active, f.fence, f.source(t)); err != nil {
			t.Fatal(err)
		}
		want := []string{"verify", "list", "verify", "page-0", "verify", "page-1", "list", "verify"}
		if !reflect.DeepEqual(f.calls, want) {
			t.Fatal("whole-manifest observation omitted or reordered a browser/page/discovery proof")
		}
	}
}

func TestSuspendRestoredPagesCopiesCallerPlan(t *testing.T) {
	f := newSuspendRestoredFixture(2)
	f.after = func(name string) {
		if name == "verify" {
			f.plan.targetIDs[1] = "replacement"
			f.plan.manifest.Tabs[1].URL = "https://replacement.invalid"
			f.plan.manifest.Tabs[1].ScrollY = 999
		}
	}
	if err := verifySuspendRestoredPagesWith(suspendPagesContext(t), f.plan, f.active, f.fence, f.source(t)); err != nil {
		t.Fatal("caller slice mutation retargeted the bounded observation plan")
	}
}

func TestSuspendRestoredPagesAdmissionBeforeIO(t *testing.T) {
	for _, kind := range []string{"nil-context", "no-budget", "cancelled", "nil-active", "nil-fence", "nil-verify", "nil-list", "nil-position", "port-zero", "port-large", "browser-empty", "browser-long", "browser-path", "browser-encoded", "no-tabs", "many-tabs", "bad-manifest", "aggregate", "missing-target", "extra-target", "duplicate-target", "empty-target", "long-target", "path-target", "encoded-target", "space-target", "dot-target", "active", "fence"} {
		t.Run(kind, func(t *testing.T) {
			n := 2
			if kind == "many-tabs" {
				n = maxSuspendTabs + 1
			} else if kind == "aggregate" {
				n = maxSuspendTabs
			}
			f := newSuspendRestoredFixture(n)
			ctx, active, fence, source := suspendPagesContext(t), f.active, f.fence, f.source(t)
			switch kind {
			case "nil-context":
				ctx = nil
			case "no-budget":
				ctx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "nil-active":
				active = nil
			case "nil-fence":
				fence = nil
			case "nil-verify":
				source.verify = nil
			case "nil-list":
				source.list = nil
			case "nil-position":
				source.position = nil
			case "port-zero":
				f.plan.port = 0
			case "port-large":
				f.plan.port = 65536
			case "browser-empty":
				f.plan.browserID = ""
			case "browser-long":
				f.plan.browserID = strings.Repeat("a", 129)
			case "browser-path":
				f.plan.browserID = "../other"
			case "browser-encoded":
				f.plan.browserID = "%2Fother"
			case "no-tabs":
				f.plan.manifest.Tabs = nil
			case "bad-manifest":
				f.plan.manifest.Generation = 0
			case "aggregate":
				for i := range f.plan.manifest.Tabs {
					f.plan.manifest.Tabs[i].URL = "https://example.invalid/" + strings.Repeat("a", 5000)
					f.expected[i].URL = f.plan.manifest.Tabs[i].URL
					f.targets[i].URL = f.plan.manifest.Tabs[i].URL
				}
			case "missing-target":
				f.plan.targetIDs = f.plan.targetIDs[:1]
			case "extra-target":
				f.plan.targetIDs = append(f.plan.targetIDs, "extra")
			case "duplicate-target":
				f.plan.targetIDs[1] = f.plan.targetIDs[0]
				// Keep selection coherent with the forged mapping so only the
				// duplicate guard can reject this before browser IO.
				f.activeID = f.plan.targetIDs[0]
			case "empty-target":
				f.plan.targetIDs[0] = ""
			case "long-target":
				f.plan.targetIDs[0] = strings.Repeat("a", 129)
			case "path-target":
				f.plan.targetIDs[0] = "page-0?other"
			case "encoded-target":
				f.plan.targetIDs[0] = "%2Fother"
			case "space-target":
				f.plan.targetIDs[0] = "page 0"
			case "dot-target":
				f.plan.targetIDs[0] = ".."
			case "active":
				f.activeID = "page-0"
			case "fence":
				f.stale = true
			}
			if err := verifySuspendRestoredPagesWith(ctx, f.plan, active, fence, source); err == nil || len(f.calls) != 0 {
				t.Fatal("invalid plan performed browser IO or succeeded")
			}
		})
	}
}

func TestSuspendRestoredPagesKeepsOriginalBudget(t *testing.T) {
	f := newSuspendRestoredFixture(1)
	ctx, cancel := context.WithCancel(suspendPagesContext(t))
	defer cancel()
	source := f.source(t)
	verify := source.verify
	source.verify = func(got context.Context, port int, id string) error {
		if got != ctx {
			t.Error("observation replaced the caller-owned original budget")
		}
		return verify(got, port, id)
	}
	source.list = func(got context.Context, _ int) ([]cdpdiscovery.Target, error) {
		if got != ctx {
			t.Error("discovery replaced the original budget")
		}
		f.calls = append(f.calls, "list")
		cancel()
		<-got.Done()
		// Even a provider reporting success after cancellation is not proof.
		return f.targets, nil
	}
	if err := verifySuspendRestoredPagesWith(ctx, f.plan, f.active, f.fence, source); err == nil || !reflect.DeepEqual(f.calls, []string{"verify", "list"}) {
		t.Fatal("cancelled discovery succeeded or continued to another page")
	}
}

func TestSuspendRestoredPagesAcceptsWindowBound(t *testing.T) {
	f := newSuspendRestoredFixture(maxSuspendTabs)
	if err := verifySuspendRestoredPagesWith(suspendPagesContext(t), f.plan, f.active, f.fence, f.source(t)); err != nil || len(f.calls) != 2*maxSuspendTabs+4 {
		t.Fatal("complete bounded tab window was rejected or partly observed")
	}
}

func TestSuspendRestoredPagesActiveReadFailure(t *testing.T) {
	for _, changedFence := range []bool{false, true} {
		f := newSuspendRestoredFixture(2)
		active := func() (string, error) {
			if changedFence {
				f.stale = true
				return f.activeID, nil
			}
			return f.activeID, errors.New("private-restored-sentinel")
		}
		if err := verifySuspendRestoredPagesWith(suspendPagesContext(t), f.plan, active, f.fence, f.source(t)); err == nil || len(f.calls) != 0 || strings.Contains(err.Error(), "private-restored-sentinel") {
			t.Fatal("failed or stale active-page observation performed IO or succeeded")
		}
	}
}

func TestSuspendRestoredPagesReadOnlyCDPFences(t *testing.T) {
	for boundary := 1; boundary <= 6; boundary++ {
		c, tab := suspendRestoreFixture()
		checks := 0
		err := verifySuspendTabScrollOn(suspendPagesContext(t), c, tab, func() error {
			checks++
			if checks == boundary {
				return errors.New("private-restored-sentinel")
			}
			return nil
		})
		if err == nil || len(c.calls) != boundary/2 || strings.Contains(err.Error(), "private-restored-sentinel") {
			t.Fatal("read-only observation ignored its CDP incarnation fence")
		}
	}
	for _, kind := range []string{"url", "world", "exception", "false", "truthy", "type", "provider"} {
		c, tab := suspendRestoreFixture()
		switch kind {
		case "url":
			tab.URL += "other"
		case "world":
			c.world["executionContextId"] = float64(0)
		case "exception":
			c.result["exceptionDetails"] = map[string]any{"text": "private-restored-sentinel"}
		case "false":
			c.result["result"].(map[string]any)["value"] = false
		case "truthy":
			c.result["result"].(map[string]any)["value"] = "true"
		case "type":
			c.result["result"].(map[string]any)["type"] = "object"
		case "provider":
			c.fail = "Runtime.callFunctionOn"
		}
		if err := verifySuspendTabScrollOn(suspendPagesContext(t), c, tab, func() error { return nil }); err == nil || strings.Contains(err.Error(), "private-restored-sentinel") {
			t.Fatal("read-only observation accepted a changed/partial document or leaked provider detail")
		}
	}
}

func TestSuspendRestoredPagesReadOnlyScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("pure scroll-observation fixture needs Node.js")
	}
	source, err := json.Marshal(suspendScrollVerifyFunction)
	if err != nil {
		t.Fatal(err)
	}
	program := `const vm=require('node:vm'),assert=require('node:assert/strict');
const source=` + string(source) + `;
const cases=[
 {name:'exact',ok:true},{name:'rounding',dx:1,dy:-1,ok:true},
 {name:'x-outside',dx:1.01,ok:false},{name:'y-outside',dy:-1.01,ok:false},
 {name:'nonfinite-x',badX:true,ok:false},{name:'nonfinite-y',badY:true,ok:false},
 {name:'loading',ready:'loading',ok:false},{name:'interactive',ready:'interactive',ok:false},
 {name:'url',url:'https://other.invalid',ok:false},
 {name:'fragment',url:'https://example.invalid/page#other',ok:false}
];
for(const spec of cases){
 let calls=0;
 const context={expected:'https://example.invalid/page#part',x:-3.5,y:25.25,
 location:{href:spec.url||'https://example.invalid/page#part'},
 document:{readyState:spec.ready||'complete'},
 window:{scrollX:spec.badX?NaN:-3.5+(spec.dx||0),scrollY:spec.badY?Infinity:25.25+(spec.dy||0),
 scrollTo:()=>{calls++;throw Error('observation must never scroll');}}};
 const result=vm.runInNewContext('('+source+')(expected,x,y)',context,{timeout:1000});
 assert.equal(result,spec.ok,spec.name);assert.equal(calls,0,spec.name);
}
console.log('10 read-only document cases passed');`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, node, "-e", program).CombinedOutput(); err != nil {
		t.Fatalf("pure scroll-observation script failed: %v\n%s", err, out)
	}
}

func TestSuspendRestoredPagesRefusesIncompleteDiscovery(t *testing.T) {
	for _, pass := range []int{1, 2} {
		for _, change := range []string{"missing", "extra", "duplicate", "type", "endpoint", "host", "query", "url", "fragment"} {
			t.Run(fmt.Sprintf("%d/%s", pass, change), func(t *testing.T) {
				f := newSuspendRestoredFixture(2)
				lists := 0
				f.after = func(name string) {
					if name != "list" {
						return
					}
					lists++
					if lists != pass {
						return
					}
					switch change {
					case "missing":
						f.targets = f.targets[:1]
					case "extra":
						f.targets = append(f.targets, cdpdiscovery.Target{ID: "extra"})
					case "duplicate":
						f.targets[1] = f.targets[0]
					case "type":
						f.targets[0].Type = "worker"
					case "endpoint":
						f.targets[0].WebSocketDebuggerURL += "/other"
					case "host":
						f.targets[0].WebSocketDebuggerURL = "ws://other.invalid/devtools/page/page-0"
					case "query":
						f.targets[0].WebSocketDebuggerURL += "?other"
					case "url":
						f.targets[0].URL += "?other"
					case "fragment":
						f.targets[0].URL += "-other"
					}
				}
				if err := verifySuspendRestoredPagesWith(suspendPagesContext(t), f.plan, f.active, f.fence, f.source(t)); err == nil || strings.Contains(err.Error(), "private-restored-sentinel") {
					t.Fatal("incomplete/changed discovery succeeded or exposed page details")
				}
				if pass == 1 && len(f.calls) != 2 || pass == 2 && len(f.calls) != 7 {
					t.Fatal("observation continued after a failed complete-discovery proof")
				}
			})
		}
	}
}

func TestSuspendRestoredPagesFencesEveryAwait(t *testing.T) {
	for boundary := 1; boundary <= 8; boundary++ {
		for _, change := range []string{"fence", "active", "cancel", "error"} {
			t.Run(fmt.Sprintf("%d/%s", boundary, change), func(t *testing.T) {
				f := newSuspendRestoredFixture(2)
				ctx, cancel := context.WithCancel(suspendPagesContext(t))
				defer cancel()
				f.after = func(string) {
					if len(f.calls) == boundary {
						switch change {
						case "fence":
							f.stale = true
						case "active":
							f.activeID = "page-0"
						case "cancel":
							cancel()
						}
					}
				}
				source := f.source(t)
				if change == "error" {
					verify, list, position := source.verify, source.list, source.position
					providerErr := func() error {
						if len(f.calls) == boundary {
							return errors.New("private-restored-sentinel")
						}
						return nil
					}
					source.verify = func(ctx context.Context, port int, id string) error { _ = verify(ctx, port, id); return providerErr() }
					source.list = func(ctx context.Context, port int) ([]cdpdiscovery.Target, error) {
						targets, _ := list(ctx, port)
						return targets, providerErr()
					}
					source.position = func(ctx context.Context, p pin, tab suspendTab, fence func() error) error {
						_ = position(ctx, p, tab, fence)
						return providerErr()
					}
				}
				err := verifySuspendRestoredPagesWith(ctx, f.plan, f.active, f.fence, source)
				if err == nil || len(f.calls) != boundary || strings.Contains(err.Error(), "private-restored-sentinel") {
					t.Fatal("observation continued or acknowledged after a failed awaited proof")
				}
			})
		}
	}
}
