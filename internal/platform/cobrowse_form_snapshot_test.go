package platform

import (
	"errors"
	"testing"
)

type snapshotCaller struct {
	calls     []string
	params    map[string]map[string]any
	world     any
	frame     string
	fail      string
	loader    string
	evalError error
	exception bool
}

func (f *snapshotCaller) Call(method string, params map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, method)
	if f.params == nil {
		f.params = map[string]map[string]any{}
	}
	f.params[method] = params
	if method == f.fail {
		return nil, errors.New("synthetic read failure")
	}
	switch method {
	case "Page.getFrameTree":
		result := frameTreeResult([]fakeFrame{{ID: f.frame, URL: "https://form.test"}})
		result["frameTree"].(map[string]any)["frame"].(map[string]any)["loaderId"] = f.loader
		return result, nil
	case "Page.createIsolatedWorld":
		return map[string]any{"executionContextId": f.world}, nil
	case "Runtime.evaluate":
		if f.evalError != nil {
			return nil, f.evalError
		}
		if f.exception {
			return map[string]any{"exceptionDetails": map[string]any{"text": "synthetic exception"}, "result": map[string]any{"value": map[string]any{"resolved": true}}}, nil
		}
		return map[string]any{"result": map[string]any{"value": map[string]any{"resolved": true, "content_hash": "bound", "form_files": []any{map[string]any{"field": "att", "name": "invoice.txt", "size": 8.0, "type": "text/plain", "last_modified": 100.0}}}}}, nil
	}
	panic("unexpected CDP method")
}

func TestFormSnapshotContextReuseIsDocumentScoped(t *testing.T) {
	var cache formSnapshotWorld
	probe := func(target, frame, loader string, world float64, wantCreate int, wantID int) {
		t.Helper()
		f := &snapshotCaller{frame: frame, loader: loader, world: world}
		if _, err := inspectTargetCachedOn(f, TargetProbe{}, func() error { return nil }, &cache, target); err != nil {
			t.Fatal(err)
		}
		created := 0
		for _, call := range f.calls {
			if call == "Page.createIsolatedWorld" {
				created++
			}
		}
		if created != wantCreate || f.params["Runtime.evaluate"]["contextId"] != wantID {
			t.Fatalf("context reuse: calls=%v params=%v", f.calls, f.params)
		}
	}
	probe("target", "frame", "loader", 17, 1, 17)
	probe("target", "frame", "loader", 18, 0, 17)
	probe("target", "frame", "next-loader", 19, 1, 19)
	probe("next-target", "frame", "next-loader", 20, 1, 20)
	probe("next-target", "next-frame", "next-loader", 21, 1, 21)
	f := &snapshotCaller{frame: "next-frame", loader: "next-loader", evalError: errors.New("Cannot find context with specified id")}
	if _, err := inspectTargetCachedOn(f, TargetProbe{}, func() error { return nil }, &cache, "next-target"); err == nil {
		t.Fatal("lost context was accepted")
	}
	probe("next-target", "next-frame", "next-loader", 22, 1, 22)
	probe("next-target", "next-frame", "", 23, 1, 23)
	probe("next-target", "next-frame", "", 24, 1, 24)
}

func TestFormSnapshotRefusesEvaluateException(t *testing.T) {
	f := &snapshotCaller{frame: "root", world: 17.0, exception: true}
	if _, err := inspectTargetOn(f, TargetProbe{}, func() error { return nil }); err == nil {
		t.Fatal("exception result accepted")
	}
}

func TestFormSnapshotInspectTargetDriverFence(t *testing.T) {
	for _, phase := range []string{"Page.getFrameTree", "Page.createIsolatedWorld", "Runtime.evaluate"} {
		t.Run(phase, func(t *testing.T) {
			f := startLatencyCDP(t, nil)
			m, id := newBudgetSession(t, f)
			defer m.StopAll()
			f.mu.Lock()
			f.callHook = func(method string, params map[string]any) (map[string]any, bool) {
				if method == phase {
					forceHandoff(m, id)
				}
				switch method {
				case "Page.getFrameTree":
					return map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "root", "loaderId": "loader"}}}, true
				case "Page.createIsolatedWorld":
					return map[string]any{"executionContextId": 17.0}, true
				case "Runtime.evaluate":
					return map[string]any{"result": map[string]any{"value": map[string]any{"resolved": true}}}, true
				}
				return nil, false
			}
			f.mu.Unlock()
			if _, err := m.InspectTarget(id, TargetProbe{Kind: "click", Selector: "#submit"}); !errors.Is(err, ErrHandedOff) {
				t.Fatalf("handoff during %s escaped driver fence: %v", phase, err)
			}
		})
	}
}

func TestFormSnapshotRequiresIsolatedWorld(t *testing.T) {
	f := &snapshotCaller{frame: "root", world: 17.0}
	checks := 0
	info, err := inspectTargetOn(f, TargetProbe{Kind: "click", Selector: "#submit"}, func() error { checks++; return nil })
	if err != nil || !info.Resolved || info.ContentHash != "bound" || len(info.FormFiles) != 1 || info.FormFiles[0].Name != "invoice.txt" {
		t.Fatalf("snapshot: %+v %v", info, err)
	}
	if f.params["Page.createIsolatedWorld"]["frameId"] != "root" || f.params["Runtime.evaluate"]["contextId"] != 17 || checks != 4 {
		t.Fatalf("missing frame/context/fences: %+v checks=%d", f.params, checks)
	}
	for _, world := range []any{nil, 0.0, -1.0, 1.5, "17"} {
		bad := &snapshotCaller{frame: "root", world: world}
		if _, err := inspectTargetOn(bad, TargetProbe{}, func() error { return nil }); !errors.Is(err, ErrTargetChanged) || bad.params["Runtime.evaluate"] != nil {
			t.Fatalf("invalid world fell back to default: world=%v err=%v", world, err)
		}
	}
}

func TestFormSnapshotStopsOnReadOrFenceFailure(t *testing.T) {
	for _, method := range []string{"Page.getFrameTree", "Page.createIsolatedWorld", "Runtime.evaluate"} {
		f := &snapshotCaller{frame: "root", world: 17.0, fail: method}
		if _, err := inspectTargetOn(f, TargetProbe{}, func() error { return nil }); err == nil {
			t.Fatal("read failure accepted")
		}
		if f.calls[len(f.calls)-1] != method {
			t.Fatalf("continued after failure: %v", f.calls)
		}
	}
	for failAt := 1; failAt <= 4; failAt++ {
		f := &snapshotCaller{frame: "root", world: 17.0}
		n := 0
		_, err := inspectTargetOn(f, TargetProbe{}, func() error {
			n++
			if n == failAt {
				return ErrHandedOff
			}
			return nil
		})
		if !errors.Is(err, ErrHandedOff) || len(f.calls) != failAt-1 {
			t.Fatalf("continued after fence %d: %v %v", failAt, f.calls, err)
		}
	}
}
