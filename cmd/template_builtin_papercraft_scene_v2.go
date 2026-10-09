package cmd

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// papercraft-scene-render-v2 (citadel-cli, aceteam-ai/aceteam#10803) is the same
// self-serve Paper Trail render as papercraft-scene-render (v1), with newer vendored
// shells (cmd/papercraft_scene_assets_v2/) that add selectable visual styles: an
// optional top-level "theme" in scene.json naming one of 24 built-in presets (12 paper
// themes and 12 non-paper skins) or an object of overrides, plus two end-card fixes
// (a long tagline wraps to the card, and a badge equal to the series name is shown
// once). A scene with no theme renders the original look; only its end card differs
// from v1.
//
// v1 is deliberately left untouched (its own builtin name, Go code and embedded
// shells), so a node owner's existing approval of papercraft-scene-render keeps
// meaning exactly what it meant. v2 is a separate builtin that needs its own template
// registration and approval.
//
// Everything after the shell assembly is shared with v1 and papercraft-render: input
// classification by exact file name, the JSON object checks, the two-placeholder
// injection, and the Chromium-capture plus ffmpeg-encode path
// (runPapercraftRenderBuiltin) are reused unchanged. The only new step is
// validatePapercraftSceneTheme, which fails closed on an unknown preset name before
// any Chromium work starts (the shell would also refuse it in the browser, but only
// after a 30 second READY timeout with a much less actionable error).

const papercraftSceneV2BuiltinName = "papercraft-scene-render-v2"

//go:embed papercraft_scene_assets_v2/shell.landscape.html
var papercraftSceneV2ShellLandscape []byte

//go:embed papercraft_scene_assets_v2/shell.portrait.html
var papercraftSceneV2ShellPortrait []byte

// presets.json is the paper-trail style registry the two v2 shells were built from
// (lib/selfserve/presets.json at the same commit). It is used only to validate a
// scene's theme name in Go; the shells carry their own baked-in copy for rendering.
//
//go:embed papercraft_scene_assets_v2/presets.json
var papercraftSceneV2PresetsJSON []byte

func init() {
	RegisterBuiltinTemplateRunner(papercraftSceneV2BuiltinName, func(ctx context.Context, params json.RawMessage, inputs []string, outDir string) ([]string, error) {
		return runPapercraftSceneRenderV2Builtin(ctx, params, inputs, outDir, livePapercraftRenderDeps())
	})
}

// papercraftSceneV2Registry is the parsed presets.json: the valid preset names and
// the skin keys those presets use (a theme object may name a skin directly).
type papercraftSceneV2Registry struct {
	presets map[string]bool
	skins   map[string]bool
}

func loadPapercraftSceneV2Registry() (papercraftSceneV2Registry, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(papercraftSceneV2PresetsJSON, &raw); err != nil {
		return papercraftSceneV2Registry{}, fmt.Errorf("embedded presets.json is not valid JSON: %w", err)
	}
	reg := papercraftSceneV2Registry{presets: map[string]bool{}, skins: map[string]bool{}}
	for key, body := range raw {
		if strings.HasPrefix(key, "_") {
			continue
		}
		reg.presets[key] = true
		var entry struct {
			Skin string `json:"skin"`
		}
		if err := json.Unmarshal(body, &entry); err == nil && entry.Skin != "" {
			reg.skins[entry.Skin] = true
		}
	}
	if len(reg.presets) == 0 {
		return papercraftSceneV2Registry{}, fmt.Errorf("embedded presets.json has no presets")
	}
	return reg, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validatePapercraftSceneTheme checks scene.json's optional "theme" against the
// embedded registry, mirroring the shell's own resolveTheme rules: absent or null is
// the original look; a string must name a preset (case-insensitive); an object may
// carry "base" (a preset name) and "skin" (a skin key), each of which must exist.
// Anything else fails closed with the list of valid names.
func validatePapercraftSceneTheme(sceneJSON []byte, reg papercraftSceneV2Registry) error {
	var scene map[string]json.RawMessage
	if err := json.Unmarshal(sceneJSON, &scene); err != nil {
		return fmt.Errorf("%s is not valid JSON: %w", papercraftSceneInputName, err)
	}
	rawTheme, ok := scene["theme"]
	if !ok || string(rawTheme) == "null" {
		return nil
	}
	valid := strings.Join(sortedKeys(reg.presets), ", ")
	var name string
	if err := json.Unmarshal(rawTheme, &name); err == nil {
		if !reg.presets[strings.ToLower(name)] {
			return fmt.Errorf("unknown theme preset %q; valid presets: %s", name, valid)
		}
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rawTheme, &obj); err != nil {
		return fmt.Errorf("theme must be a preset name string or an object of overrides; valid presets: %s", valid)
	}
	if rawBase, ok := obj["base"]; ok && string(rawBase) != "null" {
		var base string
		if err := json.Unmarshal(rawBase, &base); err != nil || !reg.presets[strings.ToLower(base)] {
			return fmt.Errorf("unknown theme base %s; valid presets: %s", string(rawBase), valid)
		}
	}
	if rawSkin, ok := obj["skin"]; ok && string(rawSkin) != "null" {
		var skin string
		if err := json.Unmarshal(rawSkin, &skin); err != nil || !reg.skins[skin] {
			return fmt.Errorf("unknown theme skin %s; valid skins: %s", string(rawSkin), strings.Join(sortedKeys(reg.skins), ", "))
		}
	}
	return nil
}

// runPapercraftSceneRenderV2Builtin is the papercraft-scene-render-v2 entry point.
// It is v1's runPapercraftSceneRenderBuiltin with the v2 shells and the theme check.
func runPapercraftSceneRenderV2Builtin(ctx context.Context, params json.RawMessage, inputs []string, outDir string, deps papercraftRenderDeps) ([]string, error) {
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
	reg, err := loadPapercraftSceneV2Registry()
	if err != nil {
		return nil, err
	}
	if err := validatePapercraftSceneTheme(sceneJSON, reg); err != nil {
		return nil, err
	}
	timingsJSON, err := os.ReadFile(timingsPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", papercraftSceneTimingsName, err)
	}
	if err := validatePapercraftSceneJSONObject(timingsJSON, papercraftSceneTimingsName); err != nil {
		return nil, err
	}

	landscapeHTML, err := injectPapercraftScene(papercraftSceneV2ShellLandscape, sceneJSON, timingsJSON)
	if err != nil {
		return nil, fmt.Errorf("assemble landscape shell: %w", err)
	}
	portraitHTML, err := injectPapercraftScene(papercraftSceneV2ShellPortrait, sceneJSON, timingsJSON)
	if err != nil {
		return nil, fmt.Errorf("assemble portrait shell: %w", err)
	}

	workDir, err := os.MkdirTemp("", "papercraft-scene-render-v2-*")
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
	return runPapercraftRenderBuiltin(ctx, params, renderInputs, outDir, deps)
}
