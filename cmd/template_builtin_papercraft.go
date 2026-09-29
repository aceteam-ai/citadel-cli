package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
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

var errChromiumShutdownIncomplete = errors.New("Chromium process tree shutdown incomplete")

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
	removeAll    func(string) error
}

func init() {
	RegisterBuiltinTemplateRunner(papercraftBuiltinName, func(ctx context.Context, params json.RawMessage, inputs []string, outDir string) ([]string, error) {
		return runPapercraftRenderBuiltin(ctx, params, inputs, outDir, livePapercraftRenderDeps())
	})
}

func livePapercraftRenderDeps() papercraftRenderDeps {
	return papercraftRenderDeps{startPage: startChromiumRenderPage, startEncoder: startPapercraftFFmpeg, removeAll: os.RemoveAll}
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
		if err := removePapercraftProfile(profileDir, deps.removeAll); err != nil {
			return nil, fmt.Errorf("reset Chromium profile: %w", err)
		}
		if err := os.MkdirAll(profileDir, 0o700); err != nil {
			return nil, fmt.Errorf("create Chromium profile: %w", err)
		}
		page, err := deps.startPage(ctx, format.html, format.width, format.height, profileDir)
		if err != nil {
			var cleanupErr error
			if !errors.Is(err, errChromiumShutdownIncomplete) {
				cleanupErr = removePapercraftProfile(profileDir, deps.removeAll)
			}
			return nil, errors.Join(
				fmt.Errorf("start %s Chromium render: %w", format.name, err),
				cleanupErr,
			)
		}

		durationValue, evalErr := page.Evaluate(ctx, "window.DUR")
		duration, durationErr := finiteFloat(durationValue)
		if evalErr != nil || durationErr != nil || duration <= 0 || duration > papercraftMaxDuration {
			var durationFailure error
			if evalErr != nil {
				durationFailure = fmt.Errorf("read %s window.DUR: %w", format.name, evalErr)
			} else if durationErr != nil {
				durationFailure = fmt.Errorf("read %s window.DUR: %w", format.name, durationErr)
			} else {
				durationFailure = fmt.Errorf("%s window.DUR must be greater than 0 and at most %d", format.name, papercraftMaxDuration)
			}
			return nil, errors.Join(durationFailure, cleanupPapercraftRender(page, profileDir, deps.removeAll))
		}
		if hasDuration && math.Abs(duration-wantDuration) > 1.0/papercraftFPS {
			return nil, errors.Join(
				fmt.Errorf("%s window.DUR %.6f disagrees with duration_seconds %.6f", format.name, duration, wantDuration),
				cleanupPapercraftRender(page, profileDir, deps.removeAll),
			)
		}
		if hasDuration {
			duration = wantDuration
		}

		frameCount := int(math.Ceil(duration * papercraftFPS))
		encodedDuration := float64(frameCount) / papercraftFPS
		outputPath := filepath.Join(outDir, format.outputName)
		if err := removeStaleBuiltinOutput(outputPath); err != nil {
			return nil, errors.Join(err, cleanupPapercraftRender(page, profileDir, deps.removeAll))
		}
		encoder, err := deps.startEncoder(ctx, papercraftEncoderConfig{
			Width: format.width, Height: format.height, FPS: papercraftFPS,
			DurationSeconds: encodedDuration, AudioPath: audio, OutputPath: outputPath,
		})
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("start %s ffmpeg encode: %w", format.name, err),
				cleanupPapercraftRender(page, profileDir, deps.removeAll),
			)
		}

		renderErr := renderPapercraftFrames(ctx, page, encoder, frameCount)
		if renderErr == nil {
			renderErr = encoder.Finish()
		} else {
			encoder.Abort()
		}
		cleanupErr := cleanupPapercraftRender(page, profileDir, deps.removeAll)
		if combined := errors.Join(renderErr, cleanupErr); combined != nil {
			return nil, fmt.Errorf("render %s: %w", format.name, combined)
		}
		info, err := os.Stat(outputPath)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, fmt.Errorf("ffmpeg reported success but did not write a non-empty %s", format.outputName)
		}
		outputs = append(outputs, format.outputName)
	}
	return outputs, nil
}

func cleanupPapercraftRender(page papercraftPage, profileDir string, removeAll func(string) error) error {
	if page != nil {
		if err := page.Close(); err != nil {
			// A failed close means the browser process tree may still hold profile
			// files. Leave the directory in place rather than racing a live child.
			return fmt.Errorf("close Chromium render before profile removal: %w", err)
		}
	}
	return removePapercraftProfile(profileDir, removeAll)
}

func removePapercraftProfile(profileDir string, removeAll func(string) error) error {
	if removeAll == nil {
		removeAll = os.RemoveAll
	}
	if err := removeAll(profileDir); err != nil {
		return fmt.Errorf("remove Chromium profile %q: %w", profileDir, err)
	}
	return nil
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
	cdp     *cobrowsestream.Client
	process chromiumProcessControl
}

type chromiumProcessControl interface {
	Done() <-chan struct{}
	WaitErr() error
	Diagnostic() string
	Shutdown() error
}

type execChromiumProcess struct {
	cancel        context.CancelFunc
	terminateTree func() error
	done          chan struct{}
	stderr        *bytes.Buffer
	waitErr       error
	stopOnce      sync.Once
	stopErr       error
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
	args := papercraftChromiumArgs(profileDir, width, height, port, runtime.GOOS, platform.IsRoot())
	env := withEnvOverrides(os.Environ(),
		"HOME="+profileDir,
		"XDG_CACHE_HOME="+filepath.Join(profileDir, "cache"),
		"XDG_CONFIG_HOME="+filepath.Join(profileDir, "config"),
		"TMPDIR="+profileDir, "TMP="+profileDir, "TEMP="+profileDir,
	)
	process, err := startChromiumProcess(ctx, chrome, args, env)
	if err != nil {
		return nil, fmt.Errorf("start Chromium: %w", err)
	}
	cdp, err := waitForChromiumCDP(ctx, process, func() (*cobrowsestream.Client, error) {
		return cobrowsestream.DialCDP(port)
	}, 30*time.Second, 50*time.Millisecond)
	if err != nil {
		return nil, err
	}
	p := &chromiumRenderPage{cdp: cdp, process: process}
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
			return nil, errors.Join(err, p.Close())
		}
	}
	pageURL, err := papercraftFileURL(html, runtime.GOOS)
	if err != nil {
		return nil, errors.Join(err, p.Close())
	}
	if _, err := cdp.Command(ctx, "Page.navigate", map[string]any{"url": pageURL}); err != nil {
		return nil, errors.Join(err, p.Close())
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
			return nil, errors.Join(ctx.Err(), p.Close())
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil, errors.Join(fmt.Errorf("timed out waiting for window.READY"), p.Close())
}

func startChromiumProcess(ctx context.Context, binary string, args, env []string) (*execChromiumProcess, error) {
	procCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(procCtx, binary, args...)
	stderr := &bytes.Buffer{}
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	terminateTree := configurePapercraftProcessTree(cmd)
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	process := &execChromiumProcess{
		cancel:        cancel,
		terminateTree: terminateTree,
		done:          make(chan struct{}),
		stderr:        stderr,
	}
	go func() {
		process.waitErr = cmd.Wait()
		close(process.done)
	}()
	return process, nil
}

func (p *execChromiumProcess) Done() <-chan struct{} { return p.done }

func (p *execChromiumProcess) WaitErr() error {
	<-p.done
	return p.waitErr
}

func (p *execChromiumProcess) Diagnostic() string {
	return compactCommandOutput(p.stderr.Bytes())
}

func (p *execChromiumProcess) Shutdown() error {
	p.stopOnce.Do(func() {
		// Do not rely on exec.Cmd.Cancel alone. os/exec stops watching the
		// command context after the root process exits, while Chromium helpers
		// in its process group may still be alive. Explicit termination here is
		// therefore required even when Done is already closed.
		terminateErr := p.terminateTree()
		p.cancel()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			p.stopErr = errors.Join(
				terminateErr,
				fmt.Errorf("%w: timed out waiting for Chromium process tree to exit", errChromiumShutdownIncomplete),
			)
			return
		}
		p.stopErr = terminateErr
	})
	return p.stopErr
}

func waitForChromiumCDP(
	ctx context.Context,
	process chromiumProcessControl,
	dial func() (*cobrowsestream.Client, error),
	timeout, retry time.Duration,
) (*cobrowsestream.Client, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	var lastErr error
	for {
		cdp, err := dial()
		if err == nil && cdp != nil {
			return cdp, nil
		}
		if err == nil {
			err = fmt.Errorf("CDP dial returned a nil client")
		}
		lastErr = err
		retryTimer := time.NewTimer(retry)
		select {
		case <-process.Done():
			retryTimer.Stop()
			return nil, errors.Join(
				fmt.Errorf("Chromium exited before CDP became ready: %v: %s", process.WaitErr(), process.Diagnostic()),
				process.Shutdown(),
			)
		case <-ctx.Done():
			retryTimer.Stop()
			return nil, errors.Join(ctx.Err(), process.Shutdown())
		case <-deadline.C:
			retryTimer.Stop()
			shutdownErr := process.Shutdown()
			startupErr := fmt.Errorf("Chromium CDP did not become ready: %w", lastErr)
			if shutdownErr == nil {
				startupErr = fmt.Errorf("Chromium CDP did not become ready: %w: %s", lastErr, process.Diagnostic())
			}
			return nil, errors.Join(
				startupErr,
				shutdownErr,
			)
		case <-retryTimer.C:
		}
	}
}

func papercraftChromiumArgs(profileDir string, width, height, port int, goos string, elevated bool) []string {
	args := []string{
		"--headless=new", "--disable-gpu", "--force-color-profile=srgb", "--font-render-hinting=none",
		"--disable-background-networking", "--disable-component-update", "--disable-sync", "--metrics-recording-only",
		"--disable-breakpad", "--disable-crash-reporter", "--crash-dumps-dir=" + filepath.Join(profileDir, "crash"),
		"--host-resolver-rules=MAP * ~NOTFOUND", "--proxy-server=http://127.0.0.1:9", "--proxy-bypass-list=<-loopback>",
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--remote-debugging-address=127.0.0.1", fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir=" + profileDir, "--disk-cache-dir=" + filepath.Join(profileDir, "cache"),
		fmt.Sprintf("--window-size=%d,%d", width, height), "--hide-scrollbars", "--no-first-run", "--no-default-browser-check",
		"about:blank",
	}
	if goos == "linux" && elevated {
		// Chromium refuses to start as root with its setuid sandbox. The worker's
		// approved-template gate remains the code authorization boundary on that
		// Linux deployment shape. Windows administrators and all non-root nodes
		// retain Chromium's sandbox.
		args = append([]string{"--no-sandbox"}, args...)
	}
	return args
}

func papercraftFileURL(path, goos string) (string, error) {
	u := &url.URL{Scheme: "file", RawQuery: "render=1"}
	if goos != "windows" {
		if !strings.HasPrefix(path, "/") {
			return "", fmt.Errorf("local player path is not absolute: %q", path)
		}
		u.Path = path
		return u.String(), nil
	}

	slashPath := strings.ReplaceAll(path, `\`, "/")
	if strings.HasPrefix(slashPath, "//") {
		unc := strings.TrimPrefix(slashPath, "//")
		host, sharePath, ok := strings.Cut(unc, "/")
		if !ok || host == "" || sharePath == "" {
			return "", fmt.Errorf("UNC player path is incomplete: %q", path)
		}
		u.Host = host
		u.Path = "/" + sharePath
		return u.String(), nil
	}
	if len(slashPath) < 3 || slashPath[1] != ':' || slashPath[2] != '/' || !isASCIIAlpha(slashPath[0]) {
		return "", fmt.Errorf("Windows player path is not absolute: %q", path)
	}
	u.Path = "/" + slashPath
	return u.String(), nil
}

func isASCIIAlpha(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
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
	if p.process != nil {
		return p.process.Shutdown()
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
