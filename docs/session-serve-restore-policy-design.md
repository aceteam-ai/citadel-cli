# Session serve: restore-only dependency policy design

Refs #1118 and #1109. This is a new design comparison against current source,
not a reconstruction of the lost private B1 draft or an implementation.

The [owner decision](https://github.com/aceteam-ai/citadel-cli/issues/1118#issuecomment-6347292050)
requires `citadel session serve` to refuse an identity needing login, point to
`citadel login`, and never initiate login or mint a node key. An empty auth-key
argument or a populated state directory does not establish that guarantee.
Keep implementation held until a separately reviewed dependency policy can
enforce it before the prohibited action, at startup and throughout reconnect.

Automated-citadel requested two alternatives: a small dependency patch with an
upstream proposal, and a persisted-profile precheck with a NeedsLogin watcher.
The recommendation is the complete immutable dependency policy below. Merely
skipping tsnet's explicit interactive-login call is insufficient. The watcher
alternative cannot meet the current invariant and must not become a fallback.

## Pinned evidence

`go.mod` pins `tailscale.com v1.100.0`. The findings below are bounded to that
version, not a claim about all future upstream implementations. References are
to the official tag; the same paths were inspected in the local module cache.
No actual profile, state store, credentials or running node was read.

| Boundary | Pinned source | Consequence |
| --- | --- | --- |
| Credential resolution and interactive startup | [tsnet.go](https://github.com/tailscale/tailscale/blob/v1.100.0/tsnet/tsnet.go#L721-L755), [backend start](https://github.com/tailscale/tailscale/blob/v1.100.0/tsnet/tsnet.go#L915-L968) | Empty `Server.AuthKey` can use ambient credentials; startup resolves credentials, starts the backend, then may request interactive login. Suppressing its log output changes none of those actions. |
| Backend authentication | [local.go](https://github.com/tailscale/tailscale/blob/v1.100.0/ipn/ipnlocal/local.go#L2912-L2952), [existing-key resume](https://github.com/tailscale/tailscale/blob/v1.100.0/ipn/ipnlocal/local.go#L3159-L3171) | Config/system policy can supply auth material. The backend also starts control-client login before returning; the later tsnet interactive check is not the only path. |
| Node-key mutation | [direct.go](https://github.com/tailscale/tailscale/blob/v1.100.0/control/controlclient/direct.go#L663-L752), [server retry](https://github.com/tailscale/tailscale/blob/v1.100.0/control/controlclient/direct.go#L875-L896), [retry caller](https://github.com/tailscale/tailscale/blob/v1.100.0/control/controlclient/direct.go#L575-L584) | Missing key, local expiry or interactive flags can generate a key. Server expiry/signature responses can trigger regeneration without waiting for an application watcher. |
| Persistent identity construction | [file store](https://github.com/tailscale/tailscale/blob/v1.100.0/ipn/store/stores.go#L158-L188), [profiles](https://github.com/tailscale/tailscale/blob/v1.100.0/ipn/ipnlocal/profiles.go#L915-L965), [machine key](https://github.com/tailscale/tailscale/blob/v1.100.0/ipn/ipnlocal/local.go#L4254-L4283) | A missing/empty store may be initialized; profile selection can migrate or create a profile; a missing machine key can be generated. A directory check does not prove an existing usable identity. |
| Expiry knowledge | [Persist](https://github.com/tailscale/tailscale/blob/v1.100.0/types/persist/persist.go#L21-L36), [netmap expiry](https://github.com/tailscale/tailscale/blob/v1.100.0/control/controlclient/direct.go#L1280-L1287) | Persist contains identity material but no expiry timestamp. Do not invent a native persisted expiry field or infer current server authorization from key presence. |
| Observer ordering | [LocalClient](https://github.com/tailscale/tailscale/blob/v1.100.0/tsnet/tsnet.go#L406-L415), [Up](https://github.com/tailscale/tailscale/blob/v1.100.0/tsnet/tsnet.go#L531-L545) | The exported local-client path starts tsnet before the application can subscribe. Up installs its watcher after this startup too. |

The method name `controlclient.Login` also covers resuming an already authorized
identity with its existing key. Automated-citadel's **reviewer interpretation of
the 2026-10-22 owner decision, pending Jason ratification** is:

- Permit control-plane resume/registration presenting the existing persisted
  node key of an already-authorized identity, with no interaction or new
  credential. This includes key-preserving `controlclient.Login` reconnect.
- Refuse browser/interactive auth URLs; auth-key, OAuth or WIF authorization,
  including ambient auth keys; every node-key mint/rotation/rekey; adoption of
  an empty/new profile; and any NeedsLogin/expired-key recovery except a clean
  refusal pointing to `citadel login`.

This interpretation is proposed design guidance, not a newly ratified exception
or runtime permission. Jason can overrule it. It separates existing-identity
exchange from new authorization; implementation remains held on ratification
and the enforcement proof below.

## Alternative A: immutable existing-identity policy

A minimal complete patch is broader than a `NoInteractiveLogin` switch in
tsnet. Define an immutable restore policy before store/profile/backend/control
construction, and propagate it to every operation that can create identity
material or initiate fresh authorization. Illustrative API names in a later
proposal must be labelled proposals, not available upstream APIs.

The policy needs these enforcement points:

1. Load only the explicitly selected existing store and native profile through
   reviewed native decoding. Missing, empty, malformed, unsupported or ambiguous
   identity refuses before initialization, migration or new-profile fallback.
   Enumerate supported native legacy formats; do not guess JSON fields.
2. Reject explicit and ambient auth-key, OAuth, WIF and force-login inputs, plus
   config/system-policy credential fallbacks. Avoid process-global environment
   mutation; the dependency must disregard or refuse those sources within this
   immutable policy. Legacy `citadel login` and ordinary work paths retain their
   existing behavior outside the new explicit serve path.
3. Before each new node-key operation, including local expiry, server-triggered
   regeneration, reconnect and interactive/local-API entrypoints, refuse with a
   typed requires-login result. Do not mint a key then discard it or merely
   reject its later disk write. The same ordering applies to fresh authorization
   requests and login-URL flows. A post-response error cannot undo a request.
4. Define persistent identity scope explicitly: node, machine, network-lock and
   any required attestation material. Missing required identity material refuses
   rather than constructing a different identity. Ordinary ephemeral transport
   cryptography is a separate category; do not claim to prohibit every key
   allocation in the process. Any permitted migration requires its own reviewed
   identity-preservation proof.
5. Make requires-login terminal for the restore owner. No automatic retry through
   the legacy reconnect/enrollment path, cached auth material or a different
   profile. Prevent concurrent profile/identity mutation under the same policy.
   Return only static guidance to `citadel login`, never a secret or login URL.

If the permitted existing-key exchange discovers server revocation, refusal must
happen before regeneration or a fresh authorization attempt. Existing-key
contact already sent to learn that status is not retroactively preventable;
that is part of the explicit exchange decision above. Local prechecks alone
cannot prove that the server still accepts the identity.

Do not broaden that client guarantee into a claim that a server cannot recreate
a node record for the same key. The pinned
[registration request/response](https://github.com/tailscale/tailscale/blob/v1.100.0/tailcfg/tailcfg.go#L1261-L1330)
supplies no existing-record-only condition or response proof of that property. If stable
server record identity is required beyond the no-login/no-node-key contract,
it needs a server contract and separate acceptance. An identity comparison
after receiving a response is detection, not prevention of server creation.

The dependency patch would be reviewed separately against the exact pinned
version, with an upstream proposal in parallel only when separately authorized.
A temporary `go.mod` replacement would need an immutable reviewed revision,
normal checksum/reproducibility review, a maintenance owner and a removal path.
This document adds no replace directive, fork, vendored source or upstream PR.

Residual risk: this is an enforcement design, not a proved patch. Missing a key,
credential, mutation or retry path would leave a violation window. The policy
must exist before constructors and remain enforced for their whole lifetime;
a guard installed after construction would retain the startup window.

## Alternative B: precheck and NeedsLogin watcher

The strongest caller-only variant would select the existing native profile,
reject visible auth inputs, check required saved key material, start networking,
request an initial-state bus snapshot, and cancel/close on NeedsLogin or watcher
failure. It can improve diagnosis and shorten continued operation after a
reported failure. It cannot prove an unexpired native Persist field, because
that field does not exist in this pin. A server check adds communication and
still cannot prevent the next lifetime expiry.

A reviewed backend-owned read-only witness would be needed for a trustworthy
pre-start native-profile check; this is not an existing exported preflight API.
Successful bus subscription
[acknowledges installed registration](https://github.com/tailscale/tailscale/blob/v1.100.0/ipn/ipnlocal/local.go#L3685-L3703), so there is
no invented extra installation gap after it succeeds. Its initial snapshot
does not cover actions that already occurred during startup.

| Window | What can happen before the application can refuse |
| --- | --- |
| Precheck to constructor | Store/profile selection can change or perform initialization; a file snapshot is not an atomic contract with later native reads. A cooperating-process lock reduces competition but cannot add dependency semantics. |
| Constructor/start to watcher | The public local client starts tsnet first. Backend auth, explicit interactive login and key generation can occur before subscription, even if the first delivered snapshot says NeedsLogin. |
| Runtime expiry/reconnect | Control-client regeneration can occur before an auth URL or NeedsLogin observation. Cancelling after notification cannot undo a generated or sent key. |
| Notification delivery and shutdown | Delivery, scheduling and close are observations after the event, not a veto on the mutation. A bus queue can drop notifications; clean close cannot prove a prohibited action never occurred. |

The pinned [IPN bus implementation](https://github.com/tailscale/tailscale/blob/v1.100.0/ipn/ipnlocal/local.go#L3588-L3602)
has a bounded notification queue, and its
[full-queue path](https://github.com/tailscale/tailscale/blob/v1.100.0/ipn/ipnlocal/local.go#L3906-L3920)
can drop delivery. Treating watcher closure or error as refusal helps fail closed
after detection; it does not create complete prior-event coverage. A private or
ForTesting hook is not an acceptable production workaround for pre-start access.

Alternative B therefore requires relaxing the owner's invariant to best-effort
detection after possible mutation. That change is not authorized or recommended.
A watcher may supplement Alternative A for UX and shutdown, never replace its
pre-action enforcement.

Do not race `Close` against `Start`: the pinned
[Close contract](https://github.com/tailscale/tailscale/blob/v1.100.0/tsnet/tsnet.go#L599-L651)
disallows that ordering. Likewise, Up is not a passive precheck: it starts first
and can clear saved serve configuration/service advertisements. Owned shutdown
must follow initialization's completion or supported cancellation contract;
cleanup cannot be described as rollback of earlier identity actions.

## Citadel lifecycle boundary and review gates

The later B1 source must retain the approved existing Presence/Worker mode,
explicit selected directory and strict worker ownership. Missing/invalid mode
refuses. General `NetworkServer.Connect` calls `EnsureStateDir`; existing
`VerifyOrReconnect` is a legacy retry path. Neither is a proven restore-only
adapter and neither should be reused merely with an empty auth key. Do not
silently attach a different machine-wide owner or fall back to another state
directory when explicit ownership cannot be established.

Publish shared session control and report ATTACH-ready only after owned restore
and controller readiness. ATTACH-ready is not backend-heartbeat or job-ready.
On error, requires-login, watcher failure or cancellation, stop and join owned
worker/updater activity, close the shared controller and mesh, and only then
release ownership. A timeout does not authorize an overlapping replacement.
This ordering is a proposed requirement, not implemented lifecycle evidence.

Before any runtime implementation, reviewers need:

- Ratification of key-preserving existing-identity exchange semantics, the
  persistent-key scope and supported native legacy formats.
- A complete dependency enforcement design and a separately authorized patch
  strategy. No production test hooks, unreviewed fork or vendoring.
- Fake boundary evidence for missing/corrupt/legacy profiles; ambient, config and
  policy credentials; local/server expiry; signature rejection; concurrent
  mutation; reconnect; and cancellation before, during and after startup.
  Assert zero forbidden key constructors and authorization requests, not merely
  unchanged final files or suppressed URLs. Preserve default dependency behavior
  outside the explicit restore policy.
- Citadel fixtures for mode preservation, selected-directory ownership,
  no fallback/re-enrollment, readiness and close/join-before-unlock ordering.
  Native Unix/Windows state/ACL, expiry and continuous-online acceptance stay
  separate from source review and normal CI.

This draft changes documentation only. No dependency, session serve command,
bare-entry preference, credentials, state, service, SQL, deployment or live
operation changes. No local candidate formatter/parser/tests/build/mesh execution
was performed. #1118, its runtime B1 boundary and native acceptance remain open.
