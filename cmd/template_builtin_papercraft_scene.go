package cmd

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// papercraft-scene-render (citadel-cli, aceteam-ai/aceteam#10502) wraps
// papercraft-render for the self-serve short-video pilot: instead of requiring a
// caller to already have two fully built Paper Trail player documents, this builtin
// accepts a small scene.json and a small per-beat timings.json (see
// aceteam-ai/paper-trail's lib/selfserve/scene.schema.json for the scene shape, and
// lib/pipeline/selfserve_render.py for the reference Python driver this Go builtin
// mirrors), injects them into the two vendored static shells
// (cmd/papercraft_scene_assets/), and then delegates to the EXACT SAME render and
// mux code path papercraft-render already uses (runPapercraftRenderBuiltin,
// unchanged). This keeps the AceTeam flow side small (two small JSON files plus an
// optional narration audio file, instead of two ~65KB HTML documents assembled
// node-side by the flow itself) while every Chromium-capture and ffmpeg-encode
// detail stays in one place.
//
// Self-contained: the two shells are embedded at compile time (go:embed), so this
// builtin needs no paper-trail checkout on the node and no Node build. See
// cmd/papercraft_scene_assets/README.md for the vendoring provenance.

const (
	papercraftSceneBuiltinName   = "papercraft-scene-render"
	papercraftSceneMarkerScene   = "__SCENE_JSON__"
	papercraftSceneMarkerTimings = "__TIMINGS_JSON__"
	papercraftSceneInputName     = "scene.json"
	papercraftSceneTimingsName   = "timings.json"
)

//go:embed papercraft_scene_assets/shell.landscape.html
var papercraftSceneShellLandscape []byte

//go:embed papercraft_scene_assets/shell.portrait.html
var papercraftSceneShellPortrait []byte

func init() {
	RegisterBuiltinTemplateRunner(papercraftSceneBuiltinName, func(ctx context.Context, params json.RawMessage, inputs []string, outDir string) ([]string, error) {
		return runPapercraftSceneRenderBuiltin(ctx, params, inputs, outDir, livePapercraftRenderDeps())
	})
}

// runPapercraftSceneRenderBuiltin is the papercraft-scene-render entry point. It
// never touches Chromium or ffmpeg directly: once the two shells are assembled into
// real HTML files on disk, it calls runPapercraftRenderBuiltin (template_builtin_papercraft.go,
// unmodified) exactly as papercraft-render's own runner does, so both builtins share
// one render implementation.
func runPapercraftSceneRenderBuiltin(ctx context.Context, params json.RawMessage, inputs []string, outDir string, deps papercraftRenderDeps) ([]string, error) {
	scenePath, timingsPath, audioPath, err := classifyPapercraftSceneInputs(inputs)
	if err != nil {
		return nil, err
	}

	sceneJSON, err := os.ReadFile(scenePath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", papercraftSceneInputName, err)
	}
	if err := validatePapercraftSceneJSONObject(sceneJSON, papercraftSceneInputName); err != nil {
		return nil, err
	}
	timingsJSON, err := os.ReadFile(timingsPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", papercraftSceneTimingsName, err)
	}
	if err := validatePapercraftSceneJSONObject(timingsJSON, papercraftSceneTimingsName); err != nil {
		return nil, err
	}

	landscapeHTML, err := injectPapercraftScene(papercraftSceneShellLandscape, sceneJSON, timingsJSON)
	if err != nil {
		return nil, fmt.Errorf("assemble landscape shell: %w", err)
	}
	portraitHTML, err := injectPapercraftScene(papercraftSceneShellPortrait, sceneJSON, timingsJSON)
	if err != nil {
		return nil, fmt.Errorf("assemble portrait shell: %w", err)
	}

	// A scratch directory for the two assembled HTML documents, separate from the
	// adapter's own input-staging and output directories. The assembled shells have
	// no external references of any kind (the whole paper-craft kit is inlined, see
	// cmd/papercraft_scene_assets/README.md), so unlike papercraft-render's normal
	// caller-supplied HTML plus sibling assets, there is no cross-file relative-path
	// concern for the asset server's document root to worry about here.
	workDir, err := os.MkdirTemp("", "papercraft-scene-render-*")
	if err != nil {
		return nil, fmt.Errorf("create scene render scratch directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	landscapePath := filepath.Join(workDir, "landscape.html")
	if err := os.WriteFile(landscapePath, landscapeHTML, 0o600); err != nil {
		return nil, fmt.Errorf("write landscape.html: %w", err)
	}
	portraitPath := filepath.Join(workDir, "portrait.html")
	if err := os.WriteFile(portraitPath, portraitHTML, 0o600); err != nil {
		return nil, fmt.Errorf("write portrait.html: %w", err)
	}

	renderInputs := []string{landscapePath, portraitPath}
	if audioPath != "" {
		renderInputs = append(renderInputs, audioPath)
	}

	// Delegate to the exact same render-and-mux code path papercraft-render uses:
	// Chromium capture (renderPapercraftFrames), frame pacing, and the ffmpeg encode
	// (startPapercraftFFmpeg) are untouched by this wrapper.
	return runPapercraftRenderBuiltin(ctx, params, renderInputs, outDir, deps)
}

// classifyPapercraftSceneInputs resolves the declared inputs by exact file name
// (scene.json, timings.json) and, for the optional narration audio, by extension
// (reusing papercraft-render's own isPapercraftAudioExtension). The wire contract
// stages each input at its declared node_path, so filepath.Base of a staged input
// path is exactly the name the caller gave the file (run_job_template_ops.go's
// resolveInputs preserves the declared NodePath verbatim); the AceTeam flow side
// must name its two JSON files exactly "scene.json" and "timings.json" when it
// stages them onto the node (e.g. via NodeFileWrite).
func classifyPapercraftSceneInputs(inputs []string) (scenePath, timingsPath, audioPath string, err error) {
	for _, input := range inputs {
		base := filepath.Base(input)
		switch {
		case base == papercraftSceneInputName:
			if scenePath != "" {
				return "", "", "", fmt.Errorf("papercraft-scene-render accepts exactly one %s input", papercraftSceneInputName)
			}
			scenePath = input
		case base == papercraftSceneTimingsName:
			if timingsPath != "" {
				return "", "", "", fmt.Errorf("papercraft-scene-render accepts exactly one %s input", papercraftSceneTimingsName)
			}
			timingsPath = input
		case isPapercraftAudioExtension(strings.ToLower(filepath.Ext(input))):
			if audioPath != "" {
				return "", "", "", fmt.Errorf("papercraft-scene-render accepts at most one audio input")
			}
			audioPath = input
		default:
			return "", "", "", fmt.Errorf("unrecognized input %q: expected %s, %s, or an audio file", base, papercraftSceneInputName, papercraftSceneTimingsName)
		}
	}
	if scenePath == "" {
		return "", "", "", fmt.Errorf("papercraft-scene-render requires a %s input", papercraftSceneInputName)
	}
	if timingsPath == "" {
		return "", "", "", fmt.Errorf("papercraft-scene-render requires a %s input", papercraftSceneTimingsName)
	}
	return scenePath, timingsPath, audioPath, nil
}

// validatePapercraftSceneJSONObject fails fast, before any Chromium or ffmpeg work
// starts, on a malformed or non-object scene.json / timings.json. The shell's own
// client-side JavaScript (scene_template.js) would otherwise fail deep inside a
// render with a much less actionable error.
func validatePapercraftSceneJSONObject(data []byte, label string) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("%s is not valid JSON: %w", label, err)
	}
	if _, ok := v.(map[string]any); !ok {
		return fmt.Errorf("%s must be a JSON object", label)
	}
	return nil
}

// injectPapercraftScene performs the exact two string replacements
// lib/pipeline/selfserve_render.py's inject_shell does: the raw JSON text of
// scene.json and timings.json in place of the shell's two literal placeholders.
// Plain text substitution, never a JSON parse or merge of the shell itself. Each
// placeholder is required to appear exactly once, matching the build_shells.py
// assertion on the source shells; a mismatch means a vendored shell drifted from
// what this function expects, not a bad caller input, so it fails closed rather
// than silently replacing every occurrence.
func injectPapercraftScene(shell, sceneJSON, timingsJSON []byte) ([]byte, error) {
	if n := bytes.Count(shell, []byte(papercraftSceneMarkerScene)); n != 1 {
		return nil, fmt.Errorf("shell has %d occurrences of %s, want exactly 1", n, papercraftSceneMarkerScene)
	}
	if n := bytes.Count(shell, []byte(papercraftSceneMarkerTimings)); n != 1 {
		return nil, fmt.Errorf("shell has %d occurrences of %s, want exactly 1", n, papercraftSceneMarkerTimings)
	}
	out := bytes.Replace(shell, []byte(papercraftSceneMarkerScene), sceneJSON, 1)
	out = bytes.Replace(out, []byte(papercraftSceneMarkerTimings), timingsJSON, 1)
	return out, nil
}
