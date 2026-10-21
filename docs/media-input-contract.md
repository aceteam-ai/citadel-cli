# Media input v1: inactive contract foundation

`internal/mediainput` owns strict parsing for the proposed sovereign edit input
envelope. This first source slice does **not** register tasks, advertise a
capability, read files, redeem tokens, start a sidecar or run a model. A parsed
descriptor is not an authorized or verified image snapshot.

The `inputs` job field contains a JSON object with exactly `schema` (the string
`media_input.v1`) and `inputs` (an array). `image-to-image` requires exactly one
`source`; `inpaint` requires a `source` followed by one `mask`. Other tasks,
roles, cardinalities and orderings are refused, not silently adapted.

Each descriptor has exactly `role`, `origin`, `sha256`, `size`, `format` and
one location field:

- `workspace` requires `path`, never `capability`. `mediainput.ValidPath` owns
  the shared, platform-independent slash-only lexical rule: nonempty relative,
  clean and NFC, without backslashes, colons, control/format characters,
  Unicode line/paragraph separators, NBSP or fullwidth dots. Each component
  is at most `MaxPathComponentBytes` UTF-8 bytes; `..` components are refused.
  `NUL` and `con.png` have the same lexical acceptance on every OS: this is
  syntax, not a claim that a platform can or should open that name. Actual
  descriptor-based opening, Files permission, regular-file policy, containment,
  symlink/race checks and snapshot freezing remain prerequisites in C1b.
- `inbox` requires `capability`, never `path`. The opaque bearer is bounded to
  4096 ASCII token characters; diagnostics, logs and JSON redact it by default.
  Only the authorized fetch client should explicitly `Reveal()` it. Only the
  coordinator can authenticate its purpose,
  signature, binding, expiry and single-use redemption. The node receives no URL
  from this descriptor. Capability privacy must land before anything mints or
  dispatches these jobs.

SHA-256 is exactly 64 lowercase hex characters. Size is a JSON integer from 1
through 16 MiB; floats, exponents, strings and booleans do not coerce. Source
formats are `png`, `jpeg` and `webp`; a mask is `png` only. The full envelope is
bounded to 10 KiB, two descriptors, four JSON value levels (root at level one,
including scalar leaves) and a fixed parser
budget. Exact keysets and decoded duplicate-key checks prevent case aliases and
escaped duplicate keys. Malformed UTF-8 and unpaired UTF-16 escapes are refused.

`testdata/media_input_v1.json` is a synthetic **input-only** shared fixture, with
positive cases and portable negative values or first-replacement mutations.
Each negative has an expected static refusal reason, checked in both Go and
the independent stdlib Python replay. Both pin the fixture's SHA-256. A wrong
type for `inputs` is `invalid_inputs`; an array with wrong role/cardinality is
`invalid_roles` (the parser's structural bounds still apply first).
The illustrative hashes and
capabilities do not identify real images, documents or live credentials. An
input fixture pin is not proof of image integrity or execution approval.

Offline verification requires an already installed Go toolchain satisfying
the `go` directive in `go.mod` and an already populated dependency cache.
Go 1.25.4 is too old: do not pin `GOTOOLCHAIN=local` to that executable and
claim the commands work. Toolchain/dependency preparation is a separate,
authorized action; these offline commands do not authorize a download:

```sh
GOTOOLCHAIN=local go version # must satisfy go.mod, without auto-selection
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -vet=off -race -count=1 ./internal/mediainput
python3 -I -B -m unittest discover -s internal/mediainput -p 'test_contract.py'
```

Select that installed Go executable explicitly if `go` on PATH is too old.
`go version` without `GOTOOLCHAIN=local` may report an automatically selected
toolchain rather than the executable that the offline test command will use.
The test-only command disables implicit vet; it does not authorize a formatter,
standalone build, toolchain or dependency download, model or service execution.

## Reuse for multi-reference video

V1 remains the exact source/mask schema above. #1374 needs a separately reviewed
versioned extension, not a new v1 role or a relaxed two-descriptor parser.
The [paired #11825 DoR](https://github.com/aceteam-ai/aceteam/issues/11825#issuecomment-6294090893)
calls that extension **v1.1**, but explicitly leaves the exact discriminator,
envelope bound, capability advertisement and shared fixture to joint ratification
by automated-aceteam and automated-citadel. An earlier local **v2** proposal was
not a settled contract choice. Do not implement either spelling from this doc.

That DoR and [Jason's recorded input/lifecycle decisions](https://github.com/aceteam-ai/aceteam/issues/11260#issuecomment-6290189919)
already settle the relevant bounds: one source for `image-to-video`, ordered
1..8 references for `ref-to-video`, 16 MiB and 16,777,216 pixels per image,
128 MiB total reference bytes, a 176 MiB reference sidecar request cap and one
reference snapshot holder. These are not new product questions. Use the shared
admission boundary rather than permitting independent edit/video holder pools.
The [recorded output decision](https://github.com/aceteam-ai/aceteam/issues/11825#issuecomment-6293720735)
requires the same single-use media-output upload route for both conditioned
video tasks: **no input or output video/image bytes cross Redis** on this path.
The legacy text-only bytes remain unchanged.

The extension must reuse `ValidPath`, frozen-byte acquisition and the existing
permission/publication lifecycle, never workspace staging or a container bind.
Its implementation starts only after #1314 C5 and the paired contract gates.
The sole shared fixture must be extended and repinned jointly, not forked.
Main's accepted C1a input-only fixture is SHA-256
`e386064cb2c371cadc8bdc4eb6475adf78510a14fa9ae9aac450bcd830c185e0`;
this draft's lexical/refusal follow-ups use
`6d19921e8d842ecd00dfba709ad073eacc0511dce7f12e9c2107b0e0f8fcca1e`,
which is not yet paired platform ratification. Neither pin covers upload/results
or video activation. No extended parser, new task or live producer is activated.

The #1392/#1393 hard-link count and ownership questions remain acquisition-policy
gates. The inactive Linux freezer separately pins the leaf with `O_PATH` and
checks its type before opening the same inode for reading; see
`media-input-freezer.md`. That technical preflight, this lexical validator and
a matching content hash do not prove that an inode has no outside-workspace
hard links. No UID restriction is invented by this source change.

The separately prepared workspace freezer and header/EXIF/mask guards remain
inactive; parsing still does not invoke them or authorize acquisition.
Remaining #1314 work includes reviewed acquisition-policy decisions, shared
pinned inbox-fetch transport, withheld store, sidecar and handler wiring and
the live `media_inputs` permission. The published result contract
must use the separately reviewed upload route: **no edit input or output image
bytes cross Redis**. No stale base64 publication fixture is pinned here. Native
GPU/model acceptance, signing/pin refresh, release and deployment remain outside
this slice.
