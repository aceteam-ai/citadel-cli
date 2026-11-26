# Native embedding checkpoints

The first checkpoint adds an explicitly constructed, loopback-only protocol
adapter using the existing bounded embedding batch client and signed work
receipt path. It is not wired into worker construction, CLI flags, environment
selection, service discovery or engine startup. Existing TEI behavior remains
the default, including its model-alias compatibility.

The adapter requires the job to state the exact served model alias and vector
dimension. It refuses a missing or different response model and a different
vector dimension, before emitting a result or receipt. Receipt manifests bind
the model and dimensions through the existing signed hashes. Loopback endpoints
must use literal addresses and an explicit port; redirects and proxy environment
are not used. A healthy socket is not proof of verified runtime/model custody.

The [upstream server documentation](https://github.com/ggml-org/llama.cpp/tree/master/tools/server)
describes the health and embedding API paths and the model-alias option. Those
docs are protocol background, not a release pin or native compatibility proof.

Remaining work for #1930:

- An operator-visible cost/consent plan and reviewed, pinned runtime/model catalog.
- Checksum and signature admission before download/extraction/launch; no PATH fallback.
- Owner-local process custody, readiness and stop through the native lifecycle.
- Heartbeat/worker discovery and selection without changing an already-serving TEI.
- Cross-platform CI with an admitted real runtime and model, plus native acceptance.

No runtime release, model artifact or signature identity is invented or admitted
by this checkpoint. All tests use synthetic loopback HTTP services and in-memory
test receipt keys. This source work does not complete the parent issue.
