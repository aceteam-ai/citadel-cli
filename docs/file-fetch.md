# FILE_FETCH

`FILE_FETCH` is a typed worker job for staging coordinator-authorized Drive
documents larger than the binary mesh upload limit. It requires no template
registration or approval. This source change performs no node release or rollout;
the release remains paired with Film S1 (`video-compose` v1).

The platform must require exact-node `FILE_FETCH` support before routing a
staging request. The worker advertises that exact job type only when a workspace,
Files permission and a valid HTTPS coordinator origin are configured, and a
workspace probe confirms Linux anonymous staging and create-only FD publication.
Other operating systems and unsupported kernels, filesystems or sandbox policies
omit this capability. The probe links an empty anonymous inode to a random probe
name, checks create-only refusal, then removes that entry. Its result is cached
once per handler; rebuilding the handler or restarting the worker refreshes it.
Live Files permission and origin checks are not cached. Revocation
of Files removes the capability and refuses execution/publication. The origin
comes from the existing device configuration's `api_base_url`, read afresh, with
no job-supplied origin override.

Inputs (values arrive as strings through the legacy worker adapter; `max_bytes`
also accepts a finite integral JSON number within the same ceiling):

```json
{
  "url": "https://coordinator.example/api/shared/file-fetch",
  "capability": "body.signature",
  "dest_path": "chapters/film.mp4",
  "expected_sha256": "<64 hexadecimal characters>",
  "max_bytes": "536870912"
}
```

The URL must match the configured HTTPS origin and the literal endpoint
`jobs.FileFetchEndpointPath` exactly, without a trailing slash, path suffix,
query string, percent escape, credentials, fragment or explicit port. The
separate `capability` value is a URL-safe opaque token of at most 4096 bytes
(letters, digits, `_`, `-`, and `.`), sent only as
`Authorization: Bearer <capability>`. The
credential is never included in the request URL. Caller-supplied headers are
ignored. The node cannot verify an opaque HMAC token. The dedicated coordinator
endpoint must verify its purpose, org, node, document, stored object, expected
SHA-256, exact byte size and expiry
(maximum lifetime ten minutes), enforce single-use authorization, and audit mint
and fetch. Invalid or expired capabilities receive HTTP 401, 403 or 410.
This contract requires no new secret
or persistence on the node.

The handler performs a fixed HTTPS GET with no caller-provided headers/body,
redirects, environment proxy or compression. TLS verifies the origin using the
system trust store. Every connection resolves and validates all DNS answers;
numeric dial targets are pinned after rejecting private, local, mesh,
special-purpose and node-owned addresses. Reconnects repeat these checks.

`max_bytes` is required, positive and bounded by a 2 GiB node ceiling. The
coordinator may mint capabilities with a stricter ceiling. The handler caps
network execution at five minutes (or an earlier job deadline), honors hard and
cooperative cancellation, and requires SHA-256 verification before publication.
The destination is a clean workspace-relative path. An `O_TMPFILE` inode on the
workspace filesystem holds the download with mode 0600 and no directory entry.
After hash verification and sync, `linkat` publishes directly from its still-open
descriptor into a destination parent opened beneath the original `os.Root`.
Publication is atomic and **create-only**: an existing file, directory or symlink
is never replaced. An existing regular file yields `destination_exists` (other
invalid entries can yield `invalid_dest_path`); a concurrent destination creation
also refuses. Failed fetches close the anonymous inode and leave no staging name.
There is no named-temp or overwrite fallback. Subdirectory mounts that prevent
hard linking yield a publication refusal.

Publication first tries `linkat` with `AT_EMPTY_PATH`. Where an unprivileged
account receives `EPERM` or `ENOENT`, it uses the Linux `/proc/self/fd` alternative:
open and pin that directory, verify its filesystem is procfs, and `linkat` the
still-open staging FD entry with `AT_SYMLINK_FOLLOW`. Both paths link the same
verified inode and refuse existing destinations. Missing/unsupported primitives
fail closed; execution checks anonymous creation again before HTTP and refuses
publication errors. A cached capability check never replaces per-job filesystem
operations or publication checks.

This closes substitution of a visible staging filename between hashing and
publication. It does **not** isolate the worker from arbitrary malicious code
running under the same UID: such code can access process descriptors, move
directories or change published content. The hash authenticates bytes at
publication, not their integrity against later local modification.

For Drive references over 25 MiB, the platform must allocate a unique, previously
unused workspace-relative staging destination, dispatch it as `dest_path`, and
propagate the returned relative path into the manifest and node-backed FileValue.
Retries must allocate a fresh destination or explicitly handle `destination_exists`.
The existing small-file byte-write path retains its existing behavior.

Success returns `{ "path": "<exact workspace-relative dest_path>", "bytes": 10,
"sha256": "<verified digest>" }`. Refusals are sanitized JSON errors with a
stable `reason` and the message `FILE_FETCH refused`. Neither error bodies nor
handler logs include capability URLs, tokens, response content or node paths.
Mint/fetch audit belongs to the coordinator endpoint, not a generic proxy.

The receipt's `path` is exactly the validated `dest_path`, with no workspace
prefix or normalization after validation. Downstream node-backed references keep
that relative path and resolve it against the existing node workspace when
opening inputs; this does not change `RunNodeJob` file-open semantics. The shared
schema-v1 fixture `internal/jobs/testdata/file_fetch_v1.json` pins the request,
HTTP headers, response bytes, and relative receipt across the two repositories.

The legacy diagnostic job registry does not expose `FILE_FETCH`. The platform's
generic MCP job dispatcher must block `FILE_FETCH`, just as it blocks
`HTTP_PROXY`; only the dedicated authorized staging workflow may dispatch it.
