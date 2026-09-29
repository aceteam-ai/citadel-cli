package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
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

func TestWithEnvOverridesRemovesEarlierValues(t *testing.T) {
	got := withEnvOverrides([]string{"PATH=/bin", "HOME=/real", "home=/also-real"}, "HOME=/isolated", "TMP=/isolated")
	if !reflect.DeepEqual(got, []string{"PATH=/bin", "HOME=/isolated", "TMP=/isolated"}) {
		t.Fatalf("env = %v", got)
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

func (p *fakePapercraftPage) Close() error { p.closed = true; return nil }

type fakePapercraftEncoder struct {
	cfg      papercraftEncoderConfig
	frames   int
	finished bool
	aborted  bool
}

func (e *fakePapercraftEncoder) WriteFrame([]byte) error { e.frames++; return nil }
func (e *fakePapercraftEncoder) Abort()                  { e.aborted = true }
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
		startPage: func(_ context.Context, _ string, _, _ int, _ string) (papercraftPage, error) {
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
		startPage: func(context.Context, string, int, int, string) (papercraftPage, error) {
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
