package platform

import (
	"errors"
	"testing"
)

type snapshotCaller struct {
	calls  []string
	params map[string]map[string]any
	world  any
	frame  string
	fail   string
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
		return frameTreeResult([]fakeFrame{{ID: f.frame, URL: "https://form.test"}}), nil
	case "Page.createIsolatedWorld":
		return map[string]any{"executionContextId": f.world}, nil
	case "Runtime.evaluate":
		return map[string]any{"result": map[string]any{"value": map[string]any{"resolved": true, "content_hash": "bound", "form_files": []any{map[string]any{"field": "att", "name": "invoice.txt", "size": 8.0, "type": "text/plain", "last_modified": 100.0}}}}}, nil
	}
	panic("unexpected CDP method")
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
