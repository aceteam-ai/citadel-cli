# NVR init-image pin (#1396)

Source-only draft; no image layer was downloaded, pulled, run or published by
codex-citadel. Registry manifests, small config/provenance JSON and existing
GitHub build logs were read on 2026-10-21. No live module start/update occurred.

## Published source and immutable identity

The security repair [#1394](https://github.com/aceteam-ai/citadel-cli/pull/1394)
was merged by another authorized lane as
`2241cf4d5a171a0b11b733b84ec06035f185ab85`. Its existing main-branch
[build/publish run39609799044](https://github.com/aceteam-ai/citadel-cli/actions/runs/39609799044)
completed successfully and reports this OCI index digest:

`sha256:dcbe4f783eb14c7979d93db68ae9239eada5fec09f71d5966aa733b20b2eebba`

The public GHCR manifest for the **full source-commit tag**, not floating
`latest`, returns that same `Docker-Content-Digest`. The compose pins the index
with commit plus digest. The index contains one Linux/amd64 image and one
non-executable attestation entry; it does not add ARM support to this module.

- Linux/amd64 manifest:
  `sha256:a109638d0de6e97425696de401b67c56b7fc5fa210698dc0e1e0b64faa9cf2be`.
- Image config (1236 bytes):
  `sha256:5f872d2c3128d99bd421cce71fe135eaf9dc3c9b2c401d8f00aea26319c42482`.
  Read JSON declares Linux/amd64, `/usr/local/bin/nvrconfig` entrypoint and no
  non-root user override. This is metadata, not live UID/mount acceptance.
- Attestation manifest:
  `sha256:345f2e034641fa116e569b0246f0ceb4a23ab40ec5a2a3f56944a881a9c1848d`.
- SLSA provenance JSON (23003 bytes), whose body SHA was independently checked:
  `sha256:6353e9fa7a2235b52f3ad9ac7ad6dd1da462be71ed4de37a9f2e9a80f42ebd84`.
  It binds the image subject above to the full commit tag and records
  `vcs:source=https://github.com/aceteam-ai/citadel-cli` and the exact repaired
  `vcs:revision`. Build logs identify this workflow run as the BuildKit builder.
  This is consistency/provenance evidence, not a claim of independent signed
  builder attestation verification or a complete supply-chain audit.

For independent read-only recheck, use the OCI Distribution manifest endpoints
under `https://ghcr.io/v2/aceteam-ai/nvr-config/manifests/` with that full tag or
digest, plus `/blobs/` only for the small JSON objects above. Public anonymous
registry read access suffices; GitHub's package-list API required an additional
scope, which was not requested or granted. Do not fetch/run filesystem layers
to substitute for this source review.

## Cache policy and compatibility

The init service uses [`pull_policy: missing`](https://docs.docker.com/reference/compose-file/services/#pull_policy): an exact cached commit/digest
does not require the previous unconditional registry refresh. If it is absent,
image acquisition still needs network and the operator's ordinary module-start
authority. This does not claim the entire NVR stack works offline: other images,
Wyze/cloud camera sessions, NAS and assignment secrets have independent needs.

The credential opt-in, Frigate environment wiring, root identity, volumes and
init-completion dependency from #1394 are unchanged. Old compose remains on
the security repair's compatibility path; pinning source alone never repairs
an existing exposed config or changes a running container. Future security
repairs require a new reviewed pin, not silently moving this immutable image.

## Remaining gates

`TestNVRConfigInitImageIsPinnedAndCacheUsable` protects the exact reviewed
commit/digest and cache policy without registry/network/container execution.
Normal exact-head CI and independent source review are required; no local
candidate tests/build/formatter or Docker/Podman run is authorized here.
The separately owned `aceteam-ai/citadel-services` NVR catalog compose must be
mirrored through its own source-review lane before catalog installations get
this pin. Cross-repo propagation and an operator-approved cached-image start
with registry connectivity unavailable remain **unverified acceptance gates**.
No catalog write, undraft/merge, service restart, live credential/schema change,
registry publication, deployment or SQL application is authorized by this draft.
