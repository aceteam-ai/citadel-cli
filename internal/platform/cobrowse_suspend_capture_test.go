package platform

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

type suspendCaptureCaller struct {
	calls  []string
	params []map[string]any
	fail   string
	world  any
	result map[string]any
}

func (c *suspendCaptureCaller) Call(method string, params map[string]any) (map[string]any, error) {
	c.calls = append(c.calls, method)
	c.params = append(c.params, params)
	if method == c.fail {
		return nil, errors.New("private-sentinel")
	}
	switch method {
	case "Page.getFrameTree":
		return map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "main-frame"}}}, nil
	case "Page.createIsolatedWorld":
		return map[string]any{"executionContextId": c.world}, nil
	default:
		return c.result, nil
	}
}
func suspendCaptureFixture() *suspendCaptureCaller {
	return &suspendCaptureCaller{world: float64(7), result: map[string]any{"result": map[string]any{"value": map[string]any{
		"url": "https://example.invalid/path?token=private-sentinel", "title": "Page", "x": float64(-3), "y": float64(25),
	}}}}
}
func TestSuspendCaptureIsolatedBoundedRead(t *testing.T) {
	c := suspendCaptureFixture()
	tab, err := readSuspendTabOn(c, func() error { return nil })
	if err != nil || tab.ScrollX != -3 || tab.ScrollY != 25 || !tab.Active || !strings.Contains(tab.URL, "private-sentinel") {
		t.Fatal("capture lost tab data")
	}
	if !reflect.DeepEqual(c.calls, []string{"Page.getFrameTree", "Page.createIsolatedWorld", "Runtime.evaluate"}) {
		t.Fatal("capture was not read-only in the pinned main frame")
	}
	if c.params[1]["frameId"] != "main-frame" || c.params[2]["contextId"] != 7 || c.params[2]["returnByValue"] != true || c.params[2]["expression"] != suspendTabScript {
		t.Fatal("capture did not use the isolated read contract")
	}
	if !strings.Contains(suspendTabScript, "8192") || !strings.Contains(suspendTabScript, "2048") || maxSuspendCDPMessageBytes != 1048576 {
		t.Fatal("capture transport bounds changed")
	}
}
func TestSuspendCaptureFencesEveryAwait(t *testing.T) {
	for failAt := 1; failAt <= 4; failAt++ {
		c := suspendCaptureFixture()
		checks := 0
		_, err := readSuspendTabOn(c, func() error {
			checks++
			if checks == failAt {
				return errors.New("private-sentinel")
			}
			return nil
		})
		if err == nil || strings.Contains(err.Error(), "private-sentinel") || len(c.calls) != failAt-1 {
			t.Fatal("capture continued past a refused fence")
		}
	}
	c := suspendCaptureFixture()
	if _, err := readSuspendTabOn(c, nil); err == nil || len(c.calls) != 0 {
		t.Fatal("capture without a live fence was accepted")
	}
}
func TestSuspendCaptureOpaqueProviderRefusals(t *testing.T) {
	for _, method := range []string{"Page.getFrameTree", "Page.createIsolatedWorld", "Runtime.evaluate"} {
		c := suspendCaptureFixture()
		c.fail = method
		if _, err := readSuspendTabOn(c, func() error { return nil }); err == nil || strings.Contains(err.Error(), "private-sentinel") {
			t.Fatal("provider refusal succeeded or exposed tab data")
		}
	}
}
func TestSuspendCaptureRejectsMalformedValues(t *testing.T) {
	for _, value := range []any{nil, "7", float64(0), float64(-1), math.Inf(1), math.NaN(), float64(1.5), float64(math.MaxInt32) + 1} {
		c := suspendCaptureFixture()
		c.world = value
		if _, err := readSuspendTabOn(c, func() error { return nil }); err == nil || len(c.calls) != 2 {
			t.Fatal("invalid isolated world reached evaluate")
		}
	}
	for _, mutate := range []func(map[string]any){
		func(v map[string]any) { v["url"] = "javascript:alert(1)" },
		func(v map[string]any) { v["url"] = strings.Repeat("x", 8193) },
		func(v map[string]any) { delete(v, "url") },
		func(v map[string]any) { v["title"] = strings.Repeat("x", 2049) },
		func(v map[string]any) { v["title"] = "control\ncharacter" },
		func(v map[string]any) { v["x"] = math.NaN() },
		func(v map[string]any) { v["y"] = math.Inf(1) },
		func(v map[string]any) { v["x"] = float64(math.MaxInt32) + 1 },
		func(v map[string]any) { v["y"] = "25" },
	} {
		c := suspendCaptureFixture()
		v := c.result["result"].(map[string]any)["value"].(map[string]any)
		mutate(v)
		if _, err := readSuspendTabOn(c, func() error { return nil }); err == nil {
			t.Fatal("malformed tab capture accepted")
		}
	}
	for _, result := range []map[string]any{nil, {"result": map[string]any{"value": nil}}, {"exceptionDetails": map[string]any{"text": "private-sentinel"}}} {
		c := suspendCaptureFixture()
		c.result = result
		if _, err := readSuspendTabOn(c, func() error { return nil }); err == nil {
			t.Fatal("null or exceptional evaluate result accepted")
		}
	}
}
func TestSuspendCaptureRequiresDeadlineBeforeConnecting(t *testing.T) {
	if _, err := openSuspendCaptureConn(context.Background(), pin{}); err == nil || !strings.Contains(err.Error(), "finite deadline") {
		t.Fatal("unbounded capture transport accepted")
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := openSuspendCaptureConn(ctx, pin{}); err == nil || !strings.Contains(err.Error(), "deadline expired") {
		t.Fatal("expired capture connected")
	}
}
