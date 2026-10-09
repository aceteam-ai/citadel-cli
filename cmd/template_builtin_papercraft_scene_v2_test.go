package cmd

import (
	"bytes"
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

func TestPapercraftSceneV2ShellsCarryExactlyOneMarkerEach(t *testing.T) {
	for name, shell := range map[string][]byte{
		"landscape": papercraftSceneV2ShellLandscape,
		"portrait":  papercraftSceneV2ShellPortrait,
	} {
		if n := strings.Count(string(shell), papercraftSceneMarkerScene); n != 1 {
			t.Errorf("%s shell has %d occurrences of %s, want 1", name, n, papercraftSceneMarkerScene)
		}
		if n := strings.Count(string(shell), papercraftSceneMarkerTimings); n != 1 {
			t.Errorf("%s shell has %d occurrences of %s, want 1", name, n, papercraftSceneMarkerTimings)
		}
	}
}

// v1 must keep its exact embedded shells: v2 is additive, never a re-vendor of v1.
func TestPapercraftSceneV1ShellsAreDistinctFromV2(t *testing.T) {
	if bytes.Equal(papercraftSceneShellLandscape, papercraftSceneV2ShellLandscape) {
		t.Fatal("v1 landscape shell equals the v2 shell; v1 must stay on its original vendored copy")
	}
	if bytes.Contains(papercraftSceneShellLandscape, []byte("PT_PRESETS")) {
		t.Fatal("v1 landscape shell carries the v2 preset registry")
	}
}

// The embedded presets.json must be the registry the embedded shells were built from:
// every preset the Go validator accepts must also be baked into both shells.
func TestPapercraftSceneV2RegistryMatchesShells(t *testing.T) {
	reg, err := loadPapercraftSceneV2Registry()
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.presets) != 24 {
		t.Fatalf("registry has %d presets, want 24", len(reg.presets))
	}
	if len(reg.skins) != 12 {
		t.Fatalf("registry has %d skins, want 12", len(reg.skins))
	}
	for name, shell := range map[string][]byte{"landscape": papercraftSceneV2ShellLandscape, "portrait": papercraftSceneV2ShellPortrait} {
		if !bytes.Contains(shell, []byte("const PT_PRESETS=")) {
			t.Fatalf("%s shell has no baked PT_PRESETS registry", name)
		}
		for key := range reg.presets {
			if !bytes.Contains(shell, []byte(`"`+key+`":{`)) {
				t.Errorf("%s shell is missing preset %q from its baked registry", name, key)
			}
		}
		for skin := range reg.skins {
			if !bytes.Contains(shell, []byte("PT_SKINS."+skin+" = {")) {
				t.Errorf("%s shell is missing the renderer for skin %q", name, skin)
			}
		}
	}
}

func TestValidatePapercraftSceneTheme(t *testing.T) {
	reg, err := loadPapercraftSceneV2Registry()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		scene   string
		wantErr string
	}{
		{"absent", `{"title":"t"}`, ""},
		{"null", `{"title":"t","theme":null}`, ""},
		{"known paper theme", `{"theme":"kraft"}`, ""},
		{"known skin, case-insensitive", `{"theme":"SynthWave"}`, ""},
		{"override with base", `{"theme":{"base":"swiss","grain":0}}`, ""},
		{"inline skin object", `{"theme":{"skin":"flat","palette":{"bg":"#000000"}}}`, ""},
		{"unknown preset", `{"theme":"nope"}`, `unknown theme preset "nope"; valid presets: archive, blueprint`},
		{"unknown base", `{"theme":{"base":"nope"}}`, `unknown theme base "nope"; valid presets:`},
		{"unknown skin", `{"theme":{"skin":"nope"}}`, `unknown theme skin "nope"; valid skins: brutal, cad`},
		{"wrong type", `{"theme":42}`, "theme must be a preset name string or an object of overrides"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePapercraftSceneTheme([]byte(tc.scene), reg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// runV2WithFakes drives the v2 builtin with fake render deps and returns the assembled
// landscape HTML the render path received.
func runV2WithFakes(t *testing.T, scene string) (string, []string, error) {
	t.Helper()
	inDir, outDir := t.TempDir(), t.TempDir()
	scenePath := filepath.Join(inDir, "scene.json")
	timingsPath := filepath.Join(inDir, "timings.json")
	if err := os.WriteFile(scenePath, []byte(scene), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(timingsPath, []byte(`{"b1":[0,1],"total":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var landscape string
	deps := papercraftRenderDeps{
		startPage: func(_ context.Context, htmlPath string, w, _ int, _ string, _ []string) (papercraftPage, error) {
			body, err := os.ReadFile(htmlPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), papercraftSceneMarkerScene) || strings.Contains(string(body), papercraftSceneMarkerTimings) {
				t.Fatalf("assembled HTML %s still has an unsubstituted marker", htmlPath)
			}
			if w == 1920 {
				landscape = string(body)
			}
			return &fakePapercraftPage{duration: 1}, nil
		},
		startEncoder: func(_ context.Context, cfg papercraftEncoderConfig) (papercraftEncoder, error) {
			return &fakePapercraftEncoder{cfg: cfg}, nil
		},
	}
	got, err := runPapercraftSceneRenderV2Builtin(context.Background(), json.RawMessage(`{}`), []string{scenePath, timingsPath}, outDir, deps)
	return landscape, got, err
}

func TestPapercraftSceneRenderV2AcceptsKnownTheme(t *testing.T) {
	html, got, err := runV2WithFakes(t, `{"title":"t","tagline":"x","beats":[{"id":"b1","heading":"h"}],"theme":"synthwave"}`)
	if err != nil {
		t.Fatalf("runPapercraftSceneRenderV2Builtin: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"landscape.mp4", "portrait.mp4"}) {
		t.Fatalf("outputs = %v", got)
	}
	if !strings.Contains(html, `"theme":"synthwave"`) {
		t.Fatal("assembled HTML is missing the injected theme")
	}
	if !strings.Contains(html, "const PT_PRESETS=") {
		t.Fatal("assembled HTML is not built from the v2 shell")
	}
}

func TestPapercraftSceneRenderV2RendersWithoutTheme(t *testing.T) {
	html, got, err := runV2WithFakes(t, `{"title":"t","tagline":"x","beats":[{"id":"b1","heading":"h"}]}`)
	if err != nil {
		t.Fatalf("runPapercraftSceneRenderV2Builtin: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"landscape.mp4", "portrait.mp4"}) {
		t.Fatalf("outputs = %v", got)
	}
	if !strings.Contains(html, `{"scene":{"title":"t","tagline":"x","beats":[{"id":"b1","heading":"h"}]},"timings":`) {
		t.Fatal("assembled HTML does not carry the scene verbatim with no theme")
	}
}

func TestPapercraftSceneRenderV2FailsClosedOnUnknownTheme(t *testing.T) {
	inDir, outDir := t.TempDir(), t.TempDir()
	scene := filepath.Join(inDir, "scene.json")
	timings := filepath.Join(inDir, "timings.json")
	if err := os.WriteFile(scene, []byte(`{"title":"t","beats":[{"id":"b1"}],"theme":"vaporwave"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(timings, []byte(`{"b1":[0,1],"total":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := papercraftRenderDeps{
		startPage: func(context.Context, string, int, int, string, []string) (papercraftPage, error) {
			t.Fatal("must not start Chromium for an unknown theme")
			return nil, nil
		},
	}
	_, err := runPapercraftSceneRenderV2Builtin(context.Background(), json.RawMessage(`{}`), []string{scene, timings}, outDir, deps)
	if err == nil {
		t.Fatal("expected an error for an unknown theme")
	}
	for _, want := range []string{`unknown theme preset "vaporwave"`, "valid presets:", "kraft", "synthwave", "comic"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

func TestPapercraftSceneRenderV2RejectsMalformedSceneJSON(t *testing.T) {
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
	if _, err := runPapercraftSceneRenderV2Builtin(context.Background(), json.RawMessage(`{}`), []string{scene, timings}, outDir, deps); err == nil {
		t.Fatal("expected an error for malformed scene.json")
	}
}

func TestPapercraftSceneV2BuiltinRegistered(t *testing.T) {
	if _, ok := lookupBuiltinTemplateRunner(papercraftSceneV2BuiltinName); !ok {
		t.Fatalf("builtin %q is not registered", papercraftSceneV2BuiltinName)
	}
	if _, ok := lookupBuiltinTemplateRunner(papercraftSceneBuiltinName); !ok {
		t.Fatalf("v1 builtin %q must stay registered", papercraftSceneBuiltinName)
	}
}

// TestPapercraftSceneRenderV2RealChromiumAndFFmpeg renders a short scene with a skin
// preset through live Chromium and ffmpeg, the same way citadel work would. The pixel
// skin embeds a font and holds window.READY until it loads, so this also proves the
// live capture path waits for that gate instead of timing out. It also renders the same
// scene with no theme.
func TestPapercraftSceneRenderV2RealChromiumAndFFmpeg(t *testing.T) {
	if !platform.ChromiumAvailable() {
		t.Skip("Chromium is not installed")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	for _, theme := range []string{"pixel", ""} {
		name := theme
		if name == "" {
			name = "no-theme"
		}
		t.Run(name, func(t *testing.T) {
			inDir, err := os.MkdirTemp("", "pcs2-in-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(inDir) })
			outDir, err := os.MkdirTemp("", "pcs2-out-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(outDir) })
			scene := map[string]any{
				"title":   "Real Render Smoke Test",
				"tagline": "One beat, real Chromium, real ffmpeg.",
				"badge":   "TEST",
				"beats": []map[string]any{
					{"id": "b1", "heading": "Hello", "bullets": []string{"A real render"}, "visual": "process", "accent": "blue"},
				},
			}
			if theme != "" {
				scene["theme"] = theme
			}
			sceneBytes, err := json.Marshal(scene)
			if err != nil {
				t.Fatal(err)
			}
			scenePath := filepath.Join(inDir, "scene.json")
			timingsPath := filepath.Join(inDir, "timings.json")
			audioPath := filepath.Join(inDir, "narration.wav")
			if err := os.WriteFile(scenePath, sceneBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(timingsPath, []byte(`{"b1":[0,1.2],"total":1.2}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(audioPath, silentWAV(t, 2*time.Second), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := runPapercraftSceneRenderV2Builtin(context.Background(), json.RawMessage(`{}`),
				[]string{scenePath, timingsPath, audioPath}, outDir, livePapercraftRenderDeps())
			if err != nil {
				t.Fatalf("runPapercraftSceneRenderV2Builtin: %v", err)
			}
			if !reflect.DeepEqual(got, []string{"landscape.mp4", "portrait.mp4"}) {
				t.Fatalf("outputs = %v", got)
			}
			for _, f := range got {
				info, err := os.Stat(filepath.Join(outDir, f))
				if err != nil {
					t.Fatalf("stat %s: %v", f, err)
				}
				if info.Size() < 1000 {
					t.Fatalf("%s is suspiciously small: %d bytes", f, info.Size())
				}
			}
		})
	}
}
