# papercraft-scene-render-v2 vendored assets (citadel-cli, aceteam-ai/aceteam#10803)

`shell.landscape.html`, `shell.portrait.html` and `presets.json` are vendored, byte-for-byte
copies of `lib/selfserve/shell.horizontal.html`, `lib/selfserve/shell.vertical.html` and
`lib/selfserve/presets.json` from the `aceteam-ai/paper-trail` repo (branch
`feat/scene-styles`, aceteam-ai/paper-trail#22 stacked on #21, commit
2c0ba51be3b9d89cb2978f3b40176feec231874a).

These are the v2 shells. Compared with the v1 shells in `cmd/papercraft_scene_assets/`
(which stay unchanged, so an existing approval of `papercraft-scene-render` keeps its exact
semantics), they add:

- An optional top-level `theme` in scene.json: a preset name from `presets.json` (24
  built-ins: 12 paper themes and 12 skins that change the drawing style), or an object of
  overrides with an optional `base` preset. Unknown names fail closed. No theme renders the
  original Paper Trail look.
- Two end-card fixes for every render, themed or not: a long tagline wraps to the card, and
  a badge equal to the series name is shown once. Every frame before the end card of an
  unthemed render is byte-identical to v1.
- Two small OFL font subsets (PT Pixel, PT Hand) inlined as data URIs for the pixel and
  sketch skins; their licenses live in paper-trail's `lib/selfserve/fonts/`. A skin that
  embeds a font holds `window.READY` until the font has loaded.

Like v1, each shell is a self-contained, static player with no external references, and
the only unresolved text is the two literal placeholders `__SCENE_JSON__` and
`__TIMINGS_JSON__`, each appearing exactly once. `presets.json` is used only by
`validatePapercraftSceneTheme` in Go, to reject an unknown theme before Chromium starts; the
shells carry their own baked-in copy of the same registry for rendering, and
`TestPapercraftSceneV2RegistryMatchesShells` checks the two agree.

`cmd/template_builtin_papercraft_scene_v2.go` embeds all three files via `go:embed` and reuses
v1's input classification, JSON checks, placeholder injection, and the shared
`runPapercraftRenderBuiltin` Chromium and ffmpeg path.

Re-vendor by copying the three files again from a newer paper-trail commit (after running
`python3 lib/selfserve/build_shells.py` there) and updating the commit above. If the change
alters rendering semantics for an already-approved version, add a new builtin version rather
than re-vendoring this one in place, the same way v2 sits beside v1.
