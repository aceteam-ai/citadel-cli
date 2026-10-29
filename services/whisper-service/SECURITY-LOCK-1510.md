# OpenTelemetry runtime lock correction (#1510)

The existing [Whisper audit on #1452](https://github.com/aceteam-ai/citadel-cli/actions/runs/39965235647/job/120453252272)
reports `opentelemetry-api==1.45.0`, GHSA-xcq7-p3pf-p88h. The
[vendor advisory](https://github.com/open-telemetry/opentelemetry-python/security/advisories/GHSA-xcq7-p3pf-p88h)
identifies 1.45.1 as patched. The lightweight behavior-test environment had
already installed 1.45.1, while the audited runtime lock retained 1.45.0.

## Minimum compatible source change

The direct security input now pins API 1.45.1. Updating that entry alone would
break exact requirements from the old SDK and semantic conventions. The targeted
lock correction therefore changes these three entries together:

| Package | Before | After | Publisher metadata |
| --- | --- | --- | --- |
| opentelemetry-api | 1.45.0 | 1.45.1 | [PyPI](https://pypi.org/pypi/opentelemetry-api/1.45.1/json) |
| opentelemetry-sdk | 1.45.0 | 1.45.1 | [PyPI](https://pypi.org/pypi/opentelemetry-sdk/1.45.1/json) |
| opentelemetry-semantic-conventions | 0.66b0 | 0.66b1 | [PyPI](https://pypi.org/pypi/opentelemetry-semantic-conventions/0.66b1/json) |

Each replacement release has a non-yanked universal Python wheel and source
archive, with Python >=3.10 permitting this image's Python 3.12. The two hashes
for each entry come from that exact release's official PyPI digest set, sorted
and deduplicated. No artifact was downloaded or executed locally.

SDK 1.45.1 requires API 1.45.1 and semantic conventions 0.66b1; the latter also
requires API 1.45.1. Their typing-extensions requirements admit the existing pin.
The existing exporter common, gRPC and HTTP packages permit SDK `~=1.45.0`, which
includes 1.45.1; their API `~=1.15` constraint also admits 1.45.1. All seven other
OpenTelemetry entries remain unchanged, including their exact internal exporter,
transport and proto relationships. These checks use the exact locked releases'
publisher metadata, not an assumption that the whole release family must move.

The direct parents also admit the patch: pyannote.audio 4.0.7 requires API and SDK
>=1.34.0, and FastAPI 0.142.2 requires API >=1.44.0. The latter's optional
OpenTelemetry/standard extras require SDK >=1.44.0. This is a metadata constraint
check, not a proof of full installation or runtime behavior.

## Verification and remaining custody

This follows the targeted source-edit convention in SECURITY-LOCK-1406.md. It
is not a new full uv resolution and does not claim to have run the recorded
compile command. Preserve every unrelated lock entry, the exact Torch and
torchaudio CPU URLs/hashes and ABI, the current audit ignore list, workflows,
image digests and application behavior.

Require the original normal exact-head hermetic Whisper tests and dependency
audit, plus independent source/security review. Full resolver reconciliation,
installed dependency checks, runtime/native/model acceptance and a reviewed
image rebuild/digest update remain separate gates. No package installation into
the host, model/image download, inference, running-service change, publication,
deployment, SQL, credential change, undraft or merge is performed by this source
patch. It does not repair an already-published image or a running node.
