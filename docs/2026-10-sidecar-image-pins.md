# Embedded sidecar image pins

The `services/compose` defaults for Claude Code, Hermes, Kokoro and Whisper
use OCI index digests. A published `latest` tag can move; the digest fixes the
image that a node will request. This source change does not start, pull or
publish any container.

On 2026-10-23, a read-only GHCR manifest check found each `latest` index
equal to the commit-derived build tag shown below. The linked push builds on main
completed successfully. The exact references are pinned by
`TestOwnSidecarImagesArePinnedByDigest` in `services/image_refs_test.go`.

| Compose file | Build source and run | Published tag | OCI index digest |
| --- | --- | --- | --- |
| `claudecode.yml` | [`citadel-cli` 6fad6be, run 39946524944](https://github.com/aceteam-ai/citadel-cli/actions/runs/39946524944) | `6fad6be3951e28d6a4ad34a6e39e8c90113328ae` | `sha256:c2285d2bfe1db43086e240c4c95b3f605bbf83f87579fa29d1b97bade77feee2` |
| `hermes.yml` | [`citadel-cli` 6fad6be, run 39946525020](https://github.com/aceteam-ai/citadel-cli/actions/runs/39946525020) | `6fad6be3951e28d6a4ad34a6e39e8c90113328ae` | `sha256:f8ba9d904b9330a76dc86363f00f13d0261273faba74688a44d59eda95772617` |
| `kokoro.yml` | [`citadel-services` ad043e8, run 36191665762](https://github.com/aceteam-ai/citadel-services/actions/runs/36191665762) | `sha-ad043e8` | `sha256:0f99603bc21c89b5bfbc941eaec0329805ba52b923b05414c26a111636452241` |
| `transcribe.yml` | [`citadel-cli` 6fad6be, run 39946524842](https://github.com/aceteam-ai/citadel-cli/actions/runs/39946524842) | `6fad6be3951e28d6a4ad34a6e39e8c90113328ae` | `sha256:becb1051fcbc1fbd0fc5cc34b2099a7915411311350e9e2a1c28fda283c07f66` |

Kokoro's publisher uses a `sha-` prefix and seven source characters for its
immutable tag. Its complete main source commit was
`ad043e87546cb294bd9309447a5fec408efa0312`; the digest, rather than the
short tag, is the pull identity. These registry and CI checks establish tag
and digest consistency, not independent verification of signed provenance or
runtime acceptance on every node architecture.

The Claude Code and Hermes files here are standalone examples; their live
catalog modules are owned by `citadel-services`. Other catalog compose files,
the AceTeam provisioning template, and third-party images remain separate
consumer work under `aceteam-ai/aceteam#12088`. A future sidecar update needs
a new reviewed source tag, registry digest and test expectation before its
compose reference changes. Existing running containers are unchanged until an
operator starts or updates that service.
