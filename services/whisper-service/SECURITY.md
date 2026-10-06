# Whisper sidecar dependency attestation

The runtime image installs `requirements.txt` with `pip --require-hashes`.
That lock resolves 107 packages for CPython 3.12 on Linux x86-64. Torch and
Torchaudio are matching 2.11.0 CPU wheels referenced by their exact official
PyTorch URLs and SHA-256 digests. No model weights are part of the image.

The HTTP sidecar remains published on host loopback by default and has no
independent bearer-auth lifecycle. Gated model access is authenticated with a
Hugging Face read token. Adding inbound HTTP auth requires a separate design
for shared-secret generation, storage, rotation, and worker injection; this
change does not pretend that model authentication supplies transport auth.
Pyannote's optional anonymous telemetry is disabled in the application,
container, and compose environment unless an operator explicitly opts in.

Regenerate the lock from `requirements.in` with the command in the generated
header, then run:

```sh
pip-audit -r requirements.txt --no-deps --disable-pip \
  --ignore-vuln GHSA-rrmf-rvhw-rf47 \
  --ignore-vuln GHSA-h35f-9h28-mq5c
```

The exceptions are narrow and currently unavoidable with the newest matching
CPU Torch/Torchaudio ABI published for Python 3.12:

- `GHSA-rrmf-rvhw-rf47` is a low-severity `torch.jit.script` memory-corruption
  issue fixed in Torch 2.13. This sidecar never imports or calls TorchScript.
  Torch 2.11 fixes the higher-severity `GHSA-vgrw-7cvw-pwgx` that affected the
  stale implementation's 2.8.0 pin.
- `GHSA-h35f-9h28-mq5c` affects Setuptools only while creating source
  distributions on macOS filesystems. Setuptools is present because Torch
  requires a version below 82. The Linux runtime never builds or publishes an
  sdist.

Pip Audit reports direct URL requirements as unversioned skips, so reviewers
must also verify the two wheel URLs and hashes in both input and lock. CI pins
those exact attestations and rejects a return to Torch 2.8.
