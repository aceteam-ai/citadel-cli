# FILE_FETCH

`FILE_FETCH` is a typed worker job for staging coordinator-authorized Drive
documents larger than the binary mesh upload limit. It requires no template
registration or approval. This source change performs no node release or rollout;
the release remains paired with Film S1 (`video-compose` v1).

The platform must require exact-node `FILE_FETCH` support before routing a
staging request. The worker advertises that exact job type only when a workspace,
Files permission and a valid HTTPS coordinator origin are configured. Revocation
of Files removes the capability and refuses execution/publication. The origin
comes from the existing device configuration's `api_base_url`, read afresh, with
no job-supplied origin override.

Inputs (values arrive as strings through the legacy worker adapter; `max_bytes`
also accepts a finite integral JSON number within the same ceiling):

```json
{
  "url": "https://coordinator.example/api/shared/file-fetch/body.signature",
  "dest_path": "chapters/film.mp4",
  "expected_sha256": "<64 hexadecimal characters>",
  "max_bytes": "536870912"
}
```

The URL must match the configured HTTPS origin exactly and the literal endpoint
prefix `jobs.FileFetchEndpointPrefix`. Its sole suffix is a URL-safe opaque token
segment (letters, digits, `_`, `-`, and `.`); query strings, percent escapes,
credentials, fragments and explicit ports are refused. The node cannot verify
an opaque HMAC token. The dedicated coordinator endpoint must verify its purpose,
org, node, document, stored object, expected SHA-256, exact byte size and expiry
(maximum lifetime ten minutes), and audit mint and fetch. Invalid or expired
capabilities receive HTTP 401, 403 or 410. This contract requires no new secret
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
The destination is a clean workspace-relative path. Filesystem operations use a
single `os.Root` descriptor, including temporary-file cleanup and atomic rename;
ancestor symlink replacement cannot redirect publication outside that root.
Temporary files use exclusive creation and mode 0600. A failed fetch preserves
an existing destination and removes its temporary staging file. Subdirectory
mounts that prevent an atomic rename yield a publication refusal.

Success returns `{ "path": "<absolute workspace path>", "bytes": 10,
"sha256": "<verified digest>" }`. Refusals are sanitized JSON errors with a
stable `reason` and the message `FILE_FETCH refused`. Neither error bodies nor
handler logs include capability URLs, tokens, response content or node paths.
Mint/fetch audit belongs to the coordinator endpoint, not a generic proxy.

The legacy diagnostic job registry does not expose `FILE_FETCH`. The platform's
generic MCP job dispatcher must block `FILE_FETCH`, just as it blocks
`HTTP_PROXY`; only the dedicated authorized staging workflow may dispatch it.
