# RECONSTRUCT_SPLAT source plan (#1381)

Source-only DRAFT. Full issue scope is preserved: a node-dispatched video/frame
directory reconstruction, confined workspace inputs, per-job sandboxed container,
long watchdog, real-exit cancellation, bounded SfM/training progress, reference
artifacts with sizes/hashes, fail-closed signed v2 work receipt, GPL label,
mandatory PLY **and SPZ**, optional genuine poster, and a separately authorized
short-video acceptance on a 24 GB node. A contract/helper or PLY-only pipeline
does not complete this issue. Platform flow/ace CLI/viewer wiring is paired work.

## Existing source is an engine, not a dispatched trust boundary

At main5db1c381, `internal/reconstruct.Runner` and the one-shot compose already
run SfM and training. The engine accepts operator paths via EvalSymlinks, keeps
an unbounded SfM log buffer, returns PLY without size, and uses a pathname rename
after an early output-absence check. Those are not sufficient guarantees for
untrusted dispatched workspace paths or create-only bundle publication. Float
range comparisons alone also admit NaN/Inf in some tuning fields. No job type
or safe registration is implied by the existing engine's historical GPU test.

## Phased delivery, each with source/tests in this draft

1. Prepare necessary engine contract hardening: actual hashed artifact sizes,
   finite tuning/quality values and bounded capture of the SfM summary. Preserve
   existing local CLI defaults/PLY shape additively, and test empty/invalid
   artifacts and cancellation. These changes do not establish path authority.
2. Add the mandatory SPZ leg with pinned reviewed converter source/dependencies,
   checked conversion success, bounded format/count verification, and matching
   producer/viewer version and coordinate conventions. Hash both artifacts
   before a create-only bundle can be published. Existing operator PLY-only
   behavior may remain explicit; a new RECONSTRUCT_SPLAT success cannot omit SPZ.
   Emit a poster only if the pinned engine has a verified still-render path;
   otherwise omit it rather than invent an image or claim rendered acceptance.
3. Implement the dispatched handler and confinement: node-target-only, trusted
   workspace and live Files authority, no URLs or payload-selected host roots,
   image/executable identity or shell command. Freeze a bounded video/frame
   manifest into private per-job custody with actual hashes/sizes; validate
   no-follow regular files, symlink/hardlink/owner/device policy, file-count and
   total-byte limits. The 16 MiB image freezer is not a video/frame-tree API.
   Input policy/limits and the shared C1b rules need review, not guessed reuse.
4. Launch only an explicitly configured, verified cached immutable image under
   the existing compose hardening (no network, read-only root, dropped caps,
   narrow mounts, trusted UID). No build/pull/download is performed on a job's
   request. Bind a unique actual container ID before treating startup as owned;
   stop-and-wait that ID on cancellation, failure or watchdog. Unknown startup
   or exit retains custody and blocks overlapping execution; a CLI/HTTP timeout
   is not proof the GPU container exited. Do not reuse the old engine's broad
   host-path mounts as a job sandbox. Publish only from an owned staging FD/tree
   using create-only semantics; an early Lstat plus os.Rename is insufficient.
5. Declare the long deadline tier and serialized heavy execution, with bounded
   content-free SfM/training progress and no raw input paths/log dumps. Use the
   standard v2 signer preflight before expensive work and sign the actual input
   and output manifests before success publication. Bind trusted org/node/job,
   immutable input/image/tool identities and normalized parameters; return only
   relative artifact paths, lowercase hashes and actual sizes. No inline splats.
   Add a separately reviewed event action (proposed `reconstruct_splat`) and
   plausible observed units in Citadel **and** the backend verifier/ingest before
   live advertisement. Do not misuse `index`/`inference` or weaken the allowlist.
   This is independent of Braindance #9819's Ed25519 manifest/keyframe contract.
6. Wire registration only after the source prerequisites are real; advertise
   from actual configured/permission-supported handlers, not version thresholds.
   Prepare hermetic payload/path/argv/env/progress/receipt/custody tests and then
   obtain independent exact-head review and normal CI. Native image promotion,
   real reconstruction and paired staging flow/viewer acceptance remain held.

## SPZ source findings and consumer coordination

Primary source inspected at `nianticlabs/spz@affd0ecea7fbb4c265ee119475af7ee5b2997482`
([repository](https://github.com/nianticlabs/spz/tree/affd0ecea7fbb4c265ee119475af7ee5b2997482)).
This is source provenance, not a fetched/built/accepted converter artifact.
The [publisher README](https://github.com/nianticlabs/spz/blob/affd0ecea7fbb4c265ee119475af7ee5b2997482/README.md)
identifies MIT and describes current v4/ZSTD versus legacy gzip formats and
explicit coordinate conversion. Viewer #11829 currently names Spark but does
not settle accepted SPZ versions. Do not silently assume the newest producer
default is supported by that consumer or that unspecified axes are correct.

The [converter source](https://github.com/nianticlabs/spz/blob/affd0ecea7fbb4c265ee119475af7ee5b2997482/cli_tools/src/ply_to_spz.cpp)
does not inspect the save function's boolean result, so its exit status alone
cannot prove a successful conversion. Its default options also do not freeze
the required consumer contract. A reviewed wrapper or explicit compatible
source version is required, plus artifact/header/count verification. The
[CMake source](https://github.com/nianticlabs/spz/blob/affd0ecea7fbb4c265ee119475af7ee5b2997482/CMakeLists.txt)
can fetch missing compression dependencies: source preparation must pin/review
them and forbid an unreviewed fallback fetch, not claim an offline build.

Spirula's existing GPL-3.0 attribution and separate-container boundary remain
visible; label it without enforcing a new licence gate or claiming legal
distribution approval. SPZ attribution and exact source/notice pins must be
retained with any later image change. No source metadata is native validation.

## Acceptance ledger

- Cross-repo signing/manifest/units/viewer choice is [a proposal only](https://github.com/aceteam-ai/aceteam/issues/11829#issuecomment-6327102060),
  not a frozen design. Automated-aceteam reports no assigned viewer/backend
  owner while its P0 queue is active. Neither side may invent the new action
  unilaterally. Actual pinned consumer version, axes/exporter fixture, manifest
  encoding, signed-unit equality and confinement limits still need ratification.
- Phase 1 source prepared: artifact sizes count the bytes actually hashed;
  empty final artifacts are refused. Nonfinite tuning, overflowing/impossible
  quality counts and percentages are rejected; SfM capture is a fixed-size
  summary-tail ring with full upstream-write consumption. Synthetic regressions
  cover these boundaries, including retained/lost summaries and hash cancellation.
  They are NOT locally executed results. No safe publication/path guarantee is
  inferred from this prerequisite, and the remaining phases are still required.
- Prepared plan/source phases are not proof of a usable dispatched job.
- Engine publication prerequisite prepared: Linux no-replace rename with opened
  parent descriptors, immediate-parent symlink refusal and no unsafe fallback.
  Synthetic regressions cover late destination collisions, two complete bundles
  racing, cancellation and parent symlinks. This closes the empty-directory
  replacement race only; trusted operator ancestor paths/private trees remain
  outside dispatched input/output confinement. No fsync/crash durability,
  network-filesystem recovery or real container-exit custody is established.
- Pending: accepted receipt action/units and paired backend owner; viewer SPZ
  version/axes, converter/tool/dependency pins and confined input limits/policy.
- Bounded progress source prepared: an optional trusted observer receives only
  fixed stage/state transitions for extraction, SfM and training, at most six
  events. Synthetic tests include private noisy logs and tool errors, failed
  quality, cancellation and missing artifacts. Starting does not prove process
  ownership; training completion does not prove publication or signed success.
  No authenticated progress transport or job registration is wired by the seam.
- Pending: complete handler/container/receipt/publication/registration source,
  negative/race/fault tests and exact-head independent review/CI.
- Held: local candidate execution outside separate grants, GPU/model/binary or
  image download/build/run/publication, native codec round-trip, measured short
  phone-video reconstruction on 24 GB with timings, real poster/viewer/flow/ace
  acceptance, live permission/credential/schema changes, SQL and deployment.
- Held: PR undraft/merge. Keep #1381 OPEN/WIP through the full remaining scope.
