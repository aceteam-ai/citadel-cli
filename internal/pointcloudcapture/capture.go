// Package pointcloudcapture is an inactive synthetic capture proposal. It has
// no live CDP adapter, launcher, registration or native admission authority.
package pointcloudcapture

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image/png"
	"strconv"
	"strings"
)

const (
	Width         = 320
	Height        = 180
	FPS           = 30
	Frames        = 60
	MaxPNGBytes   = 1 << 20
	CandidateMode = "angle-swiftshader-webgl2"
	controlKind   = "pointcloudcapture.synthetic.v1"
	host          = "window.CitadelPointcloudCaptureTest"
)

type Privilege string

const (
	PrivilegeUnknown      Privilege = "unknown"
	PrivilegeElevated     Privilege = "elevated"
	PrivilegeUnprivileged Privilege = "unprivileged"
)

// Environment is supplied assertion data, not evidence that a particular
// executable, flag tuple, sandbox or native software renderer has been tested.
type Environment struct {
	OS        string
	Privilege Privilege
	Mode      string
}

type Config struct {
	RunID       string
	Environment Environment
}

// CommandTransport returns the CDP result object, excluding its id envelope.
// It must honor ctx. Maps are already parsed: checks here cannot bound wire
// reads, upstream JSON expansion, transport allocation or aggregate memory.
type CommandTransport interface {
	Command(context.Context, string, map[string]any) (map[string]any, error)
}

type FrameSink func(context.Context, int, []byte) error

var (
	ErrPreflight      = errors.New("synthetic capture preflight refused")
	ErrControl        = errors.New("synthetic capture control refused")
	ErrScreenshot     = errors.New("synthetic capture screenshot refused")
	ErrScreenshotSize = errors.New("synthetic capture screenshot size refused")
	ErrSink           = errors.New("synthetic capture sink failed")
)

// Preflight is refusal only. Success is not permission to launch a browser.
func Preflight(config Config) error {
	if config.Environment.OS != "linux" || config.Environment.Privilege != PrivilegeUnprivileged || config.Environment.Mode != CandidateMode || len(config.RunID) != 32 {
		return ErrPreflight
	}
	for i := 0; i < len(config.RunID); i++ {
		c := config.RunID[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ErrPreflight
		}
	}
	return nil
}

// Run proposes the fixed two-second test path, one transaction at a time. A
// failed/uncertain command aborts; it never retries or silently reuses a frame.
// No supplied error or peer diagnostic is included in returned stage errors.
func Run(ctx context.Context, config Config, transport CommandTransport, sink FrameSink) error {
	if err := Preflight(config); err != nil {
		return err
	}
	if transport == nil || sink == nil {
		return ErrPreflight
	}
	renderer, err := evaluate(ctx, transport, config.RunID, -1, host+".init("+strconv.Quote(config.RunID)+")")
	if err != nil {
		return err
	}
	for frame := 0; frame < Frames; frame++ {
		observed, err := evaluate(ctx, transport, config.RunID, frame, host+".step("+strconv.Quote(config.RunID)+","+strconv.Itoa(frame)+")")
		if err != nil {
			return err
		}
		if observed != renderer {
			return ErrControl
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := transport.Command(ctx, "Page.captureScreenshot", map[string]any{
			"format": "png", "fromSurface": true, "captureBeyondViewport": false,
			"clip": map[string]any{"x": 0, "y": 0, "width": Width, "height": Height, "scale": 1},
		})
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return ErrScreenshot
		}
		image, err := screenshot(result)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sink(ctx, frame, image); err != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			return ErrSink
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

func evaluate(ctx context.Context, transport CommandTransport, run string, frame int, expression string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	result, err := transport.Command(ctx, "Runtime.evaluate", map[string]any{
		"expression": expression, "returnByValue": true, "awaitPromise": true,
		"generatePreview": false, "allowUnsafeEvalBlockedByCSP": false,
	})
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err != nil {
		return "", ErrControl
	}
	if len(result) != 1 {
		return "", ErrControl
	}
	remote, ok := result["result"].(map[string]any)
	if !ok || len(remote) < 2 || len(remote) > 4 || remote["type"] != "object" {
		return "", ErrControl
	}
	for key, value := range remote {
		switch key {
		case "type", "value":
		case "className", "description":
			if value != "Object" {
				return "", ErrControl
			}
		default:
			return "", ErrControl
		}
	}
	ack, ok := remote["value"].(map[string]any)
	if !ok || len(ack) != 9 {
		return "", ErrControl
	}
	for key := range ack {
		switch key {
		case "kind", "run", "frame", "ready", "width", "height", "scale", "mode", "renderer":
		default:
			return "", ErrControl
		}
	}
	if ack["kind"] != controlKind || ack["run"] != run || ack["ready"] != true || ack["mode"] != CandidateMode {
		return "", ErrControl
	}
	for key, want := range map[string]float64{"frame": float64(frame), "width": Width, "height": Height, "scale": 1} {
		value, ok := ack[key].(float64)
		if !ok || value != want {
			return "", ErrControl
		}
	}
	renderer, ok := ack["renderer"].(string)
	if !ok || len(renderer) == 0 || len(renderer) > 256 || !strings.Contains(renderer, "ANGLE") || !strings.Contains(renderer, "SwiftShader") {
		return "", ErrControl
	}
	for i := 0; i < len(renderer); i++ {
		if renderer[i] < 32 || renderer[i] > 126 {
			return "", ErrControl
		}
	}
	return renderer, nil
}

func screenshot(result map[string]any) ([]byte, error) {
	if len(result) != 1 {
		return nil, ErrScreenshot
	}
	encoded, ok := result["data"].(string)
	if !ok || len(encoded) == 0 {
		return nil, ErrScreenshot
	}
	if len(encoded) > base64.StdEncoding.EncodedLen(MaxPNGBytes) {
		return nil, ErrScreenshotSize
	}
	chars := 0
	var previous, last byte
	for i := 0; i < len(encoded); i++ {
		if encoded[i] == '\r' || encoded[i] == '\n' {
			continue
		}
		chars++
		previous, last = last, encoded[i]
	}
	length := base64.StdEncoding.DecodedLen(chars)
	if chars != 0 && chars%4 == 0 && last == '=' {
		length--
		if previous == '=' {
			length--
		}
	}
	if length > MaxPNGBytes {
		return nil, ErrScreenshotSize
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, ErrScreenshot
	}
	if len(data) > MaxPNGBytes {
		return nil, ErrScreenshotSize
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width != Width || config.Height != Height {
		return nil, ErrScreenshot
	}
	// The dimension check precedes full decode, bounding the decoded raster.
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		return nil, ErrScreenshot
	}
	return data, nil
}
