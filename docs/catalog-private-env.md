# Private catalog environment publication (#1416)

Status: source plan only, not an accepted contract or implementation. This draft
changes no runtime code, file mode, credential, service or deployment. #1357's
Frigate configuration repair is already merged; this is a separate catalog
environment-file boundary. Its catalog mirror and live acceptance remain open.

## Verified source problem

Base: `dc139d1de01239fc4d432990bef415d59f6b4e33`.
`internal/catalog/install.go:485` ends `writeEnvFile` with
`os.WriteFile(path, content, 0600)`. That mode applies to creation, not an
existing inode. A pre-existing `nvr.env` at 0644 stays broadly readable when the
installer writes MQTT/Wyze credentials into it. Fresh-file creation is already
private. No live environment file was inspected or exposure established.

Two existing read paths also affect a safe fix:

- `InstallFromManifest` reads persisted values before resolving config.
- `CarryGeneratedConfig` reads generated/carry values before
  `cmd/module_ops.go` uninstalls an installed module and deletes its environment
  file. It currently has no error result.

`readEnvFile` follows the supplied pathname and deliberately maps both missing
and unreadable files to an empty map. That can mint a new generated secret after
a read failure. Merely securing the subsequent write would not preserve the
credential a running consumer already holds.

## Existing primitives inspected

The Linux init-container writer in `cmd/nvrconfig/config_linux.go` anchors its
directory, opens existing files without following a final symlink, validates
type/owner/link count, and publishes an exclusive same-directory private temp by
rename. It is private to a Linux command and is not a portable catalog helper.

`internal/whatsapp/config.go`, `internal/memory/atomic.go` and
`internal/nodevault/vault.go` have private random-temp atomic writers. They do
not expose the checked-existing-file and cross-platform owner/ACL contract this
catalog boundary needs. Do not export them or create a broad filesystem
framework just to reuse a similar write loop.

The update package has Windows handle/reparse/owner/DACL validation patterns.
Its executable/update-lock ACL policy is not automatically suitable for module
secrets. A Windows mode of 0600 is not an owner-only DACL or native proof.

## Unaccepted decisions: obtain owner review before implementation

1. **Checked persistence reads and pre-teardown refusal.** Proposed replacement:
   a checked environment snapshot returns `(map[string]string, error)`; only an
   absent final file means no persisted values. Unreadable, unsafe-type,
   symlink/reparse, multilink or unacceptable-owner cases refuse rather than
   silently generating a replacement. `CarryGeneratedConfig` must gain an error
   result, and its production caller must return that failure before logging an
   update or invoking uninstall. This intentionally changes the old
   unreadable-means-absent behavior. It is not accepted by publishing this plan.
2. **Directory and ownership authority.** Proposed Unix authority is a pinned
   no-follow services-directory descriptor and verified regular, singly linked
   environment inode owned by the accepted directory owner. Root-created new
   temporary state would inherit the accepted parent uid/gid by descriptor,
   before writing bytes; existing supplied inodes would never be chowned.
   Whether this is the correct root/user migration policy needs explicit review.
   Do not silently repair an existing foreign owner or weaken file modes.
3. **Windows supported behavior.** Review a concrete protected-DACL creation,
   accepted-owner and no-reparse handle policy before implementation. Do not
   claim the inherited directory ACL plus `Chmod(0600)` proves privacy. Do not
   choose an unsupported/fail-closed Windows fallback without owner review:
   that itself changes working catalog installations. Other platform fallbacks
   also need an explicit supported contract, not a silent chmod-only path.
4. **Publication and durability outcome.** Same-directory rename/replacement is
   the publication commit point. Every refusal or injected failure before that
   point must leave the old bytes untouched. A failed directory durability sync
   after publication cannot honestly promise the old bytes survived; surface
   uncertain durability without blindly rolling back or deleting a replacement.
   The exact error/result representation needs review.

These are source-policy questions for automated-citadel/owner coordination, not
requests to inspect, chmod or rotate a live environment file.

## Invariants to retain

- Preserve resolution order exactly: explicit assignment/override wins;
  persisted generated/carry values are reused; defaults/required prompts apply
  as before for the remaining inputs. Do not resurrect stale non-generated
  settings or mutate the assignment map recorded for provenance.
- No automatic credential rotation as privacy remediation. Failed reads or
  failed publication must not be disguised as a successful install/reset.
- Build deterministic output by sorted keys. Preserve the documented existing
  parser's comments/blanks/first-`=` behavior; agree any new rejection or escaping
  rules before broadening this into a dotenv grammar rewrite. Test supported
  values without logging synthetic or real secret contents on failure.
- Never write secret bytes to a broad-mode temporary or destination file and
  chmod afterward. Establish the new inode's Unix mode or reviewed Windows ACL
  before its first sensitive write. Never chmod/chown through a pathname link.
- Validate existing targets before reading or preparing a replacement. Do not
  truncate or change their contents on refusal. Recheck the accepted destination
  identity before publication; do not follow an unexpected replacement.
- The node services directory and ancestors remain trusted. An advisory lock or
  check-then-rename sequence cannot defeat a malicious directory owner. Do not
  claim atomic compare-and-unlink or delete a changed final path during cleanup.
- Concurrency must not add silent credential regeneration. If a new serialization
  mechanism is necessary, review its persistent-inode and ownership semantics
  separately rather than borrowing the destination inode as an atomic-write lock.

## Proposed delivery after the decisions are accepted

1. Add the narrow checked read/publication helpers and platform-specific policy
   within `internal/catalog`, with static no-secret refusal diagnostics.
2. Thread checked read errors through install and the pre-teardown carry call.
   Preserve all existing generated/carry/override precedence regressions.
3. Add synthetic new/0600/0644 publication, immutable-old-byte failure,
   symlink/hardlink/nonregular/owner refusal, unreadable preservation,
   deterministic supported-value round-trip, generated-secret stability and
   pre-uninstall abort tests. Include injected preparation/write/sync/rename
   failures with the pre/post-commit distinction above.
4. Use normal existing GitHub CI for source tests and supported-target compile
   contracts. Existing Linux tests and cross-compiles do not establish native
   macOS or Windows ownership/ACL/rename acceptance.

No local candidate test, formatter, vet, build, dependency acquisition or native
execution is authorized for this issue. Keep the PR DRAFT/WIP. No host file
inspection/remediation, chmod/chown, live key/config/credential change or
rotation, container/image action, service restart, schema/SQL application,
undraft, merge or deployment is included. A future source merge alone would not
repair already-installed files; operator acceptance/remediation remains separate.
