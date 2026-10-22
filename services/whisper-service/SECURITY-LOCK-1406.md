# Whisper dependency security refresh (#1406)

Source-only draft; no runtime/image/model download, execution or publication.

## Evidence and changes

CI run [39667914960](https://github.com/aceteam-ai/citadel-cli/actions/runs/39667914960)
ran the existing fail-closed audit on the unchanged main lock, reporting certifi
PYSEC-2026-4320 and eight Pillow advisories. This refresh changes only these two
package entries, adds explicit input pins, and preserves all other package
versions/hashes, the Torch/torchaudio CPU URLs/ABI, and the existing audit rules.

- certifi **2026.10.19**: the [primary advisory](https://github.com/certifi/python-certifi/security/advisories/GHSA-6q48-6c7w-9gw2)
  identifies the compromised emSign trust-chain incident; the repaired bundle
  supersedes 2026.7.22. [Official release metadata](https://pypi.org/pypi/certifi/2026.10.19/json)
  supplies two non-yanked artifact hashes, Python >=3.7 and no dependencies.
- Pillow **13.0.0**: [publisher release notes](https://pillow.readthedocs.io/en/stable/releasenotes/13.0.0.html)
  list fixes for all eight reported advisories: GHSA-p4wg-p73j-c528,
  GHSA-65qm-jvmh-6pqg, GHSA-58r4-6rv3-gfh8, GHSA-pchr-8j38-3x4p,
  GHSA-q45j-58j2-45rg, GHSA-462x-pgg6-55fp, GHSA-c6hw-5grf-ff96,
  GHSA-vx6w-vv9f-6gvg. [Official release metadata](https://pypi.org/pypi/Pillow/13.0.0/json)
  supplies 83 non-yanked artifact hashes. Python >=3.11 permits this image's
  Python 3.12; its CPython 3.12 manylinux_2_28 x86_64 wheel is published.
  Optional extras are not enabled, so this change adds no package dependency.

All replacement hashes are copied from that version's PyPI JSON `urls` digest
set, sorted and deduplicated; no artifact was downloaded or executed locally.
This is a targeted lock edit, **not** a rerun of the recorded uv compile command.
The source regression tests pin the target wheel hashes and expected hash counts;
they do not contact PyPI or prove installability.

## Compatibility review and remaining gates

Official PyPI metadata for the already locked reverse dependencies permits the
new pins: httpcore 1.0.9 and httpx 0.28.1 require certifi without an upper bound;
requests 2.34.2 requires certifi>=2023.5.7; matplotlib 3.11.2 requires pillow>=9.
Pillow 13 removes Python 3.10 support and several deprecated image APIs; this
service targets 3.12 and contains no direct PIL use. That source observation
does not prove transitive image/plotting behavior or the complete ML runtime.

Normal exact-head CI should run the existing hermetic app tests and dependency
audit without new ignore rules. Green audit/test receipts prove only their
tested scope. Full uv lock resolution, installed dependency checks, native
diarization/transcription and image build acceptance remain separately held;
do not claim this source patch performs those checks. Reconcile the targeted
lock with the documented compile command under the appropriate execution
custody before accepting a new runtime image. Independent source/security
review is still required. No workflow, image digest, deployment, live
credential/schema or service state is changed, and this grants no merge right.
