package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowsestream"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

const (
	papercraftBuiltinName = "papercraft-render"
	papercraftFPS         = 30
	papercraftCaptureFPS  = 10
	papercraftMaxDuration = 60 * 60
	papercraftMaxHTML     = 128 << 20
	papercraftMaxPNG      = 32 << 20
)

type papercraftFormat struct {
	name       string
	width      int
	height     int
	html       string
	outputName string
}

var requiredPapercraftFormats = []papercraftFormat{
	{name: "landscape", width: 1920, height: 1080, outputName: "landscape.mp4"},
	{name: "portrait", width: 1080, height: 1920, outputName: "portrait.mp4"},
}

type papercraftPage interface {
	Evaluate(context.Context, string) (any, error)
	CapturePNG(context.Context) ([]byte, error)
	Close() error
}

type papercraftEncoder interface {
	WriteFrame([]byte) error
	Finish() error
	Abort()
}

type papercraftEncoderConfig struct {
	Width, Height   int
	FPS             int
	DurationSeconds float64
	AudioPath       string
	OutputPath      string
}

type papercraftRenderDeps struct {
	startPage    func(context.Context, string, int, int, string) (papercraftPage, error)
	startEncoder func(context.Context, papercraftEncoderConfig) (papercraftEncoder, error)
}

func init() {
	RegisterBuiltinTemplateRunner(papercraftBuiltinName, func(ctx context.Context, params json.RawMessage, inputs []string, outDir string) ([]string, error) {
		return runPapercraftRenderBuiltin(ctx, params, inputs, outDir, livePapercraftRenderDeps())
	})
}

func livePapercraftRenderDeps() papercraftRenderDeps {
	return papercraftRenderDeps{startPage: startChromiumRenderPage, startEncoder: startPapercraftFFmpeg}
}

func runPapercraftRenderBuiltin(ctx context.Context, params json.RawMessage, inputs []string, outDir string, deps papercraftRenderDeps) ([]string, error) {
	m, err := decodeBuiltinParams(params, "duration_seconds")
	if err != nil {
		return nil, err
	}
	wantDuration, hasDuration, err := optionalFiniteNumber(m, "duration_seconds")
	if err != nil {
		return nil, err
	}
	if hasDuration && (wantDuration <= 0 || wantDuration > papercraftMaxDuration) {
		return nil, fmt.Errorf("duration_seconds must be greater than 0 and at most %d", papercraftMaxDuration)
	}

	formats, audio, err := classifyPapercraftInputs(inputs)
	if err != nil {
		return nil, err
	}
	outputs := make([]string, 0, len(requiredPapercraftFormats))
	for _, format := range formats {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		profileDir := filepath.Join(outDir, ".chromium-"+format.name)
		if err := os.RemoveAll(profileDir); err != nil {
			return nil, fmt.Errorf("reset Chromium profile: %w", err)
		}
		if err := os.MkdirAll(profileDir, 0o700); err != nil {
			return nil, fmt.Errorf("create Chromium profile: %w", err)
		}
		page, err := deps.startPage(ctx, format.html, format.width, format.height, profileDir)
		if err != nil {
			_ = os.RemoveAll(profileDir)
			return nil, fmt.Errorf("start %s Chromium render: %w", format.name, err)
		}

		durationValue, evalErr := page.Evaluate(ctx, "window.DUR")
		duration, durationErr := finiteFloat(durationValue)
		if evalErr != nil || durationErr != nil || duration <= 0 || duration > papercraftMaxDuration {
			_ = page.Close()
			_ = os.RemoveAll(profileDir)
			if evalErr != nil {
				return nil, fmt.Errorf("read %s window.DUR: %w", format.name, evalErr)
			}
			if durationErr != nil {
				return nil, fmt.Errorf("read %s window.DUR: %w", format.name, durationErr)
			}
			return nil, fmt.Errorf("%s window.DUR must be greater than 0 and at most %d", format.name, papercraftMaxDuration)
		}
		if hasDuration && math.Abs(duration-wantDuration) > 1.0/papercraftFPS {
			_ = page.Close()
			_ = os.RemoveAll(profileDir)
			return nil, fmt.Errorf("%s window.DUR %.6f disagrees with duration_seconds %.6f", format.name, duration, wantDuration)
		}
		if hasDuration {
			duration = wantDuration
		}

		frameCount := int(math.Ceil(duration * papercraftFPS))
		encodedDuration := float64(frameCount) / papercraftFPS
		outputPath := filepath.Join(outDir, format.outputName)
		if err := removeStaleBuiltinOutput(outputPath); err != nil {
			_ = page.Close()
			_ = os.RemoveAll(profileDir)
			return nil, err
		}
		encoder, err := deps.startEncoder(ctx, papercraftEncoderConfig{
			Width: format.width, Height: format.height, FPS: papercraftFPS,
			DurationSeconds: encodedDuration, AudioPath: audio, OutputPath: outputPath,
		})
		if err != nil {
			_ = page.Close()
			_ = os.RemoveAll(profileDir)
			return nil, fmt.Errorf("start %s ffmpeg encode: %w", format.name, err)
		}

		renderErr := renderPapercraftFrames(ctx, page, encoder, frameCount)
		if renderErr == nil {
			renderErr = encoder.Finish()
		} else {
			encoder.Abort()
		}
		closeErr := page.Close()
		_ = os.RemoveAll(profileDir)
		if renderErr != nil {
			return nil, fmt.Errorf("render %s: %w", format.name, renderErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close %s Chromium render: %w", format.name, closeErr)
		}
		info, err := os.Stat(outputPath)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, fmt.Errorf("ffmpeg reported success but did not write a non-empty %s", format.outputName)
		}
		outputs = append(outputs, format.outputName)
	}
	return outputs, nil
}

func renderPapercraftFrames(ctx context.Context, page papercraftPage, encoder papercraftEncoder, frameCount int) error {
	lastKey := -1
	var png []byte
	for frame := 0; frame < frameCount; frame++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		t := float64(frame) / papercraftFPS
		key := int(math.Floor(t*papercraftCaptureFPS + 1e-6))
		if key != lastKey {
			expression := "window.renderFrame(" + strconv.FormatFloat(t, 'f', 9, 64) + ")"
			if _, err := page.Evaluate(ctx, expression); err != nil {
				return fmt.Errorf("frame %d evaluate: %w", frame, err)
			}
			var err error
			png, err = page.CapturePNG(ctx)
			if err != nil {
				return fmt.Errorf("frame %d screenshot: %w", frame, err)
			}
			if len(png) < 8 || !bytes.Equal(png[:8], []byte("\x89PNG\r\n\x1a\n")) {
				return fmt.Errorf("frame %d screenshot is not PNG", frame)
			}
			if len(png) > papercraftMaxPNG {
				return fmt.Errorf("frame %d screenshot exceeds %d bytes", frame, papercraftMaxPNG)
			}
			lastKey = key
		}
		if err := encoder.WriteFrame(png); err != nil {
			return fmt.Errorf("frame %d ffmpeg pipe: %w", frame, err)
		}
	}
	return nil
}

var papercraftStageDimensions = regexp.MustCompile(`body\.render\s+#stage\s*\{\s*width\s*:\s*(\d+)px\s*;\s*height\s*:\s*(\d+)px`)

func classifyPapercraftInputs(inputs []string) ([]papercraftFormat, string, error) {
	formats := append([]papercraftFormat(nil), requiredPapercraftFormats...)
	audio := ""
	for _, input := range inputs {
		ext := strings.ToLower(filepath.Ext(input))
		switch {
		case ext == ".html" || ext == ".htm":
			w, h, err := readPapercraftStageDimensions(input)
			if err != nil {
				return nil, "", err
			}
			matched := false
			for i := range formats {
				if formats[i].width == w && formats[i].height == h {
					if formats[i].html != "" {
						return nil, "", fmt.Errorf("multiple HTML inputs declare the %dx%d %s stage", w, h, formats[i].name)
					}
					formats[i].html = input
					matched = true
					break
				}
			}
			if !matched {
				return nil, "", fmt.Errorf("HTML input %q declares unsupported stage %dx%d", input, w, h)
			}
		case isPapercraftAudioExtension(ext):
			if audio != "" {
				return nil, "", fmt.Errorf("papercraft-render accepts at most one audio input")
			}
			audio = input
		}
	}
	for _, format := range formats {
		if format.html == "" {
			return nil, "", fmt.Errorf("papercraft-render requires one %dx%d %s HTML input", format.width, format.height, format.name)
		}
	}
	return formats, audio, nil
}

func readPapercraftStageDimensions(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("open HTML input %q: %w", path, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, papercraftMaxHTML+1))
	if err != nil {
		return 0, 0, fmt.Errorf("read HTML input %q: %w", path, err)
	}
	if len(b) > papercraftMaxHTML {
		return 0, 0, fmt.Errorf("HTML input %q exceeds %d bytes", path, papercraftMaxHTML)
	}
	m := papercraftStageDimensions.FindSubmatch(b)
	if len(m) != 3 {
		return 0, 0, fmt.Errorf("HTML input %q does not declare body.render #stage dimensions", path)
	}
	w, _ := strconv.Atoi(string(m[1]))
	h, _ := strconv.Atoi(string(m[2]))
	return w, h, nil
}

func isPapercraftAudioExtension(ext string) bool {
	switch ext {
	case ".wav", ".mp3", ".opus", ".ogg", ".flac", ".m4a", ".aac":
		return true
	default:
		return false
	}
}

func optionalFiniteNumber(m map[string]any, key string) (float64, bool, error) {
	v, ok := m[key]
	if !ok {
		return 0, false, nil
	}
	f, err := finiteFloat(v)
	if err != nil {
		return 0, false, fmt.Errorf("%s: %w", key, err)
	}
	return f, true, nil
}

func finiteFloat(v any) (float64, error) {
	var f float64
	var err error
	switch n := v.(type) {
	case json.Number:
		f, err = strconv.ParseFloat(string(n), 64)
	case float64:
		f = n
	default:
		return 0, fmt.Errorf("must be a number")
	}
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, fmt.Errorf("must be a finite number")
	}
	return f, nil
}

type chromiumRenderPage struct {
	cdp    *cobrowsestream.Client
	cancel context.CancelFunc
	wait   <-chan error
	stderr *bytes.Buffer
}

func startChromiumRenderPage(ctx context.Context, html string, width, height int, profileDir string) (papercraftPage, error) {
	chrome, err := platform.FindChromium()
	if err != nil {
		return nil, err
	}
	port, err := reserveLoopbackPort()
	if err != nil {
		return nil, err
	}
	procCtx, cancel := context.WithCancel(ctx)
	args := []string{
		"--headless=new", "--disable-gpu", "--force-color-profile=srgb", "--font-render-hinting=none",
		"--disable-background-networking", "--disable-component-update", "--disable-sync", "--metrics-recording-only",
		"--disable-breakpad", "--disable-crash-reporter", "--crash-dumps-dir=" + filepath.Join(profileDir, "crash"),
		"--host-resolver-rules=MAP * ~NOTFOUND", "--proxy-server=http://127.0.0.1:9", "--proxy-bypass-list=<-loopback>",
		"--remote-debugging-address=127.0.0.1", fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir=" + profileDir, "--disk-cache-dir=" + filepath.Join(profileDir, "cache"),
		fmt.Sprintf("--window-size=%d,%d", width, height), "--hide-scrollbars", "--no-first-run", "--no-default-browser-check",
		"about:blank",
	}
	if platform.IsRoot() {
		// Chromium refuses to start as root with its setuid sandbox. The worker's
		// approved-template gate remains the code authorization boundary on that
		// deployment shape; non-root nodes retain Chromium's sandbox.
		args = append([]string{"--no-sandbox"}, args...)
	}
	cmd := exec.CommandContext(procCtx, chrome, args...)
	stderr := &bytes.Buffer{}
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	cmd.Env = withEnvOverrides(os.Environ(),
		"HOME="+profileDir,
		"XDG_CACHE_HOME="+filepath.Join(profileDir, "cache"),
		"XDG_CONFIG_HOME="+filepath.Join(profileDir, "config"),
		"TMPDIR="+profileDir, "TMP="+profileDir, "TEMP="+profileDir,
	)
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start Chromium: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()

	var cdp *cobrowsestream.Client
	deadline := time.Now().Add(30 * time.Second)
	for cdp == nil && time.Now().Before(deadline) {
		select {
		case err := <-wait:
			cancel()
			return nil, fmt.Errorf("Chromium exited before CDP became ready: %v: %s", err, compactCommandOutput(stderr.Bytes()))
		case <-ctx.Done():
			cancel()
			return nil, ctx.Err()
		default:
		}
		cdp, err = cobrowsestream.DialCDP(port)
		if err != nil {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if cdp == nil {
		cancel()
		return nil, fmt.Errorf("Chromium CDP did not become ready: %w", err)
	}
	p := &chromiumRenderPage{cdp: cdp, cancel: cancel, wait: wait, stderr: stderr}
	for _, command := range []struct {
		method string
		params map[string]any
	}{
		{"Network.enable", nil},
		{"Network.setBlockedURLs", map[string]any{"urls": []string{"http://*", "https://*", "ws://*", "wss://*", "ftp://*"}}},
		{"Page.enable", nil},
		{"Emulation.setDeviceMetricsOverride", map[string]any{"width": width, "height": height, "deviceScaleFactor": 1, "mobile": false}},
	} {
		if _, err := cdp.Command(ctx, command.method, command.params); err != nil {
			_ = p.Close()
			return nil, err
		}
	}
	pageURL := (&url.URL{Scheme: "file", Path: html}).String() + "?render=1"
	if _, err := cdp.Command(ctx, "Page.navigate", map[string]any{"url": pageURL}); err != nil {
		_ = p.Close()
		return nil, err
	}
	readyDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(readyDeadline) {
		v, err := p.Evaluate(ctx, "window.READY === true")
		if err == nil {
			if ready, _ := v.(bool); ready {
				return p, nil
			}
		}
		select {
		case <-ctx.Done():
			_ = p.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	_ = p.Close()
	return nil, fmt.Errorf("timed out waiting for window.READY")
}

func (p *chromiumRenderPage) Evaluate(ctx context.Context, expression string) (any, error) {
	res, err := p.cdp.Command(ctx, "Runtime.evaluate", map[string]any{
		"expression": expression, "returnByValue": true, "awaitPromise": true,
	})
	if err != nil {
		return nil, err
	}
	if details, ok := res["exceptionDetails"]; ok {
		return nil, fmt.Errorf("JavaScript exception: %v", details)
	}
	remote, ok := res["result"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Runtime.evaluate returned no result")
	}
	return remote["value"], nil
}

func (p *chromiumRenderPage) CapturePNG(ctx context.Context) ([]byte, error) {
	res, err := p.cdp.Command(ctx, "Page.captureScreenshot", map[string]any{"format": "png", "fromSurface": true})
	if err != nil {
		return nil, err
	}
	encoded, _ := res["data"].(string)
	if encoded == "" {
		return nil, fmt.Errorf("Page.captureScreenshot returned no data")
	}
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode screenshot: %w", err)
	}
	return b, nil
}

func (p *chromiumRenderPage) Close() error {
	if p.cdp != nil {
		p.cdp.Close()
	}
	p.cancel()
	select {
	case err := <-p.wait:
		if err != nil && !strings.Contains(err.Error(), "killed") && !strings.Contains(err.Error(), "signal") {
			return fmt.Errorf("Chromium exit: %w: %s", err, compactCommandOutput(p.stderr.Bytes()))
		}
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timed out waiting for Chromium to exit")
	}
	return nil
}

func reserveLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve Chromium debug port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

type ffmpegFrameEncoder struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *bytes.Buffer
	done   bool
}

func startPapercraftFFmpeg(ctx context.Context, cfg papercraftEncoderConfig) (papercraftEncoder, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found in PATH: install ffmpeg on the render node")
	}
	args := papercraftFFmpegArgs(cfg)
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}
	return &ffmpegFrameEncoder{cmd: cmd, stdin: stdin, stderr: stderr}, nil
}

func papercraftFFmpegArgs(cfg papercraftEncoderConfig) []string {
	args := []string{
		"-hide_banner", "-nostdin", "-y", "-loglevel", "error",
		"-f", "image2pipe", "-framerate", strconv.Itoa(cfg.FPS), "-c:v", "png", "-i", "-",
	}
	if cfg.AudioPath != "" {
		args = append(args, "-i", cfg.AudioPath, "-map", "0:v", "-map", "1:a")
	} else {
		args = append(args, "-map", "0:v")
	}
	args = append(args,
		"-c:v", "libx264", "-threads", "4", "-preset", "slow", "-crf", "17", "-tune", "animation",
		"-pix_fmt", "yuv420p", "-r", strconv.Itoa(cfg.FPS),
	)
	if cfg.AudioPath != "" {
		args = append(args, "-c:a", "aac", "-b:a", "192k", "-ar", "48000")
	}
	args = append(args, "-t", strconv.FormatFloat(cfg.DurationSeconds, 'f', 4, 64), "-movflags", "+faststart", cfg.OutputPath)
	return args
}

func (e *ffmpegFrameEncoder) WriteFrame(png []byte) error {
	_, err := e.stdin.Write(png)
	return err
}

func (e *ffmpegFrameEncoder) Finish() error {
	if e.done {
		return nil
	}
	e.done = true
	if err := e.stdin.Close(); err != nil {
		_ = e.cmd.Process.Kill()
		_ = e.cmd.Wait()
		return err
	}
	if err := e.cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg exited: %w: %s", err, compactCommandOutput(e.stderr.Bytes()))
	}
	return nil
}

func (e *ffmpegFrameEncoder) Abort() {
	if e.done {
		return
	}
	e.done = true
	_ = e.stdin.Close()
	_ = e.cmd.Process.Kill()
	_ = e.cmd.Wait()
}
