package pointcloudcapture

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

const fixtureRun = "0123456789abcdef0123456789abcdef"

func fixtureConfig() Config {
	return Config{RunID: fixtureRun, Environment: Environment{OS: "linux", Privilege: PrivilegeUnprivileged, Mode: CandidateMode}}
}

type fakeCDP struct {
	t               *testing.T
	initialized     bool
	next            int
	ready           int
	captures        int
	events          []string
	mutate          func(int, map[string]any)
	image           func(int) []byte
	controlErr      error
	captureErr      error
	afterCapture    func()
	gate            <-chan struct{}
	entered         chan<- struct{}
	allowBareRemote bool
}

func (f *fakeCDP) Command(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	f.t.Helper()
	if method == "Runtime.evaluate" {
		if len(params) != 5 || params["returnByValue"] != true || params["awaitPromise"] != true || params["generatePreview"] != false || params["allowUnsafeEvalBlockedByCSP"] != false {
			return nil, errors.New("unexpected evaluation options")
		}
		frame := -1
		want := "window.CitadelPointcloudCaptureTest.init(" + strconv.Quote(fixtureRun) + ")"
		if f.initialized {
			frame = f.next
			want = "window.CitadelPointcloudCaptureTest.step(" + strconv.Quote(fixtureRun) + "," + strconv.Itoa(frame) + ")"
		}
		if params["expression"] != want || f.ready >= 0 {
			return nil, errors.New("unexpected step or unresolved capture")
		}
		if f.controlErr != nil {
			return nil, f.controlErr
		}
		if frame == 0 && f.gate != nil {
			f.entered <- struct{}{}
			select {
			case <-f.gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		f.events = append(f.events, "ready:"+strconv.Itoa(frame))
		f.initialized = true
		if frame >= 0 {
			f.ready = frame
			f.next++
		}
		ack := map[string]any{
			"kind": "pointcloudcapture.synthetic.v1", "run": fixtureRun, "frame": float64(frame), "ready": true,
			"width": float64(320), "height": float64(180), "scale": float64(1),
			"mode": CandidateMode, "renderer": "ANGLE (fixture SwiftShader)",
		}
		remote := map[string]any{"type": "object", "value": ack}
		if !f.allowBareRemote {
			remote["className"], remote["description"] = "Object", "Object"
		}
		result := map[string]any{"result": remote}
		if f.mutate != nil {
			f.mutate(frame, result)
		}
		return result, nil
	}
	if method != "Page.captureScreenshot" || f.ready < 0 || len(params) != 4 || params["format"] != "png" || params["fromSurface"] != true || params["captureBeyondViewport"] != false {
		return nil, errors.New("unexpected capture")
	}
	clip, ok := params["clip"].(map[string]any)
	if !ok || len(clip) != 5 || clip["x"] != 0 || clip["y"] != 0 || clip["width"] != 320 || clip["height"] != 180 || clip["scale"] != 1 {
		return nil, errors.New("unexpected capture clip")
	}
	frame := f.ready
	f.ready = -1
	f.captures++
	f.events = append(f.events, "capture:"+strconv.Itoa(frame))
	if f.captureErr != nil {
		return nil, f.captureErr
	}
	data := stampPNG(f.t, frame)
	if f.image != nil {
		data = f.image(frame)
	}
	if f.afterCapture != nil {
		f.afterCapture()
	}
	return map[string]any{"data": base64.StdEncoding.EncodeToString(data)}, nil
}

func newFake(t *testing.T) *fakeCDP {
	return &fakeCDP{t: t, ready: -1}
}

// Encoder and verifier are deliberately separate fixture implementations. The
// encoder draws top-left cells; the HTML uses bottom-left GL scissor geometry.
// Their agreement is NOT proof that a native CDP screenshot captures HTML paint.
func stampImage(frame int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 320, 180))
	for y := 0; y < 180; y++ {
		for x := 0; x < 320; x++ {
			img.SetNRGBA(x, y, color.NRGBA{A: 255})
		}
	}
	for cell := 0; cell < 18; cell++ {
		white := cell < 2 || cell == 15
		if cell >= 2 && cell < 8 {
			white = frame&(1<<(cell-2)) != 0
		}
		if cell >= 8 && cell < 14 {
			white = frame&(1<<(cell-8)) == 0
		}
		value := uint8(0)
		if white {
			value = 255
		}
		for y := 0; y < 8; y++ {
			for x := cell * 8; x < (cell+1)*8; x++ {
				img.SetNRGBA(x, y, color.NRGBA{R: value, G: value, B: value, A: 255})
			}
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func stampPNG(t *testing.T, frame int) []byte {
	t.Helper()
	return encodePNG(t, stampImage(frame))
}

// This verifier is test-only. Production validates PNG identity/dimensions but
// never examines stamp pixels, exposes a stamp flag or accepts stamp metadata.
func verifyStamp(data []byte) (int, error) {
	if len(data) > 1<<20 {
		return 0, errors.New("stamp payload too large")
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width != 320 || config.Height != 180 {
		return 0, errors.New("stamp dimensions invalid")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return 0, errors.New("stamp PNG invalid")
	}
	var values [18]int
	for cell := range values {
		bit := -1
		for y := 0; y < 8; y++ {
			for x := cell * 8; x < cell*8+8; x++ {
				r, g, b, a := img.At(x, y).RGBA()
				if a != 65535 || r != g || r != b || (r != 0 && r != 65535) {
					return 0, errors.New("stamp pixel is not binary opaque RGB")
				}
				value := int(r / 65535)
				if bit >= 0 && bit != value {
					return 0, errors.New("stamp cell is not uniform")
				}
				bit = value
			}
		}
		values[cell] = bit
	}
	if values[0] != 1 || values[1] != 1 || values[14] != 0 || values[15] != 1 || values[16] != 0 || values[17] != 0 {
		return 0, errors.New("stamp anchors invalid")
	}
	frame := 0
	for bit := 0; bit < 6; bit++ {
		if values[bit+2]+values[bit+8] != 1 {
			return 0, errors.New("stamp complement invalid")
		}
		frame |= values[bit+2] << bit
	}
	if frame >= 60 {
		return 0, errors.New("stamp index outside test")
	}
	return frame, nil
}

func TestSyntheticCaptureOrdersAndVerifiesAllSixtyFrames(t *testing.T) {
	for _, bare := range []bool{false, true} {
		fake := newFake(t)
		fake.allowBareRemote = bare
		seen := 0
		err := Run(context.Background(), fixtureConfig(), fake, func(_ context.Context, frame int, data []byte) error {
			observed, err := verifyStamp(data)
			if err != nil || observed != frame || frame != seen {
				return fmt.Errorf("wrong frame: %d %d %v", frame, observed, err)
			}
			fake.events = append(fake.events, "sink:"+strconv.Itoa(frame))
			seen++
			return nil
		})
		if err != nil || seen != 60 || fake.captures != 60 || fake.next != 60 {
			t.Fatalf("capture = %v; sink=%d capture=%d steps=%d", err, seen, fake.captures, fake.next)
		}
		if len(fake.events) != 181 || fake.events[0] != "ready:-1" {
			t.Fatalf("unexpected transaction count: %#v", fake.events)
		}
		for frame := 0; frame < 60; frame++ {
			for offset, stage := range []string{"ready:", "capture:", "sink:"} {
				if got := fake.events[1+frame*3+offset]; got != stage+strconv.Itoa(frame) {
					t.Fatalf("frame %d stage %s: %s", frame, stage, got)
				}
			}
		}
	}
}

func TestStampGoldenCellsAndIndependentVerifier(t *testing.T) {
	for _, test := range []struct {
		frame int
		cells [18]int
	}{
		{0, [18]int{1, 1, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 1, 1, 0, 1, 0, 0}},
		{1, [18]int{1, 1, 1, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 1, 0, 1, 0, 0}},
		{31, [18]int{1, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 1, 0, 1, 0, 0}},
		{32, [18]int{1, 1, 0, 0, 0, 0, 0, 1, 1, 1, 1, 1, 1, 0, 0, 1, 0, 0}},
		{59, [18]int{1, 1, 1, 1, 0, 1, 1, 1, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0}},
	} {
		img := stampImage(test.frame)
		for cell, want := range test.cells {
			got := img.NRGBAAt(cell*8+4, 4)
			value := uint8(want * 255)
			if got != (color.NRGBA{R: value, G: value, B: value, A: 255}) {
				t.Fatalf("golden frame%d cell%d = %#v", test.frame, cell, got)
			}
		}
		observed, err := verifyStamp(encodePNG(t, img))
		if err != nil || observed != test.frame {
			t.Fatalf("golden frame%d decoded %d %v", test.frame, observed, err)
		}
	}
}

func TestControlRefusalsPrecedeAnyFrameCapture(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any, map[string]any, map[string]any)
	}{
		{"exception", func(res, _, _ map[string]any) { res["exceptionDetails"] = map[string]any{"text": "private diagnostic"} }},
		{"extra wrapper", func(_, remote, _ map[string]any) { remote["objectId"] = "unexpected" }},
		{"wrong remote type", func(_, remote, _ map[string]any) { remote["type"] = "string" }},
		{"wrong class", func(_, remote, _ map[string]any) { remote["className"] = "Other" }},
		{"oversized description", func(_, remote, _ map[string]any) { remote["description"] = strings.Repeat("x", 4097) }},
		{"extra ack", func(_, _, ack map[string]any) { ack["points"] = []any{1, 2, 3} }},
		{"missing ack", func(_, _, ack map[string]any) { delete(ack, "run") }},
		{"wrong kind", func(_, _, ack map[string]any) { ack["kind"] = "other.synthetic.v1" }},
		{"wrong run", func(_, _, ack map[string]any) { ack["run"] = "ffffffffffffffffffffffffffffffff" }},
		{"stale index", func(_, _, ack map[string]any) { ack["frame"] = float64(-1) }},
		{"skipped index", func(_, _, ack map[string]any) { ack["frame"] = float64(1) }},
		{"wrong number type", func(_, _, ack map[string]any) { ack["frame"] = 0 }},
		{"nonfinite", func(_, _, ack map[string]any) { ack["frame"] = math.NaN() }},
		{"not ready", func(_, _, ack map[string]any) { ack["ready"] = false }},
		{"wrong width", func(_, _, ack map[string]any) { ack["width"] = float64(640) }},
		{"wrong height", func(_, _, ack map[string]any) { ack["height"] = float64(360) }},
		{"wrong scale", func(_, _, ack map[string]any) { ack["scale"] = float64(2) }},
		{"wrong mode", func(_, _, ack map[string]any) { ack["mode"] = "gpu" }},
		{"unknown renderer", func(_, _, ack map[string]any) { ack["renderer"] = "hardware renderer" }},
		{"changed renderer", func(_, _, ack map[string]any) { ack["renderer"] = "ANGLE (different SwiftShader)" }},
		{"oversized renderer", func(_, _, ack map[string]any) { ack["renderer"] = "ANGLE SwiftShader" + strings.Repeat("x", 257) }},
		{"renderer control bytes", func(_, _, ack map[string]any) { ack["renderer"] = "ANGLE SwiftShader\nprivate" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFake(t)
			fake.mutate = func(frame int, result map[string]any) {
				if frame == 0 {
					remote := result["result"].(map[string]any)
					test.mutate(result, remote, remote["value"].(map[string]any))
				}
			}
			err := Run(context.Background(), fixtureConfig(), fake, func(context.Context, int, []byte) error { return nil })
			if !errors.Is(err, ErrControl) || fake.captures != 0 || strings.Contains(err.Error(), "private") {
				t.Fatalf("control refusal = %v, captures%d", err, fake.captures)
			}
		})
	}
}

func TestRefusalOnlyPreflightDoesNotCallTransport(t *testing.T) {
	for _, change := range []func(*Config){
		func(c *Config) { c.Environment.Privilege = PrivilegeElevated },
		func(c *Config) { c.Environment.Privilege = PrivilegeUnknown },
		func(c *Config) { c.Environment.Privilege = "" },
		func(c *Config) { c.Environment.Mode = "gpu-fallback" },
		func(c *Config) { c.Environment.OS = "darwin" },
		func(c *Config) { c.RunID = "" },
		func(c *Config) { c.RunID = "0123456789abcdef0123456789abcdeF" },
		func(c *Config) { c.RunID = `0123456789abcdef0123456789abcde"` },
	} {
		config := fixtureConfig()
		change(&config)
		fake := newFake(t)
		err := Run(context.Background(), config, fake, func(context.Context, int, []byte) error { return nil })
		if !errors.Is(err, ErrPreflight) || len(fake.events) != 0 {
			t.Fatalf("invalid assertions reached CDP: %v %#v", err, fake.events)
		}
	}
}

func TestStepResolutionPrecedesCaptureAndCancellationStopsIt(t *testing.T) {
	for _, cancelBlocked := range []bool{false, true} {
		fake := newFake(t)
		entered, release := make(chan struct{}, 1), make(chan struct{})
		fake.entered, fake.gate = entered, release
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- Run(ctx, fixtureConfig(), fake, func(context.Context, int, []byte) error { return nil })
		}()
		select {
		case <-entered:
		case <-time.After(15 * time.Second):
			cancel()
			t.Fatal("fake Step was not entered")
		}
		if fake.captures != 0 {
			cancel()
			t.Fatal("screenshot began before Step resolution")
		}
		if cancelBlocked {
			cancel()
		} else {
			close(release)
		}
		select {
		case err := <-done:
			if cancelBlocked {
				if !errors.Is(err, context.Canceled) || fake.captures != 0 {
					t.Fatalf("blocked cancellation = %v captures%d", err, fake.captures)
				}
			} else if err != nil || fake.captures != 60 {
				t.Fatalf("resolved Step capture = %v count%d", err, fake.captures)
			}
		case <-time.After(15 * time.Second):
			cancel()
			t.Fatal("capture did not resolve after gate/cancellation")
		}
		cancel()
	}
}

func TestStaticStageErrorsAndCancellationDoNotLeakPeerDiagnostics(t *testing.T) {
	for _, stage := range []string{"control", "capture", "sink"} {
		t.Run(stage, func(t *testing.T) {
			fake := newFake(t)
			peer := errors.New("private peer diagnostic and credential")
			sink := FrameSink(func(context.Context, int, []byte) error { return nil })
			want, captures := ErrControl, 0
			switch stage {
			case "control":
				fake.controlErr = peer
			case "capture":
				fake.captureErr = peer
				want, captures = ErrScreenshot, 1
			case "sink":
				sink = func(context.Context, int, []byte) error { return peer }
				want, captures = ErrSink, 1
			}
			err := Run(context.Background(), fixtureConfig(), fake, sink)
			if !errors.Is(err, want) || fake.captures != captures || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "credential") {
				t.Fatalf("stage error = %v captures%d", err, fake.captures)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	fake := newFake(t)
	seen := 0
	err := Run(ctx, fixtureConfig(), fake, func(context.Context, int, []byte) error {
		seen++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || seen != 1 || fake.captures != 1 || fake.next != 1 {
		t.Fatalf("sink cancellation = %v count%d captures%d next%d", err, seen, fake.captures, fake.next)
	}
	err = Run(context.Background(), fixtureConfig(), nil, func(context.Context, int, []byte) error { return nil })
	if !errors.Is(err, ErrPreflight) {
		t.Fatalf("nil transport = %v", err)
	}
	err = Run(context.Background(), fixtureConfig(), newFake(t), nil)
	if !errors.Is(err, ErrPreflight) {
		t.Fatalf("nil sink = %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	fake = newFake(t)
	err = Run(ctx, fixtureConfig(), fake, func(context.Context, int, []byte) error { return nil })
	if !errors.Is(err, context.Canceled) || len(fake.events) != 0 {
		t.Fatalf("pre-cancelled run reached transport: %v %#v", err, fake.events)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	fake = newFake(t)
	fake.afterCapture = cancel
	seen = 0
	err = Run(ctx, fixtureConfig(), fake, func(context.Context, int, []byte) error {
		seen++
		return nil
	})
	if !errors.Is(err, context.Canceled) || fake.captures != 1 || seen != 0 {
		t.Fatalf("capture cancellation reached sink: %v captures%d sink%d", err, fake.captures, seen)
	}
}

func TestStampRejectsStaleRepeatedSkippedAndAckDisagreement(t *testing.T) {
	for _, mode := range []string{"stale", "repeated", "skipped"} {
		t.Run(mode, func(t *testing.T) {
			fake := newFake(t)
			fake.image = func(frame int) []byte {
				switch mode {
				case "stale":
					return stampPNG(t, 59)
				case "repeated":
					return stampPNG(t, 0)
				default:
					return stampPNG(t, frame+1)
				}
			}
			verified := 0
			err := Run(context.Background(), fixtureConfig(), fake, func(_ context.Context, frame int, data []byte) error {
				observed, err := verifyStamp(data)
				if err != nil || observed != frame {
					return errors.New("wrong stamped index")
				}
				verified++
				return nil
			})
			want := 0
			if mode == "repeated" {
				want = 1
			}
			if !errors.Is(err, ErrSink) || verified != want || fake.captures != want+1 {
				t.Fatalf("stamp refusal = %v verified%d captures%d", err, verified, fake.captures)
			}
		})
	}
	// A permissive sink does NOT turn Run into a stamp verifier. This control
	// pins the separation between production PNG handling and test evidence.
	fake := newFake(t)
	fake.image = func(int) []byte { return stampPNG(t, 0) }
	if err := Run(context.Background(), fixtureConfig(), fake, func(context.Context, int, []byte) error { return nil }); err != nil || fake.captures != 60 {
		t.Fatalf("Run incorrectly claimed stamp verification: %v", err)
	}
}

func TestStampVerifierRejectsOrientationAnchorsAndNonbinaryPixels(t *testing.T) {
	for _, mode := range []string{"mirrored", "vertical_flip", "absent", "anchor", "complement", "nonbinary", "alpha", "index_outside"} {
		t.Run(mode, func(t *testing.T) {
			img := stampImage(31)
			switch mode {
			case "mirrored", "vertical_flip":
				flipped := image.NewNRGBA(img.Bounds())
				for y := 0; y < 180; y++ {
					for x := 0; x < 320; x++ {
						if mode == "mirrored" {
							flipped.SetNRGBA(x, y, img.NRGBAAt(319-x, y))
						} else {
							flipped.SetNRGBA(x, y, img.NRGBAAt(x, 179-y))
						}
					}
				}
				img = flipped
			case "absent":
				img = image.NewNRGBA(img.Bounds())
				for y := 0; y < 180; y++ {
					for x := 0; x < 320; x++ {
						img.SetNRGBA(x, y, color.NRGBA{A: 255})
					}
				}
			case "anchor":
				for y := 0; y < 8; y++ {
					for x := 0; x < 8; x++ {
						img.SetNRGBA(x, y, color.NRGBA{A: 255})
					}
				}
			case "complement":
				for y := 0; y < 8; y++ {
					for x := 64; x < 72; x++ {
						img.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
					}
				}
			case "nonbinary":
				for y := 0; y < 8; y++ {
					for x := 112; x < 120; x++ {
						img.SetNRGBA(x, y, color.NRGBA{R: 127, G: 127, B: 127, A: 255})
					}
				}
			case "alpha":
				for y := 0; y < 8; y++ {
					for x := 112; x < 120; x++ {
						img.SetNRGBA(x, y, color.NRGBA{A: 254})
					}
				}
			case "index_outside":
				img = stampImage(63)
			}
			if _, err := verifyStamp(encodePNG(t, img)); err == nil {
				t.Fatal("invalid stamp accepted")
			}
		})
	}
}

func TestScreenshotPNGSizeDimensionsAndMalformedPayloadRefusals(t *testing.T) {
	valid := stampPNG(t, 0)
	for _, test := range []struct {
		name   string
		result map[string]any
		want   error
	}{
		{"missing", map[string]any{}, ErrScreenshot},
		{"wrong type", map[string]any{"data": 123}, ErrScreenshot},
		{"extra field", map[string]any{"data": base64.StdEncoding.EncodeToString(valid), "extra": true}, ErrScreenshot},
		{"malformed base64", map[string]any{"data": "!!!!"}, ErrScreenshot},
		{"non PNG", map[string]any{"data": "eA=="}, ErrScreenshot},
		{"truncated PNG", map[string]any{"data": base64.StdEncoding.EncodeToString(valid[:len(valid)/2])}, ErrScreenshot},
		{"encoded oversized syntax", map[string]any{"data": strings.Repeat("!", base64.StdEncoding.EncodedLen(MaxPNGBytes)+1)}, ErrScreenshotSize},
		{"encoded oversized newline syntax", map[string]any{"data": strings.Repeat("\n", base64.StdEncoding.EncodedLen(MaxPNGBytes)) + "!"}, ErrScreenshotSize},
		{"same encoded oversized syntax", map[string]any{"data": strings.Repeat("!", base64.StdEncoding.EncodedLen(MaxPNGBytes))}, ErrScreenshotSize},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := screenshot(test.result)
			if data != nil || !errors.Is(err, test.want) {
				t.Fatalf("PNG refusal = %v bytes%d", err, len(data))
			}
		})
	}
	for _, dimensions := range []image.Rectangle{
		image.Rect(0, 0, 319, 180), image.Rect(0, 0, 320, 179), image.Rect(0, 0, 1<<20, 1),
	} {
		// A valid wide one-row PNG is a safe dimension-bomb regression: removing
		// the dimension guard would accept it, without risking a huge raster.
		data := encodePNG(t, image.NewNRGBA(dimensions))
		got, err := screenshot(map[string]any{"data": base64.StdEncoding.EncodeToString(data)})
		if got != nil || !errors.Is(err, ErrScreenshot) {
			t.Fatalf("wrong dimensions %v = %v", dimensions, err)
		}
	}
	// Exact-limit base64 with '==' must pass the size preflight and reach PNG
	// validation. These bytes intentionally are not a PNG, so the typed reason
	// distinguishes padding correctness from an oversized-size refusal.
	exact := base64.StdEncoding.EncodeToString(make([]byte, MaxPNGBytes))
	got, err := screenshot(map[string]any{"data": exact})
	if got != nil || !errors.Is(err, ErrScreenshot) || errors.Is(err, ErrScreenshotSize) {
		t.Fatalf("exact-limit padding rejected as size: %v", err)
	}
	over := base64.StdEncoding.EncodeToString(make([]byte, MaxPNGBytes+1))
	if len(over) != len(exact) {
		t.Fatal("one-over fixture must share encoded ceiling with exact limit")
	}
	got, err = screenshot(map[string]any{"data": over})
	if got != nil || !errors.Is(err, ErrScreenshotSize) {
		t.Fatalf("same-encoded valid base64 one-over passed size preflight: %v", err)
	}
}
