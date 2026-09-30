package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowsestream"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestProductionTemplateBuiltinsRegistered(t *testing.T) {
	for _, name := range []string{papercraftBuiltinName, audioMixBuiltinName} {
		if _, ok := lookupBuiltinTemplateRunner(name); !ok {
			t.Errorf("builtin %q is not registered", name)
		}
	}
}

func TestDecodeBuiltinParamsUsesExactKeys(t *testing.T) {
	if _, err := decodeBuiltinParams(json.RawMessage(`{"duration_seconds":1}`), "duration_seconds"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"Duration_Seconds":1}`, `{"duration_seconds":1,"extra":true}`} {
		if _, err := decodeBuiltinParams(json.RawMessage(raw), "duration_seconds"); err == nil {
			t.Fatalf("accepted noncanonical params %s", raw)
		}
	}
}

func TestMinimalRenderChildEnvDoesNotLeakWorkerCredentials(t *testing.T) {
	t.Setenv("CITADEL_DEVICE_API_TOKEN", "worker-secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "cloud-secret")
	t.Setenv("PATH", "/safe/bin")
	got := minimalRenderChildEnv("/attempt/profile")
	for _, entry := range got {
		if strings.Contains(entry, "worker-secret") || strings.Contains(entry, "cloud-secret") || strings.HasPrefix(entry, "CITADEL_") || strings.HasPrefix(entry, "AWS_") {
			t.Fatalf("render child inherited worker credential: %q", entry)
		}
	}
	for _, want := range []string{"HOME=/attempt/profile", "PATH=/safe/bin", "XDG_CACHE_HOME=/attempt/profile/cache", "TMPDIR=/attempt/profile"} {
		if !containsString(got, want) {
			t.Errorf("minimal env missing %q: %v", want, got)
		}
	}
}

func TestRemoveStaleBuiltinOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stale.mp4")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleBuiltinOutput(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("stale output still exists: %v", err)
	}
}

type fakePapercraftPage struct {
	duration    float64
	evaluations []string
	captures    int
	closed      bool
	closeErr    error
}

func (p *fakePapercraftPage) Evaluate(_ context.Context, expression string) (any, error) {
	p.evaluations = append(p.evaluations, expression)
	if expression == "window.DUR" {
		return p.duration, nil
	}
	return nil, nil
}

func (p *fakePapercraftPage) CapturePNG(context.Context) ([]byte, error) {
	p.captures++
	return []byte("\x89PNG\r\n\x1a\npixels"), nil
}

func (p *fakePapercraftPage) Close() error { p.closed = true; return p.closeErr }

type fakePapercraftEncoder struct {
	cfg      papercraftEncoderConfig
	frames   int
	finished bool
	aborted  bool
}

func (e *fakePapercraftEncoder) WriteFrame([]byte) error { e.frames++; return nil }
func (e *fakePapercraftEncoder) Abort() error            { e.aborted = true; return nil }
func (e *fakePapercraftEncoder) Finish() error {
	e.finished = true
	return os.WriteFile(e.cfg.OutputPath, []byte("mp4"), 0o600)
}

func writePapercraftHTML(t *testing.T, dir, name string, width, height int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	body := "<style>body.render #stage{width:" + strconv.Itoa(width) + "px;height:" + strconv.Itoa(height) + "px}</style>"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPapercraftRenderBuiltinRendersBothFormatsWithTenFPSCaptureDedup(t *testing.T) {
	inDir, outDir := t.TempDir(), t.TempDir()
	landscape := writePapercraftHTML(t, inDir, "index.html", 1920, 1080)
	portrait := writePapercraftHTML(t, inDir, "index_short.html", 1080, 1920)
	audio := filepath.Join(inDir, "mix.wav")
	asset := filepath.Join(inDir, "scene.js")
	for path, body := range map[string]string{audio: "wav", asset: "scene"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var pages []*fakePapercraftPage
	var encoders []*fakePapercraftEncoder
	deps := papercraftRenderDeps{
		startPage: func(_ context.Context, _ string, _, _ int, _ string, _ []string) (papercraftPage, error) {
			p := &fakePapercraftPage{duration: 0.31}
			pages = append(pages, p)
			return p, nil
		},
		startEncoder: func(_ context.Context, cfg papercraftEncoderConfig) (papercraftEncoder, error) {
			e := &fakePapercraftEncoder{cfg: cfg}
			encoders = append(encoders, e)
			return e, nil
		},
	}
	got, err := runPapercraftRenderBuiltin(context.Background(), json.RawMessage(`{"duration_seconds":0.31}`), []string{asset, portrait, audio, landscape}, outDir, deps)
	if err != nil {
		t.Fatalf("runPapercraftRenderBuiltin: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"landscape.mp4", "portrait.mp4"}) {
		t.Fatalf("outputs = %v", got)
	}
	if len(pages) != 2 || len(encoders) != 2 {
		t.Fatalf("pages=%d encoders=%d, want 2 each", len(pages), len(encoders))
	}
	for i := range pages {
		if pages[i].captures != 4 {
			t.Errorf("page %d captures = %d, want 4 distinct 10fps captures", i, pages[i].captures)
		}
		if !pages[i].closed {
			t.Errorf("page %d was not closed", i)
		}
		if encoders[i].frames != 10 || !encoders[i].finished || encoders[i].aborted {
			t.Errorf("encoder %d = frames:%d finished:%v aborted:%v", i, encoders[i].frames, encoders[i].finished, encoders[i].aborted)
		}
		if encoders[i].cfg.AudioPath != audio || encoders[i].cfg.FPS != 30 {
			t.Errorf("encoder %d config = %+v", i, encoders[i].cfg)
		}
	}
	if encoders[0].cfg.Width != 1920 || encoders[0].cfg.Height != 1080 || encoders[1].cfg.Width != 1080 || encoders[1].cfg.Height != 1920 {
		t.Fatalf("unexpected format order/configs: %+v %+v", encoders[0].cfg, encoders[1].cfg)
	}
}

func TestPapercraftRenderBuiltinRejectsMissingFormatAndTimingMismatch(t *testing.T) {
	dir := t.TempDir()
	landscape := writePapercraftHTML(t, dir, "index.html", 1920, 1080)
	if _, _, err := classifyPapercraftInputs([]string{landscape}); err == nil || !strings.Contains(err.Error(), "portrait") {
		t.Fatalf("missing portrait error = %v", err)
	}
	portrait := writePapercraftHTML(t, dir, "short.html", 1080, 1920)
	deps := papercraftRenderDeps{
		startPage: func(context.Context, string, int, int, string, []string) (papercraftPage, error) {
			return &fakePapercraftPage{duration: 2}, nil
		},
		startEncoder: func(context.Context, papercraftEncoderConfig) (papercraftEncoder, error) {
			return nil, errors.New("must not encode")
		},
	}
	_, err := runPapercraftRenderBuiltin(context.Background(), json.RawMessage(`{"duration_seconds":1}`), []string{landscape, portrait}, t.TempDir(), deps)
	if err == nil || !strings.Contains(err.Error(), "disagrees") {
		t.Fatalf("timing mismatch error = %v", err)
	}
}

func TestPapercraftFFmpegArgsAreArgvOnlyAndMatchReferenceCodec(t *testing.T) {
	args := papercraftFFmpegArgs(papercraftEncoderConfig{FPS: 30, DurationSeconds: 2.5, AudioPath: "/tmp/audio.wav", OutputPath: "/tmp/out.mp4"})
	joined := strings.Join(args, " ")
	for _, want := range []string{"-f image2pipe", "-framerate 30", "-c:v libx264", "-preset slow", "-crf 17", "-tune animation", "-pix_fmt yuv420p", "-c:a aac", "-b:a 192k", "+faststart"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	if strings.Contains(joined, "sh -c") || strings.Contains(joined, "/bin/sh") {
		t.Fatalf("shell found in args: %v", args)
	}
}

func TestPapercraftFileURLCrossPlatform(t *testing.T) {
	for _, tc := range []struct {
		name, goos, path, want string
	}{
		{"posix escaping", "linux", "/tmp/a b%/é.html", "file:///tmp/a%20b%25/%C3%A9.html?render=1"},
		{"windows drive", "windows", `C:\workspace\a b%\紙.html`, "file:///C:/workspace/a%20b%25/%E7%B4%99.html?render=1"},
		{"windows UNC", "windows", `\\server\share\a b%\紙.html`, "file://server/share/a%20b%25/%E7%B4%99.html?render=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := papercraftFileURL(tc.path, tc.goos)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("URL = %q, want %q", got, tc.want)
			}
		})
	}
	for _, tc := range []struct{ goos, path string }{
		{"linux", "relative/index.html"},
		{"windows", `workspace\index.html`},
		{"windows", `\\server`},
	} {
		if got, err := papercraftFileURL(tc.path, tc.goos); err == nil {
			t.Fatalf("papercraftFileURL(%q, %q) = %q, want error", tc.path, tc.goos, got)
		}
	}
}

func TestPapercraftChromiumArgsKeepSandboxAndBlockWebRTCUDP(t *testing.T) {
	contains := func(args []string, want string) bool {
		return strings.Contains(" "+strings.Join(args, " ")+" ", " "+want+" ")
	}
	for _, tc := range []struct {
		name          string
		goos          string
		elevated      bool
		wantNoSandbox bool
	}{
		{name: "linux root", goos: "linux", elevated: true, wantNoSandbox: true},
		{name: "linux user", goos: "linux", elevated: false},
		{name: "windows administrator", goos: "windows", elevated: true},
		{name: "darwin root", goos: "darwin", elevated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := papercraftChromiumArgs(`C:\profile`, 1920, 1080, 9222, "http://127.0.0.1:43123", tc.goos, tc.elevated)
			if got := contains(args, "--no-sandbox"); got != tc.wantNoSandbox {
				t.Fatalf("no-sandbox present = %v, want %v: %v", got, tc.wantNoSandbox, args)
			}
			if !contains(args, "--force-webrtc-ip-handling-policy=disable_non_proxied_udp") {
				t.Fatalf("WebRTC direct UDP policy missing: %v", args)
			}
			if !contains(args, "--proxy-server=http://127.0.0.1:43123") || !contains(args, "--proxy-bypass-list=<-loopback>") {
				t.Fatalf("non-forwarding render proxy is not mandatory: %v", args)
			}
		})
	}
}

func TestPapercraftBlocksNonProxySchemesBeforeNavigation(t *testing.T) {
	blocked := papercraftBlockedURLs()
	for _, want := range []string{"file://*", "data:*", "blob:*", "ws://*", "wss://*", "ftp://*"} {
		if !containsString(blocked, want) {
			t.Errorf("blocked URLs = %v, want %q", blocked, want)
		}
	}
}

func TestPapercraftAssetServerConfinesFilesToHTMLRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	html := filepath.Join(root, "index.html")
	asset := filepath.Join(root, "asset.js")
	secret := filepath.Join(outside, "secret.txt")
	for path, body := range map[string]string{html: "html", asset: "asset", secret: "secret"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(secret, filepath.Join(root, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	resolvedRoot, allowed, err := papercraftAssetSet([]string{html, asset})
	if err != nil {
		t.Fatal(err)
	}
	prefix := "/unguessable-token/"
	handler := papercraftAssetHandler(resolvedRoot, prefix, allowed)
	for _, tc := range []struct {
		method     string
		host       string
		path       string
		wantStatus int
		wantBody   string
	}{
		{method: http.MethodGet, host: papercraftAssetHost, path: prefix + "asset.js", wantStatus: http.StatusOK, wantBody: "asset"},
		{method: http.MethodGet, host: papercraftAssetHost, path: prefix + "escape.txt", wantStatus: http.StatusNotFound},
		{method: http.MethodGet, host: papercraftAssetHost, path: "/wrong/asset.js", wantStatus: http.StatusNotFound},
		{method: http.MethodGet, host: "127.0.0.1:6379", path: prefix + "asset.js", wantStatus: http.StatusNotFound},
		{method: http.MethodConnect, host: papercraftAssetHost, path: prefix + "asset.js", wantStatus: http.StatusNotFound},
	} {
		req := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tc.wantStatus || (tc.wantBody != "" && response.Body.String() != tc.wantBody) {
			t.Errorf("GET %s = %d %q, want %d %q", tc.path, response.Code, response.Body.String(), tc.wantStatus, tc.wantBody)
		}
	}
}

func TestPapercraftChromiumCannotLoadBlockedURLSubresources(t *testing.T) {
	if !platform.ChromiumAvailable() {
		t.Skip("Chromium is not installed")
	}
	originalCommand := renderLimitedCommand
	renderLimitedCommand = func(ctx context.Context, env []string, binary string, args ...string) (*renderCommand, error) {
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = append([]string(nil), env...)
		return &renderCommand{cmd: cmd}, nil
	}
	t.Cleanup(func() { renderLimitedCommand = originalCommand })

	root, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret.svg")
	if err := os.WriteFile(secret, []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="7" height="9"><rect width="7" height="9" fill="red"/></svg>`), 0o600); err != nil {
		t.Fatal(err)
	}
	secretURL, err := papercraftFileURL(secret, runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	html := filepath.Join(root, "index.html")
	inlineSVG := `<svg xmlns="http://www.w3.org/2000/svg" width="11" height="13"><rect width="11" height="13" fill="blue"/></svg>`
	body := `<img id="secret"><img id="data"><img id="blob"><script>` +
		`document.getElementById('secret').src=` + strconv.Quote(secretURL) + `;` +
		`document.getElementById('data').src=` + strconv.Quote("data:image/svg+xml,"+inlineSVG) + `;` +
		`document.getElementById('blob').src=URL.createObjectURL(new Blob([` + strconv.Quote(inlineSVG) + `],{type:'image/svg+xml'}));` +
		`window.READY=true</script>`
	if err := os.WriteFile(html, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	profileRoot, err := os.MkdirTemp("/tmp", "papercraft-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profileRoot) })
	page, err := startChromiumRenderPage(context.Background(), html, 100, 100, filepath.Join(profileRoot, "profile"), []string{html})
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("loopback sockets are unavailable: %v", err)
		}
		t.Fatal(err)
	}
	defer page.Close()
	time.Sleep(500 * time.Millisecond)
	for _, id := range []string{"secret", "data", "blob"} {
		width, err := page.Evaluate(context.Background(), `document.getElementById(`+strconv.Quote(id)+`).naturalWidth`)
		if err != nil {
			t.Fatal(err)
		}
		if width != float64(0) {
			t.Fatalf("blocked %s subresource loaded with naturalWidth=%v", id, width)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type fakeChromiumProcessControl struct {
	done        chan struct{}
	shutdown    bool
	shutdownErr error
}

func (p *fakeChromiumProcessControl) Done() <-chan struct{} { return p.done }
func (p *fakeChromiumProcessControl) WaitErr() error        { <-p.done; return nil }
func (p *fakeChromiumProcessControl) Diagnostic() string    { return "fake Chromium" }
func (p *fakeChromiumProcessControl) Shutdown() error {
	p.shutdown = true
	return p.shutdownErr
}

func TestWaitForChromiumCDPCancellationShutsDownAndReportsCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cleanupErr := errors.New("process tree did not reap")
	process := &fakeChromiumProcessControl{done: make(chan struct{}), shutdownErr: cleanupErr}
	_, err := waitForChromiumCDP(ctx, process, func() (*cobrowsestream.Client, error) {
		return nil, errors.New("not ready")
	}, time.Minute, time.Millisecond)
	if !process.shutdown {
		t.Fatal("startup cancellation did not synchronously shut down the Chromium process tree")
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cleanupErr) {
		t.Fatalf("error = %v, want cancellation and cleanup failure", err)
	}
}

func TestPapercraftRenderReportsProfileCleanupFailure(t *testing.T) {
	inDir, outDir := t.TempDir(), t.TempDir()
	landscape := writePapercraftHTML(t, inDir, "index.html", 1920, 1080)
	portrait := writePapercraftHTML(t, inDir, "short.html", 1080, 1920)
	removeCalls := 0
	deps := papercraftRenderDeps{
		startPage: func(context.Context, string, int, int, string, []string) (papercraftPage, error) {
			return &fakePapercraftPage{duration: 0.1}, nil
		},
		startEncoder: func(_ context.Context, cfg papercraftEncoderConfig) (papercraftEncoder, error) {
			return &fakePapercraftEncoder{cfg: cfg}, nil
		},
		removeAll: func(string) error {
			removeCalls++
			if removeCalls == 2 {
				return errors.New("profile locked")
			}
			return nil
		},
	}
	_, err := runPapercraftRenderBuiltin(context.Background(), json.RawMessage(`{}`), []string{landscape, portrait}, outDir, deps)
	if err == nil || !strings.Contains(err.Error(), "profile locked") || !strings.Contains(err.Error(), "remove Chromium profile") {
		t.Fatalf("cleanup error = %v", err)
	}
}

func TestPapercraftCleanupDoesNotRaceUnreapedBrowser(t *testing.T) {
	page := &fakePapercraftPage{closeErr: errChromiumShutdownIncomplete}
	removed := false
	err := cleanupPapercraftRender(page, "profile", func(string) error {
		removed = true
		return nil
	})
	if !errors.Is(err, errChromiumShutdownIncomplete) {
		t.Fatalf("cleanup error = %v, want shutdown-incomplete sentinel", err)
	}
	if removed {
		t.Fatal("profile removal raced an unreaped Chromium process tree")
	}
}

func TestAudioMixBuiltinRunsMeasuredTwoPassLoudnorm(t *testing.T) {
	dir := t.TempDir()
	inputs := []string{filepath.Join(dir, "narration.opus"), filepath.Join(dir, "music.wav"), filepath.Join(dir, "foley.wav")}
	for _, input := range inputs {
		if err := os.WriteFile(input, []byte("audio"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outDir := t.TempDir()
	var calls [][]string
	deps := audioMixDeps{
		lookFFmpeg: func() (string, error) { return "/usr/bin/ffmpeg", nil },
		run: func(_ context.Context, _ string, args []string) ([]byte, error) {
			calls = append(calls, append([]string(nil), args...))
			if len(calls) == 1 {
				return []byte("log\n{\n\"input_i\":\"-18.20\",\"input_tp\":\"-3.10\",\"input_lra\":\"4.00\",\"input_thresh\":\"-28.30\",\"target_offset\":\"0.10\"\n}\n"), nil
			}
			return nil, os.WriteFile(filepath.Join(outDir, audioMixOutput), []byte("wav"), 0o600)
		},
	}
	got, err := runAudioMixBuiltin(context.Background(), json.RawMessage(`{}`), inputs, outDir, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{audioMixOutput}) || len(calls) != 2 {
		t.Fatalf("outputs=%v calls=%d", got, len(calls))
	}
	analysis, encode := strings.Join(calls[0], " "), strings.Join(calls[1], " ")
	for _, text := range []string{"amix=inputs=3", "loudnorm=I=-14:TP=-2:LRA=11", "print_format=json"} {
		if !strings.Contains(analysis, text) {
			t.Errorf("analysis args missing %q: %s", text, analysis)
		}
	}
	for _, text := range []string{"measured_I=-18.20", "measured_TP=-3.10", "linear=true", "pcm_s24le", "48000"} {
		if !strings.Contains(encode, text) {
			t.Errorf("encode args missing %q: %s", text, encode)
		}
	}
}

func TestAudioMixBuiltinRejectsParamsAndSilentStats(t *testing.T) {
	if _, err := decodeBuiltinParams(json.RawMessage(`{"target_lufs":-10}`)); err == nil {
		t.Fatal("audio-mix accepted a target override")
	}
	_, err := parseLoudnormStats([]byte(`{"input_i":"-inf","input_tp":"-inf","input_lra":"0.00","input_thresh":"-70.00","target_offset":"inf"}`))
	if err == nil || !strings.Contains(err.Error(), "not finite") {
		t.Fatalf("silent stats error = %v", err)
	}
}
