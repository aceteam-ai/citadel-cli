package cmd

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestPapercraftSceneShellsCarryExactlyOneMarkerEach(t *testing.T) {
	for name, shell := range map[string][]byte{
		"landscape": papercraftSceneShellLandscape,
		"portrait":  papercraftSceneShellPortrait,
	} {
		if n := strings.Count(string(shell), papercraftSceneMarkerScene); n != 1 {
			t.Errorf("%s shell has %d occurrences of %s, want 1", name, n, papercraftSceneMarkerScene)
		}
		if n := strings.Count(string(shell), papercraftSceneMarkerTimings); n != 1 {
			t.Errorf("%s shell has %d occurrences of %s, want 1", name, n, papercraftSceneMarkerTimings)
		}
	}
}

func TestClassifyPapercraftSceneInputs(t *testing.T) {
	dir := t.TempDir()
	scene := filepath.Join(dir, "scene.json")
	timings := filepath.Join(dir, "timings.json")
	audio := filepath.Join(dir, "narration.wav")
	for _, p := range []string{scene, timings, audio} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	gotScene, gotTimings, gotAudio, err := classifyPapercraftSceneInputs([]string{scene, timings, audio})
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if gotScene != scene || gotTimings != timings || gotAudio != audio {
		t.Fatalf("classify = (%q, %q, %q)", gotScene, gotTimings, gotAudio)
	}

	// Audio is optional.
	gotScene, gotTimings, gotAudio, err = classifyPapercraftSceneInputs([]string{scene, timings})
	if err != nil {
		t.Fatalf("classify without audio: %v", err)
	}
	if gotScene != scene || gotTimings != timings || gotAudio != "" {
		t.Fatalf("classify without audio = (%q, %q, %q)", gotScene, gotTimings, gotAudio)
	}

	for name, inputs := range map[string][]string{
		"missing scene.json":       {timings},
		"missing timings.json":     {scene},
		"duplicate scene.json":     {scene, filepath.Join(dir, "sub", "scene.json"), timings},
		"unrecognized extra input": {scene, timings, filepath.Join(dir, "notes.txt")},
		"duplicate audio":          {scene, timings, audio, filepath.Join(dir, "second.mp3")},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "duplicate scene.json" || name == "unrecognized extra input" || name == "duplicate audio" {
				for _, p := range inputs {
					if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, _, _, err := classifyPapercraftSceneInputs(inputs); err == nil {
				t.Fatalf("expected an error for %s", name)
			}
		})
	}
}

func TestValidatePapercraftSceneJSONObject(t *testing.T) {
	if err := validatePapercraftSceneJSONObject([]byte(`{"beats":[]}`), "scene.json"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`not json`, `[1,2,3]`, `"a string"`, `42`} {
		if err := validatePapercraftSceneJSONObject([]byte(bad), "scene.json"); err == nil {
			t.Fatalf("accepted invalid scene.json: %s", bad)
		}
	}
}

func TestInjectPapercraftSceneSubstitutesExactlyTheTwoMarkers(t *testing.T) {
	shell := []byte(`before __SCENE_JSON__ middle __TIMINGS_JSON__ after`)
	out, err := injectPapercraftScene(shell, []byte(`{"a":1}`), []byte(`{"b":2}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `before {"a":1} middle {"b":2} after`
	if string(out) != want {
		t.Fatalf("injected = %q, want %q", out, want)
	}
}

func TestInjectPapercraftSceneFailsClosedOnMarkerCountDrift(t *testing.T) {
	for name, shell := range map[string][]byte{
		"no scene marker":     []byte(`__TIMINGS_JSON__`),
		"two scene markers":   []byte(`__SCENE_JSON__ __SCENE_JSON__ __TIMINGS_JSON__`),
		"no timings marker":   []byte(`__SCENE_JSON__`),
		"two timings markers": []byte(`__SCENE_JSON__ __TIMINGS_JSON__ __TIMINGS_JSON__`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := injectPapercraftScene(shell, []byte(`{}`), []byte(`{}`)); err == nil {
				t.Fatalf("expected an error for %s", name)
			}
		})
	}
}

func TestPapercraftSceneRenderBuiltinDelegatesToPapercraftRenderCodePath(t *testing.T) {
	inDir, outDir := t.TempDir(), t.TempDir()
	scene := filepath.Join(inDir, "scene.json")
	timings := filepath.Join(inDir, "timings.json")
	audio := filepath.Join(inDir, "narration.wav")
	if err := os.WriteFile(scene, []byte(`{"title":"t","beats":[{"id":"b1"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(timings, []byte(`{"b1":[0,1],"total":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audio, []byte("wav"), 0o600); err != nil {
		t.Fatal(err)
	}

	var pages []*fakePapercraftPage
	var encoders []*fakePapercraftEncoder
	deps := papercraftRenderDeps{
		startPage: func(_ context.Context, htmlPath string, _, _ int, _ string, inputs []string) (papercraftPage, error) {
			// The HTML papercraft-render receives must be real, assembled files,
			// not the scene.json/timings.json this builtin consumed.
			body, err := os.ReadFile(htmlPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), papercraftSceneMarkerScene) || strings.Contains(string(body), papercraftSceneMarkerTimings) {
				t.Fatalf("assembled HTML %s still has an unsubstituted marker", htmlPath)
			}
			if !strings.Contains(string(body), `"title":"t"`) {
				t.Fatalf("assembled HTML %s missing injected scene JSON", htmlPath)
			}
			for _, in := range inputs {
				if filepath.Base(in) == "scene.json" || filepath.Base(in) == "timings.json" {
					t.Fatalf("scene.json/timings.json leaked into papercraft-render inputs: %v", inputs)
				}
			}
			p := &fakePapercraftPage{duration: 1}
			pages = append(pages, p)
			return p, nil
		},
		startEncoder: func(_ context.Context, cfg papercraftEncoderConfig) (papercraftEncoder, error) {
			if cfg.AudioPath != audio {
				t.Fatalf("encoder audio path = %q, want %q", cfg.AudioPath, audio)
			}
			e := &fakePapercraftEncoder{cfg: cfg}
			encoders = append(encoders, e)
			return e, nil
		},
	}

	got, err := runPapercraftSceneRenderBuiltin(context.Background(), json.RawMessage(`{}`), []string{scene, timings, audio}, outDir, deps)
	if err != nil {
		t.Fatalf("runPapercraftSceneRenderBuiltin: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"landscape.mp4", "portrait.mp4"}) {
		t.Fatalf("outputs = %v", got)
	}
	if len(pages) != 2 || len(encoders) != 2 {
		t.Fatalf("pages=%d encoders=%d, want 2 each", len(pages), len(encoders))
	}
	for _, name := range []string{"landscape.mp4", "portrait.mp4"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
			t.Errorf("missing output %s: %v", name, err)
		}
	}
}

func TestPapercraftSceneRenderBuiltinRejectsMalformedSceneJSON(t *testing.T) {
	inDir, outDir := t.TempDir(), t.TempDir()
	scene := filepath.Join(inDir, "scene.json")
	timings := filepath.Join(inDir, "timings.json")
	if err := os.WriteFile(scene, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(timings, []byte(`{"total":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := papercraftRenderDeps{
		startPage: func(context.Context, string, int, int, string, []string) (papercraftPage, error) {
			t.Fatal("must not start Chromium on malformed scene.json")
			return nil, nil
		},
	}
	if _, err := runPapercraftSceneRenderBuiltin(context.Background(), json.RawMessage(`{}`), []string{scene, timings}, outDir, deps); err == nil {
		t.Fatal("expected an error for malformed scene.json")
	}
}

func TestPapercraftSceneBuiltinRegistered(t *testing.T) {
	if _, ok := lookupBuiltinTemplateRunner(papercraftSceneBuiltinName); !ok {
		t.Fatalf("builtin %q is not registered", papercraftSceneBuiltinName)
	}
}

// TestPapercraftSceneRenderBuiltinRealChromiumAndFFmpeg invokes the registered
// papercraft-scene-render builtin exactly as citadel work would (live Chromium
// via livePapercraftRenderDeps(), live ffmpeg), on a small self-contained scene
// and a short silent WAV, so it stays CI-portable. It does not start citadel
// work and does not touch any live worker; it calls the registered runner
// function directly, the same way the node's RUN_JOB_TEMPLATE adapter does.
func TestPapercraftSceneRenderBuiltinRealChromiumAndFFmpeg(t *testing.T) {
	if !platform.ChromiumAvailable() {
		t.Skip("Chromium is not installed")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}

	// Chromium's SingletonSocket path (under outDir/.chromium-<format>) is an
	// AF_UNIX socket, whose path has the kernel's ~104-byte limit. t.TempDir()
	// nests under a path derived from this test's own (long) name, which alone
	// can exceed that limit, unrelated to anything under test here. Short,
	// top-level scratch dirs avoid it the same way production's outDir
	// (a node workspace run directory, not a per-test nested path) does.
	inDir, err := os.MkdirTemp("", "pcs-in-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(inDir) })
	outDir, err := os.MkdirTemp("", "pcs-out-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(outDir) })
	scenePath := filepath.Join(inDir, "scene.json")
	timingsPath := filepath.Join(inDir, "timings.json")
	audioPath := filepath.Join(inDir, "narration.wav")

	scene := map[string]any{
		"title":   "Real Render Smoke Test",
		"tagline": "One beat, real Chromium, real ffmpeg.",
		"badge":   "TEST",
		"beats": []map[string]any{
			{
				"id":      "b1",
				"heading": "Hello",
				"bullets": []string{"A real render"},
				"visual":  "idea",
				"accent":  "blue",
			},
		},
	}
	sceneBytes, err := json.Marshal(scene)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scenePath, sceneBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	timings := map[string]any{"b1": []float64{0, 1.2}, "total": 1.2}
	timingsBytes, err := json.Marshal(timings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(timingsPath, timingsBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audioPath, silentWAV(t, 2*time.Second), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := runPapercraftSceneRenderBuiltin(
		context.Background(),
		json.RawMessage(`{}`),
		[]string{scenePath, timingsPath, audioPath},
		outDir,
		livePapercraftRenderDeps(),
	)
	if err != nil {
		t.Fatalf("runPapercraftSceneRenderBuiltin: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"landscape.mp4", "portrait.mp4"}) {
		t.Fatalf("outputs = %v", got)
	}
	for _, name := range got {
		p := filepath.Join(outDir, name)
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Size() < 1000 {
			t.Fatalf("%s is suspiciously small: %d bytes", name, info.Size())
		}
		t.Logf("%s: %d bytes", name, info.Size())
	}
}

// silentWAV builds a minimal valid PCM16 mono WAV of the given duration at
// 8kHz, small enough to keep this test fast while still giving ffmpeg a real
// audio stream to mux.
func silentWAV(t *testing.T, d time.Duration) []byte {
	t.Helper()
	const sampleRate = 8000
	n := int(d.Seconds() * sampleRate)
	data := make([]byte, n*2)
	var buf strings.Builder
	buf.WriteString("RIFF")
	writeLE32(&buf, uint32(36+len(data)))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	writeLE32(&buf, 16)
	writeLE16(&buf, 1)
	writeLE16(&buf, 1)
	writeLE32(&buf, sampleRate)
	writeLE32(&buf, sampleRate*2)
	writeLE16(&buf, 2)
	writeLE16(&buf, 16)
	buf.WriteString("data")
	writeLE32(&buf, uint32(len(data)))
	out := []byte(buf.String())
	return append(out, data...)
}

func writeLE32(buf *strings.Builder, v uint32) {
	for i := 0; i < 4; i++ {
		buf.WriteByte(byte(v >> (8 * i)))
	}
}

func writeLE16(buf *strings.Builder, v uint16) {
	for i := 0; i < 2; i++ {
		buf.WriteByte(byte(v >> (8 * i)))
	}
}
