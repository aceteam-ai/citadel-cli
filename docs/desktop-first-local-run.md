# Desktop first local run: review proposal

Status: design for independent review, not an implemented or shipping contract.
Refs #570, #617, #1233 and aceteam-ai/aceteam#10239. The ratified #617
decisions take precedence over the older product framing in #10239.

## Scope and settled boundaries

One Citadel Tauri shell supervises the Go sidecar. It is not a packaged
AceTeam website, a new headless node API, or a general shell. macOS is first;
#619 and #618 reuse this shell. The existing desktop implements hosted login,
pairing and a cockpit, but not chat or model installation.

Produce separate Apple Silicon and Intel Tauri DMGs. #1233 implements an
unsigned validation entrypoint, not distribution. Public distribution remains
gated on #672, Developer ID signing, notarization, stapling and native installer
acceptance. The old root `build-dmg.sh` must not become a second desktop path.
Intel packaging does not promise local GPU serving: the settled automatic
local-setup minimum is Apple Silicon with at least 16 GiB RAM. Below that,
offer an existing or new box; do not silently switch to hosted inference.
Propose macOS 14 as the first supported version, subject to native compatibility
review. Do not raise the current application deployment target in this slice.

## Authenticated node-pinned chat

Add an optional `target_node` to the existing platform chat request, using the
canonical headscale node identifier, not an IP, hostname, URL or org supplied
by the client. This field name is proposed and must be reconciled with any
existing request schema before implementation. The Next.js public boundary
must forward only validated input and derive user and org from the authenticated
session. The Python boundary must independently validate the pin.

Resolve the node through the existing own-org/shared-node authorization filter,
then require a fresh heartbeat, the requested advertised model and a reachable
per-node stream. Reject malformed identifiers, unauthorized pins and offline or
model-mismatched nodes before starting SSE. Do not reveal another org's node or
model inventory, and never replace a failed pin with another node or pool.
Recheck authorization when opening a new turn; cached inventory is not a grant.

Reuse the existing pinned dispatch queue and streaming response contract.
Cancellation and disconnect release all admission slots exactly once. Metadata,
model aliases, retries, geo hints and warming must not override the pin. Report
the actual executing node in trusted response metadata so the client can verify
the local badge; a requested pin alone is not proof of execution.

The ratified billing rule is free inference on the caller's own org node, with
normal billing for shared or marketplace nodes. Resolve ownership server-side
before selecting that rule. Free is not exempt from concurrency, abuse or
resource admission limits. Do not let a client flag, missing price, alias or
unverified receipt waive billing. Test both buffered and streaming settlement,
errors, warming, cancellation and retries before claiming the rule works.

Keep credentials in the Rust credential store. Expose bounded typed chat IPC
commands and sanitized events to the web view, not bearer tokens, arbitrary
URLs or arbitrary subprocess arguments. Stream text with bounded buffering;
render text without executable HTML. An explicit selected node/model remains
visible during warming, reconnect, pause and failure. Offline loopback chat is
a separate reviewed slice; do not bypass platform authentication in this one.

## Ollama bootstrap proposal

Use a private, versioned runtime and model directory under the app's Application
Support root, owned by the current user. No sudo, Homebrew, global PATH edits,
system LaunchDaemon, global environment changes or adoption of a pre-existing
Ollama install. Reuse the existing Go native-service supervision through fixed
typed commands, adding an explicit executable path if required. Reject unsafe
archive paths, symlink escapes and unexpected architectures before execution.

Proposed runtime: official Ollama `v0.35.1` CLI archive, observed 2026-10-06:

- URL: `https://github.com/ollama/ollama/releases/download/v0.35.1/ollama-darwin.tgz`
- Compressed bytes: `159625239`.
- SHA-256: `3137dbf28948ee844e0fb3e584d9b5de6879d73d9f0cb7eff3ad64930601d307`.

Pin that URL, size ceiling and checksum in a reviewed manifest. Never fetch
`latest` or execute a downloaded install script. Review the extracted layout,
licenses and native security posture before accepting this archive for release.
The app installer identity does not automatically validate third-party code.

Run app-owned Ollama on an explicit loopback listener with `OLLAMA_NO_CLOUD=1`
and a private `OLLAMA_MODELS` directory. Leave an occupied port or external
runtime untouched and surface a conflict instead of killing or reconfiguring it.
Integrate the selected private endpoint with the sidecar and truthful heartbeat
discovery before claiming the model is available to Fabric. Do not widen CORS or
expose the unauthenticated runtime to the LAN.

Proposed models, with registry manifest digests observed 2026-10-06:

| Machine | Model ID | Approximate download | Minimum free disk |
| --- | --- | --- | --- |
| Small model, manual choice on an eligible machine | `qwen3:1.7b` | 1.4 GB | 6 GiB |
| Apple Silicon, 16 to less than 32 GiB RAM | `qwen3:4b` | 2.5 GB | 8 GiB |
| Apple Silicon, at least 32 GiB RAM | `qwen3:8b` | 5.2 GB | 14 GiB |

The small model does not relax the settled 16 GiB automatic-setup minimum.
These are proposed conservative tiers, not measured native acceptance results.
Cap initial context at 4096 tokens and initial generation at 256 output tokens;
do not allocate the advertised maximum context by default.

- `qwen3:1.7b`: `8f68893c685c3ddff2aa3fffce2aa60a30bb2da65ca488b61fff134a4d1730e7`
- `qwen3:4b`: `359d7dd4bcdab3d86b87d73ac27966f4dbb9f5efdfcc75d34a8764a09474fae7`
- `qwen3:8b`: `500a1f067a9f782620b40bee6f7b0c89e17ae61f686b92c24933e4ca4b2b8b41`

Model tags are mutable. Require the reviewed digest before pull and verify the
installed digest after pull before warming or advertisement. A changed tag is
an explicit review/update error, not permission to use a different model. Do
not assume the pull API accepts a digest-qualified model name. Review licensing
and preserve provenance in the installed state record.

## Installation state and bounded failure

Explicit consent precedes runtime/model downloads and shows size and storage
location. Persist an app-owned operation record with node, model, expected
digest, completed layers and current state, never credentials. Check free space
before and during download; require the table's floor and enough space for
remaining layers, bounded staging and a 2 GiB reserve, whichever is greater.

States: consent, runtime staging, registering, pulling, warming, verifying
advertisement, ready, paused, retryable failure. Record interruption as paused
or failed, never ready. Show observed per-layer pull progress; report ETA as an
estimate. A restart reconciles the actual installed digest and active job before
resuming, rather than spawning duplicate pulls.

Use a 30-second connection deadline, a 120-second no-progress deadline and a
30-minute total pull deadline. Pause/cancel closes the owned request and verifies
the pull stopped; if cancellation is not acknowledged, show stopping and do not
claim paused. Test real Ollama cancellation/resumption before enabling that UI.
Retain verified layers for a user-requested resume. Never remove shared caches or
terminate another user's process. Runtime pause while inference is active needs
an explicit busy response or a reviewed drain operation.

Warm with a bounded prompt, then require a real nonempty response and a fresh
Fabric advertisement for this exact node/model. Allow 120 seconds for warmup
and 60 seconds for advertisement. Timeout remains retryable and Chat stays
disabled. The final acceptance prompt must travel through authenticated pinned
platform chat, not only loopback: validate trusted executing-node metadata and
a nonempty streamed reply. Pause is not a broken-node error.

Updates require a newly reviewed runtime/model manifest, idle staging and
rollback to the prior valid runtime on failed health checks. No automatic tag
refresh or independent updater competes with the desktop helper lifecycle.
Uninstall stops only app-owned work and asks separately about app-owned model
data and enrollment revocation; preserve manual services and unrelated config.

## Source slices and evidence

1. Review this design and the exact auth, billing, runtime and model contracts.
2. Platform source: node pin, own-org billing classification and end-to-end
   boundary tests. No expansion of aceteam-ai/aceteam#10269's callback scope.
3. Desktop source: typed authenticated chat IPC and bounded streaming states.
4. Sidecar/bootstrap source: reviewed manifest, fixed runtime commands, storage,
   cancellation, readiness and truthful native model discovery.
5. Disposable Mac acceptance: both DMGs, install/eject/move/upgrade, hosted
   callback and MFA, Keychain isolation, enrollment, model pull/cancel/resume,
   warming, advertisement, first pinned reply and uninstall preservation.
6. Separate reviewed Developer ID/notarization/update implementation and release.

Before any source PR becomes ready, record its exact-head automated checks and
all manual gates it includes. Design acceptance does not constitute a native
pass, production deployment, model download, signing purchase or permission to
waive issue #570's remaining gates.

## Primary references

- [Framework and ratified decisions](https://github.com/aceteam-ai/citadel-cli/issues/617)
- [Transferred onboarding design](https://github.com/aceteam-ai/aceteam/issues/10239)
- [Ollama runtime release and asset metadata](https://github.com/ollama/ollama/releases/tag/v0.35.1)
- [Ollama Qwen3 model catalog](https://ollama.com/library/qwen3)
- [Ollama configuration and local-only mode](https://docs.ollama.com/faq)
- [Ollama pull API](https://docs.ollama.com/api/pull)
- [Ollama generate API](https://docs.ollama.com/api/generate)
