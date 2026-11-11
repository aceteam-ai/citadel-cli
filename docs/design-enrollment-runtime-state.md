# Enrollment runtime state proposal

Refs #1356, item 3. This is an unratified source design requiring independent
design review. It authorizes no migration, activation, ownership change,
credential operation or deployment. The proposed `platformStateDir` contract
below is not an existing implemented API. Exact platform paths, ownership and
upgrade admission must be agreed before implementation changes any default.

The purpose is to keep mutable runtime material out of configuration locations
such as `/etc/citadel`, while keeping every process serving one enrollment on
the same state. Merely replacing `platform.ConfigDir()` with another resolver
does not establish that property.

## Current source and authority

The following describes the audited source, not the contents of any machine.

| Material | Current producer and consumer | Current location and behavior |
| --- | --- | --- |
| Public fabric CA cache | `cmd/work.go` calls `status.EnsureFabricCABundle`; `internal/status/ca_bundle.go` fetches and writes it; `NewFabricCAVerifier` in `internal/status/mtls.go` consumes it | `platform.ConfigDir()/fabric-ca-bundle.pem`. Fetch failure or cache-write failure can reuse an existing file. A verifier still requires usable certificates and configured coordinator SANs. The cache has no persisted endpoint binding. |
| Gateway server certificate and private key | `cmd/work.go` and `cmd/serve.go` call `internal/tlscert.EnsureCert`; gateway certificate publication and heartbeat use `tlscert.CertPath` | Default `platform.ConfigDir()/tls/server.crt` and `server.key`. A reusable, unexpired matching pair is preserved; missing, invalid, expired or SAN-mismatched material currently triggers generation. `--gateway-cert-dir` and `serve --cert-dir` override the default. |
| Control-listener server certificate and private key | `cmd/work.go` calls `tlscert.EnsureCert` for the coordinator mTLS listener | `platform.ConfigDir()/control-tls/server.crt` and `server.key`. This server pair is distinct from the coordinator trust bundle and the enrolled receipt identity. |
| Pointer transaction lock | `withNodePointerLock` in `cmd/finetune_local_start.go` calls `finetunesafety.WithExclusive` in `internal/finetunesafety/path.go` | `<globalConfigFile>.pointer-safety/reservation.lock`; production callers generally pass `platform.ConfigDir()/config.yaml`. The directory is created with mode 0700 and the lock opened read/write with creation mode 0600. The inode must not be replaced while a holder or compatible contender can use it. |

Pointer-lock callers include manifest bootstrap and pointer publication in
`cmd/manifest.go`, scaffold creation in `cmd/init_service.go`, initialization
and credential-clear paths in `cmd/init.go`, and captured-node GPU admission
and restoration in `cmd/finetune_local_start.go`. This is an admission lock,
not a disposable cache file.

`platform.ConfigDir()` is intentionally invoker-scoped. Root, sudo and user
processes can therefore select different directories. Network state and
`network.GetNodeConfigDir()` instead use the machine state pointer, configured
node directory and owner-home fallback in `internal/network/state.go`.
`install.sh` separately reads the pointer in `resolve_node_config_dir` and
writes both `state-dir` and `config.yaml` in `write_machine_state_pointer`.
These paths cannot be migrated independently. Shell and Go currently also
handle whitespace and unsafe pointer entries differently; a shared policy
must resolve those differences without changing enrollment through fallback.

The public fabric CA cache must remain separate from two authoritative stores:

- Explicit private control-plane trust is persisted by `internal/catrust` as
  `<nodeConfigDir>/identity/control-ca.json`, with endpoint and fingerprint
  checks, after the applicable authentication outcome.
- `nodeidentity.Convergent` holds the enrolled key, leaf and chain under
  `<nodeConfigDir>/identity`. Its existing legacy-adoption rules preserve a
  registered key and do not authorize arbitrary replacement.

This proposal does not relocate or merge either store, use a receipt key as a
gateway key, change tsnet state, or treat a downloadable CA chain as proof of
enrollment. `CITADEL_FABRIC_CA_BUNDLE` remains an explicit externally managed
input; moving a default cache does not override that selection.

## Proposed runtime layout and selection

A reviewed `platformStateDir` resolver should take an explicit installation
scope and stable enrollment-owner profile. It must not derive ownership from
the current invocation's `$HOME`, elevation alone, or an arbitrary directory
that happens to exist. A user service and its interactive administrator must
resolve the same profile. A system-managed service must use its provisioned
profile. Ambiguous or inaccessible profiles must refuse affected operations
instead of selecting a fresh directory or generating replacement credentials.

Within that selected runtime root, retain recognizable relative names:
`fabric-ca-bundle.pem`, `tls/server.crt`, `tls/server.key`,
`control-tls/server.crt` and `control-tls/server.key`. Endpoint and migration
metadata are also runtime state; their exact schema needs review. The resolver
must supply the same chosen gateway path to creation, certificate publication,
heartbeat and diagnostics. Explicit certificate-directory overrides retain
their current meaning and require separate authorization to migrate.

The platform path map remains a prerequisite: define the Linux system and user
roots, macOS launchd/user roots, and Windows user/service roots and DACLs. An
unavailable user profile or Windows SYSTEM profile is not an invitation to
create another enrollment. The mapping must state which existing actors can
read public certificates, which can read private keys, and which can mutate
each directory. No added world/group access or elevated execution is implied.

## Preservation and offline behavior

Migration needs a bounded inventory of the selected profile's approved legacy
locations. Reading every home directory or adopting the first discovered key
is not an authority rule. Inventory identifies the installation owner, current
endpoints, explicit overrides, source permissions and all existing destination
material without emitting private material or raw parsing diagnostics.

A valid certificate/key pair is copied as the exact existing bytes, verified
for matching public key and the intended endpoint/SAN use before selection.
No incomplete pair is published. Missing, unreadable, expired, mismatched or
conflicting material produces a typed migration hold; it must not fall through
to `EnsureCert` generation merely because the new destination is empty. Any
later renewal remains a separately reviewed existing renewal operation, not a
migration side effect. Different valid source/destination pairs require an
explicit authority decision; modification time is insufficient. A destination
with identical approved bytes can be recognized as already copied.

The public fabric CA cache is not secret, but it controls accepted coordinator
identities. Any migrated cache needs approved provenance binding to the
authenticated control-plane origin and the selected profile, plus its verified
fingerprint. A filename, parseable PEM or self-supplied digest does not establish
that binding. Do not silently promote an unbound legacy cache to a new endpoint
or substitute it for explicit enrolled trust.

Preserve the existing offline availability requirement for an already
provisioned, proven cache. If legacy cache authority cannot be established,
leave the old selection intact pending review or hold the affected mTLS
listener. Do not weaken coordinator verification or copy unproven trust to
make migration succeed. First-start offline behavior stays fail closed.
Migration must not depend on a successful network fetch, rotate trust during
an outage, or report that a cache moved when only fresh material was fetched.

## Pointer lock and upgrade admission

The pointer transaction lock must have a stable identity derived from the
protected pointer's installation scope, independently of the pointer's current
contents. Putting it under retargetable `network.GetNodeConfigDir()` creates
two locks during retargeting and loses serialization. A per-invoker state root
has the same defect when two actors protect the same pointer.

Define a separately provisioned stable lock root and authorized actor set.
Moving the lock requires coordinated Go and installer changes, including every
pointer writer and admission/restore reader. Existing `state-dir`, global
`config.yaml` and manifest resolver differences need an explicit common
precedence and refusal policy. This does not authorize changing which enrolled
node those pointers select.

An old process can still acquire the old inode after a new process starts using
a new one. A one-time absence check is therefore insufficient. Before any lock
cutover, establish quiescence and an enforced compatibility/entrypoint policy
that prevents incompatible old writers or admission paths from re-entering.
Do not move or delete the old lock inode while it can have holders or
contenders. If that exclusion cannot be established, keep the existing lock
location and hold its migration. A dual-lock bridge would need its own reviewed
global acquisition order and old-version interoperability proof; this document
does not silently select that alternative.

Quiescence must cover workers, local service starts, restoration and trainers
whose custody is relevant. A process-list snapshot or an empty display counter
alone is not proof. Existing durable reservations and safety holds remain in
force and are neither cleared nor relocated by this proposal.

## Filesystem publication and rollback

The migration design must specify trusted directory roots and regular-file,
ancestor, symlink/reparse-point, ownership and permission checks on each
platform. It must define collision handling and serialization before mutation.
Check candidate files through the same opened objects used for bounded reads;
path-only checks do not supply race-free custody. Hardlinks, mount boundaries,
cross-device copies and Windows sharing/DACL semantics need explicit treatment.
Do not broaden private-key access, log secret paths/material, or follow a
hostile alias to repair permissions.

Publish checked copies through private temporary files and a durable commit
record. Specify file flush, atomic rename and directory durability guarantees
per platform, and what happens if any operation is unsupported or uncertain.
The two-file TLS pair needs a reviewed coherent-selection mechanism; two
independent renames are not a transactional pair. Retain original bytes until
acceptance and rollback policy permit removal. No code may interpret a partial
copy or uncertain commit as permission to regenerate.

Rollback must recover the selected prior pair and trust authority without
overwriting newer divergent material. Define the supported old-binary boundary
and the lock protocol throughout rollback. After renewal or another enrollment
changes either side, automatic rollback is held pending an explicit decision.
Neither deleting a migration marker nor restoring an old executable alone
proves safe recovery.

## Source slices and acceptance

1. Ratify the platform path/owner map, cache provenance and offline policy,
   conflict dispositions, lock identity and upgrade/rollback protocol. Keep
   unresolved cases held rather than guessing a path or authority.
2. Add pure profile/path-selection contracts and fake inventories with no
   default changes. Cover root/user/sudo/service convergence, separate scopes,
   overrides, invalid pointers and disagreement between shell and Go policy.
3. Prepare an inactive bounded migrator and transactional selection protocol.
   Fake filesystem operations cover identical/conflicting pairs, inaccessible
   and malformed sources, alias attacks, partial writes, interrupted rename,
   uncertain flush, cross-device refusal and repeat recovery. Fixtures must
   prove that none of those cases generates or rotates keys.
4. Prepare all consumers and installer pointer/lock participants together,
   retaining a disabled activation boundary until independent source review
   and compatibility acceptance pass. Add concurrent fake actors proving that
   pointer retargeting cannot produce two admission locks. Preserve enrolled
   identity and explicit overrides in every case.
5. Under separate authorization, test the exact upgrade and rollback on
   disposable platform installations: actual ownership/DACLs, file durability,
   service contexts, offline starts, gateway certificate continuity, coordinator
   verification, retained reservations and mixed-version exclusion. Only those
   results can support a later activation decision.

Source fixtures cannot prove native filesystem durability, backend certificate
pin continuity, operating-system service identity or live enrollment authority.
No local candidate parser, formatter, tests, native process, network request,
SQL, credential change, deployment or activation accompanies this document.
