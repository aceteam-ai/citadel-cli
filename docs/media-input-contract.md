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

- `workspace` requires `path`, never `capability`. The path must be local,
  workspace-relative, clean and NFC, without backslashes, colons or control
  characters. Actual descriptor-based opening, Files permission, containment,
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
bounded to 10 KiB, two descriptors, four JSON nesting levels and a fixed parser
budget. Exact keysets and decoded duplicate-key checks prevent case aliases and
escaped duplicate keys. Malformed UTF-8 and unpaired UTF-16 escapes are refused.

`testdata/media_input_v1.json` is a synthetic **input-only** shared fixture, with
positive cases and portable negative values or first-replacement mutations. Go tests and
an independent stdlib Python replay pin its SHA-256. The illustrative hashes and
capabilities do not identify real images, documents or live credentials. An
input fixture pin is not proof of image integrity or execution approval.

Offline verification:

```sh
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test ./internal/mediainput
python3 -m unittest discover -s internal/mediainput -p 'test_contract.py'
```

Remaining #1314 work includes the freezer and header/EXIF/mask guards, shared
pinned fetch transport, capability privacy, withheld store, sidecar and handler
wiring and the live `media_inputs` permission. The published result contract
must use the separately reviewed upload route: **no edit input or output image
bytes cross Redis**. No stale base64 publication fixture is pinned here. Native
GPU/model acceptance, signing/pin refresh, release and deployment remain outside
this slice.
