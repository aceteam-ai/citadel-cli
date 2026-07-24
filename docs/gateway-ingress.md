# Gateway ingress — expose node services with private/org/link visibility (#598)

Status: node-side v1 implemented. The MCP `expose` verb + console action are the
cross-repo (aceteam) follow-up that drives this.

## What this adds

A productized way to expose a node's **local** service (a dashboard, an engine
UI, Frigate's web UI, any internal tool) on the fabric through the **existing
Citadel gateway** — no parallel `tailscale serve` path. Every exposed service is
served under a dedicated `/expose/<name>/` namespace and gated by a page-style
**visibility ladder**:

| Visibility | Who can reach it | How it is enforced |
|------------|------------------|--------------------|
| `private`  | the creator only | caller's tailnet login == recorded `Creator` |
| `org`      | any same-owner mesh peer | `WhoIs(RemoteAddr).SameOwner == true` |
| `link`     | anyone with a valid token | signed, expiring, revocable HMAC token |

The verb is generic (`expose(service, visibility, ttl?)`) — the NVR/Frigate UI
(#597) is the first consumer, but nothing here is camera-specific.

## The layering that makes link/org work (read this)

The gateway's capability layer (`permissionMiddleware` → `categoryForRequest`)
gates builtin and `/modules/<prefix>/` routes and **fails closed**: a disabled
capability returns 403 and a sensitive surface without the node passcode returns
401 — *before* any later middleware runs. A `link` recipient has neither a
capability nor the node passcode (that is the whole point of a shareable link),
so exposed routes must **not** be gated by the capability switch.

Therefore:

- `categoryForPath` returns `""` (always-allowed) for `/expose/...`, so the
  capability gate never rejects a link/org caller first.
- `exposureMiddleware` (in `internal/gateway/exposure.go`) is the **sole** gate
  for `/expose/`, and it **fails closed**: an `/expose/<name>` with no registered
  policy is 404; any visibility check that cannot be affirmatively satisfied is
  denied.

This is verified through the **full `BuildHandler` chain** with maximally
restrictive permissions (`config.Permissions{}` — every capability disabled, no
passcode) in `exposure_test.go`: a valid link token must still reach the upstream.
An isolated middleware test would pass while the real chain still 403'd — that gap
is the reason the test drives the whole chain.

## Components

- `internal/gateway/exposure.go` — the primitive: `Visibility`, `ExposePolicy`,
  the per-Server exposure registry (`SetExposure`/`Expose`/`RemoveExposure`),
  `exposureMiddleware`, the `MeshIdentityResolver` interface (mirrors the #585
  terminal resolver so the package stays standalone/testable), and the `link`
  token mint/verify (`MintLinkToken`/`verifyLinkToken`, HMAC-SHA256).
- `internal/gateway/gateway.go` — `categoryForPath` always-allows `/expose/`;
  `BuildHandler` wraps `exposureMiddleware` closest to the mux; three `Server`
  fields (`exposures`, `exposeSigningKey`, `meshResolver`).
- `internal/config/expose_key.go` — `LoadOrCreateExposeSigningKey`: a dedicated
  per-node 32-byte HMAC key, persisted `0600`. (The passcode hash is bcrypt, not
  a raw HMAC key, and may be unset — so link tokens get their own key.)
- `cmd/gateway_mesh_auth.go` — `gatewayMeshResolver`: wires `network.WhoIsPeer`
  into the gateway's resolver interface (kept in cmd so the gateway package does
  not depend on internal/network).
- `cmd/serve.go`, `cmd/work.go` — wire the resolver + signing key into both
  gateway construction sites.
- `internal/worker/expose_set.go` — the `EXPOSE_SET` job handler (the node half
  of the `expose` MCP verb's contract): per-node-stream gated, validates the
  payload, calls the injected `ExposeOps`, returns `{url, token, expires_at}`.
- `cmd/expose_ops.go` — `liveExposeOps`: programs the in-process gateway
  (`gw.Expose`), mints the link token, and builds the managed mesh URL
  (`https://<vpnIP>:<gatewayPort>/expose/<name>`).

## `link` token model

`MintLinkToken(key, name, epoch, exp)` → `base64url(payload).base64url(hmac)`,
`payload = "<name>.<epoch>.<expUnix>"`. Verification (constant-time) requires:
signature valid under the node key, `name` matches the exposure, `epoch` matches
the exposure's current `TokenEpoch`, and `exp` is in the future. Revocation is
**revoke-all-for-exposure** by bumping `TokenEpoch` (re-expose with `epoch+1`);
every prior token then fails the epoch check. Per-token (`jti`) revocation is a
follow-up.

**Shareable URL:** the MCP verb composes it as `url + "?access_token=" + token`.
The bare `url` is not usable for a `link` exposure without the token.

**Browser sessions (why one token works for a whole SPA):** a browser opens the
shareable URL (token in the query), then fetches sub-resources (`/assets/app.js`,
`/api/config`) that carry NO `?access_token=`. On a valid explicit token the
middleware plants a path-scoped (`/expose/<name>/`), `Secure`, `HttpOnly`,
`SameSite=Lax` session cookie whose value is the same signed token, so every
subsequent request is re-verified (expiry included) and stays authorized. Without
this, only the first request (curl) would work and the web UI would 401 on its
first asset.

## Known scope / assumptions (v1)

- **`private` is inert end-to-end until aceteam passes a `Creator`.** A locally
  run expose cannot know a *remote* creator's tailnet login; it is an explicit
  payload field. `link` and `org` function fully node-only. Empty `Creator` fails
  closed.
- **`org` == same tailnet owner.** Mesh membership is treated as org membership,
  consistent with the #585 "TLS + mesh membership" posture. A finer org-membership
  check (beyond the tailnet owner) is a backend concern.
- **`link` widens authorization, not network reachability.** The managed URL is a
  `100.64.x` mesh address, reachable only by someone already on the mesh/LAN. A
  link removes the org-identity requirement for that reachable audience; it does
  not make the service publicly reachable.
- **Subpath serving constraint (applies to ALL visibility levels).** The route is
  served under `/expose/<name>/` with `StripPrefix`. An app that emits ABSOLUTE
  asset paths (`<script src="/assets/...">`) will have the browser resolve them at
  the gateway root → 404 — this is not auth, org/private hit it too. The exposed
  app must be configured to serve under a base path matching the prefix (e.g.
  Frigate's base-path config — the #597 nvr module is responsible for setting it
  to `/expose/<name>/`). Apps using relative paths work unchanged. A generic
  path-rewriting or per-service-subdomain answer is a follow-up if base-path
  config is not available for a given app.
- **WebSocket is enabled** on exposed routes, so live view / event streams
  (Frigate) upgrade through the gateway.
- **No tailnet HTTPS certs.** The gateway's own TLS on 8443 is the edge; nothing
  here depends on `tailscale cert` (which is disabled on the tailnet).
- **On-ramp requires the in-process gateway.** `EXPOSE_SET` needs `citadel work
  --gateway` (or `citadel serve`); with no in-process gateway it fails (retryable)
  rather than silently no-oping. Unlike modules, exposures are **not yet persisted**
  to a registry+watcher, so they do not survive a gateway restart — durable
  exposure state is a follow-up (mirror the provisioned-service registry).
- **No metering.** Exposed-service egress is not metered/billed yet (the issue's
  open question); the gateway does not wire `SetMetering` for `/expose/`.

## Cross-repo follow-up (aceteam)

Add the generic `expose(node, service, visibility, ttl?)` MCP verb + console
action that dispatches an `EXPOSE_SET` job to the node's **per-node** stream and
renders the returned `{url, token, expires_at}` (mirror `page_publish`
ergonomics: create → returns URL; re-call to change visibility / rotate the link).
The node side is ready for it.
