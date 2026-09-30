# papercraft-scene-render vendored assets (citadel-cli, aceteam-ai/aceteam#10502)

`shell.landscape.html` and `shell.portrait.html` are vendored, byte-for-byte copies of
`lib/selfserve/shell.horizontal.html` and `lib/selfserve/shell.vertical.html` from the
`aceteam-ai/paper-trail` repo (branch `feat/selfserve-render`, draft PR
aceteam-ai/paper-trail#18, commit 528c392).

Each is a self-contained, static Paper Trail player: the shared paper-craft kit
(`lib/paper.js`, `lib/props.js`, `lib/titlecard.js`) and the generic, data-driven scene
renderer (`lib/selfserve/scene_template.js`) are already baked in. Nothing external is
fetched or referenced; every prop is drawn programmatically (SVG/canvas), so there is no
asset root or relative-path concern for the Chromium render. The only thing left
unresolved in either file is two literal placeholders, `__SCENE_JSON__` and
`__TIMINGS_JSON__`, each appearing exactly once.

`cmd/template_builtin_papercraft_scene.go` embeds both files via `go:embed` and does the
two placeholder replacements in Go at render time (`injectPapercraftScene`), so this
builtin needs no paper-trail checkout, no Node build, and no external HTTP fetch on the
node. It is self-contained.

Re-vendor by copying the two files again from a newer paper-trail commit if
`lib/paper.js`, `lib/props.js`, `lib/titlecard.js`, `lib/player.html`, or
`lib/selfserve/scene_template.js` change there. There is no build step here: these are
committed as final, ready-to-serve HTML, matching how `lib/selfserve/build_shells.py`
already produces them in the source repo.
