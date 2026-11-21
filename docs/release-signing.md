# Release signing

Citadel's self-update installs a binary only when the release's
`checksums.txt` carries a valid signature from a release-signing key compiled
into the running binary. This document describes the signature format, how
the release flow produces it, how the updater checks it, and how keys rotate.

## What is signed

Every published release contains these assets before it becomes immutable:

- the platform archives (`citadel_<tag>_<os>_<arch>.tar.gz` / `.zip`);
- `checksums.txt`: `sha256sum` output, one `<64 hex>  <archive name>` line per
  archive;
- `checksums.txt.sig`: a detached signature over the **exact bytes** of
  `checksums.txt`.

In the primary flow, CI creates the unsigned draft with archives and checksums;
the operator attaches the signature before publication. The emergency legacy
flow supplies all three asset kinds in its single release-create operation.

`checksums.txt.sig` is a single-line JSON object:

```json
{"v":1,"key_id":"acc280898c1b8791","sig":"<standard base64 of the 64-byte signature>"}
```

| Field | Meaning |
|-------|---------|
| `v` | Format version. Only `1` is accepted. |
| `key_id` | First 16 hex characters of SHA-256 over the raw 32-byte Ed25519 public key. It selects a key; it confers no trust by itself. |
| `sig` | Pure Ed25519 (RFC 8032, no pre-hash, no context) signature over the `checksums.txt` bytes. |

Unknown fields, other versions, a signature that is not exactly 64 bytes, and
trailing data are all rejected. `internal/releasesig` owns the format; both
the signer (`scripts/releasesign`) and the updater use it, so there is a
single implementation of every derivation.

Because the signature is over the raw file, it can also be checked with
OpenSSL 3 by extracting `sig` (base64-decoded) into a file and running
`openssl pkeyutl -verify -pubin -inkey <public key PEM> -rawin -in checksums.txt -sigfile <sig file>`.

## Binding to the release tag

The signature alone does not say which release a `checksums.txt` belongs to.
`releasesig.ParseChecksums` therefore requires every entry to name an archive
of exactly the tag being installed (`citadel_<tag>_...`), and the updater
looks up this platform's archive by exact filename. A validly signed
`checksums.txt` from one release cannot be presented for another: its entries
name a different tag. Tags must also match `v<major>.<minor>.<patch>` with an
optional pre-release suffix before they are used in any URL.

## Release flow

The primary path is a reviewed `VERSION` bump PR, then CI, then local signing.
No release key is stored in GitHub or given to the CI workflow.

1. After the main push CI completes successfully, `release-draft.yml` checks
   its same-repository/main/push identity. The helper requires a clean checkout
   at fresh origin/main, a real increasing version change in that exact commit,
   exactly one same-repository merged PR for it, and green exact-commit checks.
   The initial VERSION baseline and ordinary no-bump main merges draft nothing.
2. CI builds all six archives, including the Windows launcher, and writes
   `checksums.txt`. Go uses GOMAXPROCS=4 and package parallelism 4, with a hard
   four-CPU container cap. It deliberately does not use `-trimpath`: Go would
   omit the linker flags required by the updater's non-executing version parser.
   CI verifies archive consistency and each CLI's recorded main package, exact
   version stamp and OS/architecture using that same parser, rechecks fresh main
   and the gates, and creates an unsigned **draft** release whose tag name is
   the version and whose `target_commitish` is the exact source commit. CI
   pushes no tag (see "Where tags are created"). It never signs, publishes or
   updates Homebrew.
3. CI prints a `CITADEL_DRAFT_RECEIPT` in its Actions log binding the tag,
   source commit, merged PR, CI run and manifest SHA-256. The draft body holds
   a copy, but the signer requires equality with the successful workflow log.
   This prevents replacement of both the archives and unsigned manifest from
   being mistaken for the original CI output. Missing/deleted logs fail closed.
4. On the operator's offline-key release host, with a clean current main
   checkout and authenticated GitHub CLI, run:

   ```bash
   scripts/release.sh --sign-draft vX.Y.Z
   ```

   The command checks workflow/source/PR provenance, that the draft targets
   exactly the receipt commit, that the tag does not exist yet, and all
   exact-source gates, downloads assets by immutable ID with byte/time
   limits, compares the manifest to the CI log digest, and verifies every
   archive against the updater's tag-bound parser. Only then does it load the
   local key and sign. It self-verifies the signature, re-reads the draft to
   reject changes, uploads the signature, checks the upload, publishes by the
   original release ID, re-reads the created tag and requires it to point at
   exactly the receipt commit, and updates the Homebrew tap from verified
   checksums.

### Where tags are created

The repository's `protect-release-tags` ruleset covers `refs/tags/v*`
(creation, update, deletion, non-fast-forward) and lets only the admin
repository role bypass it. The drafting workflow runs as github-actions[bot],
so it cannot push a tag. The ruleset stays strict, so CI creates only the
draft: GitHub allows a draft release for a tag that does not exist yet and
creates the lightweight tag at `target_commitish` on publication. The admin's
`--sign-draft` publication is therefore the single place a release tag is
created. Before publishing, sign mode requires the draft target to equal the
receipt commit, that commit to be an ancestor of fresh origin/main, and no
remote tag of that name to exist, checked again immediately before the publish
call. After publishing it requires the new tag to be a lightweight ref at
exactly the receipt commit.

Drafts are not installable. Publication happens only after the signature is
present, before GitHub freezes the release assets. The compiled updater key set
and tag binding are unchanged. Existing local gentle-mode limits also apply to
the sign-draft command.

The earlier all-local flow remains available as operator-only emergency
recovery through `scripts/release.sh --legacy`, followed by its existing engine
options. Bare invocations refuse instead of accidentally cutting a release:

1. `hook_preflight` runs on legacy-engine invocations, including `--dry-run` and a
   resumed release, before anything is tagged. It loads the signing key and
   requires its public key to be in the compiled trusted set. A missing,
   unreadable, malformed or untrusted key fails the release. It prints only
   the key id.
2. `hook_build` writes `checksums.txt`, then runs
   `go run ./scripts/releasesign sign`, which refuses to sign a file that does
   not bind to the release tag and self-verifies the result against the
   compiled trusted set.
3. `hook_pre_publish` runs immediately before `gh release create` and refuses
   to publish unless `checksums.txt.sig` exists and verifies. A release
   published without its signature could never be fixed (see "Immutable
   releases" below).

The legacy `release.sh` at the repository root cannot sign and refuses to
run.

The key path is `CITADEL_RELEASE_SIGNING_KEY`, defaulting to
`$HOME/.config/citadel-release/release-signing-ed25519.pem` (PKCS#8 PEM,
mode 0600). The private key lives only on the offline release machine and its
owner-controlled backup.

### Refusals and recovery

- A bump whose CI was cancelled, is red, or is no longer fresh main is not
  drafted. Do not bypass a gate. Before moving main again, rerun failed CI if
  appropriate; otherwise prepare a fresh reviewed bump at current main.
- Any existing remote tag of the version name (at any commit), an existing
  release (including a partial draft), unknown check provider, or incomplete
  forge pagination refuses the CI draft. CI never pushes or overwrites a tag or
  assets. Inspect and reconcile an uncertain creation/upload manually; no write
  is automatically retried.
- Sign mode refuses a draft whose target differs from the receipt commit, and
  refuses if the tag already exists before publication (for example, created by
  someone else in between). Inspect the tag and release; do not delete or move
  a tag to make it pass. If the tag exists at another commit after publication,
  the command stops before the Homebrew step with an inspect-state error. A
  published retry is accepted only when the tag is at exactly the receipt
  commit, and then finishes only the tap.
- Local signing refuses a changed/foreign/published-unsigned draft, mismatched
  CI receipt, incomplete assets, changed bytes or an unverifiable signature.
  Do not replace a failed archive or manifest by hand: fix source and rebuild
  through a reviewed bump instead.
- If publication succeeded but Homebrew failed, rerun the local command. It
  accepts an already-published signed release only after the same provenance,
  manifest and signature checks, then finishes the tap without signing,
  uploading or publishing again. A signature uploaded before a failed publish
  is likewise verified and reused, never replaced.
- Download limits are 512 MiB per archive and 64 KiB per checksum/signature
  asset, with a 300-second per-asset timeout. There is no extraction step.
- The final re-read is not a transaction against a compromised GitHub writer.
  Publication uses the original release ID, and nodes still refuse any archive
  or manifest that no longer matches the trusted signature. Compromise of CI,
  reviewed source or the offline signing host is outside this proof.

## Version preparation helpers

`scripts/release_version.py` prepares local version edits and reads candidate
release snapshots. It does not authorize, build, sign, tag, upload or publish.
The draft workflow calls the planner after the CI and merged-PR gates. The
emergency all-local engine still derives its version from Git tags.

`VERSION` contains one canonical stable version and a final LF. Its initial
addition is a baseline, not a release candidate. On a feature branch, prepare
a version-only edit with one of:

```bash
python3 scripts/release_version.py bump minor
python3 scripts/release_version.py bump patch
python3 scripts/release_version.py bump 3.0.0
```

The bumper changes only `VERSION`, refuses main/production/detached branches,
refuses nonregular files, downgrades and uncommitted version edits, and checks
for a concurrent edit before replacement. It does not create a branch, commit,
tag or PR. Commit the edit through the normal hooks and reviewed-PR process.

The read-only planner accepts complete before/after commit IDs:

```bash
python3 scripts/release_version.py plan --before <BEFORE_COMMIT> --after <AFTER_COMMIT>
```

It reads the immutable Git blobs, never an uncommitted worktree version.
Both revisions must be commits and the before revision must be an ancestor of
after. A missing baseline, initial baseline or unchanged version returns a
non-candidate. Deletion, malformed versions, a downgrade or an existing target
tag naming a different commit is refused. An existing tag is accepted for a
retry only when it peels to exactly the after commit. Blob size and regular-file
mode are checked before reading version contents.

A `candidate: true` result is **not release authorization**. The CI caller must
separately prove the reviewed merged bump PR, its approved main snapshot and
all existing exact-head release gates, and fetch complete history and tags
before planning. A locally absent tag is not proof that no remote tag exists.
Archive consistency is a separate check
with `releasesign verify-assets`; it does not establish source/CI provenance or
signature trust. The updater's compiled-key and exact-tag verification is
unchanged.

Both CI and the local signer compare against remote version tags, not just
VERSION. After an emergency legacy release, prepare a bump above that remote
floor. A signed older-release retry is refused too, so it cannot roll back the
Homebrew tap. Publication uses GitHub's semver-based latest selection instead
of forcing an older release to become latest. The write-scoped CI job pins
action revisions and does not restore a build/module cache.

Never re-run the source CI or drafting workflow after a successful draft
receipt. This first version requires the successful original runs and their
latest logs/checks; a later failed attempt deliberately invalidates signing.
If a run was accidentally re-run, or main moved before drafting, leave that
draft untouched and prepare a new reviewed VERSION bump above the remote
floor. Do not edit the receipt, overwrite assets or weaken the green-check
gate to recover. A retry of the local sign-draft command is different: after
manual reconciliation it can reuse a verified uploaded signature or finish
the tap update, without re-running CI, re-signing or replacing assets.

The signer checks the asset snapshot again immediately before publication.
GitHub does not offer an atomic asset-set precondition on the release PATCH,
so a concurrent trusted release writer can still cause an availability failure
in that final interval. Immutable publication plus the updater's signature and
archive checks prevent altered bytes from being installed; they cannot ensure
that every raced release remains usable. Do not edit a draft being signed.

## Updater verification

`update.Client.DownloadAndVerify` is the single download path for the
periodic auto-updater, `citadel update install`, and the `AGENT_UPDATE`
handler. Before fetching the archive it:

1. fetches `checksums.txt` and `checksums.txt.sig` (both size-bounded);
2. verifies the signature against the compiled trusted keys
   (`releasesig.Verify`);
3. parses and tag-binds the signed checksums;
4. only then downloads the archive and compares its SHA-256.

Any failure is fatal: `update.ErrUnsignedRelease` when no signature is
published, `update.ErrReleaseSignature` for anything else. A validly signed
release that lists no archive for this platform fails with
`update.ErrPlatformNotPublished`. The archive download is capped at
`update.MaxArchiveSize` (512 MiB), so an oversized response is abandoned
before it can fill the disk. Nothing is extracted or installed on any of
these failures.

Retrying the same release cannot change any of these outcomes
(`update.IsTerminalReleaseError`), so an `AGENT_UPDATE` job that hits one is
a terminal failure on its first delivery. The periodic updater records
`signature_missing`, `signature_invalid` or `platform_not_published`
(`update.DownloadFailureResult`) rather than `download_failed`, and the
heartbeat `auto_update.last_result` reports the same code.

## Immutable releases

GitHub immutable releases were enabled on this repository on 2026-11-09.
Only releases created after that date are immutable: their assets cannot be
added, replaced or deleted. v2.212.0 and earlier remain mutable.

This matters in two ways:

- A release must carry its signature when it is published. Draft assets remain
  editable so CI can upload unsigned archives and the local operator can attach
  the signature before publication. No signature can be attached afterward.
- Nobody can delete `checksums.txt.sig` from an immutable release to block
  updates to it. On a mutable release, removing the signature would make
  every verifying node refuse that release. Since all signed releases are
  created after immutability was enabled, signed releases are immutable.

## Transition

Releases published before signing was introduced have no `checksums.txt.sig`.
A binary that contains this verifier refuses them, including as an explicit
`AGENT_UPDATE` target. Nodes running older binaries keep their old behaviour
until they install a signed release. Consequences:

- The first release containing the verifier must itself be produced by the
  signing release flow, so that the nodes which install it can also install
  the next one.
- After a node runs a verifying binary, it can move only to signed releases.
  The updater already refuses downgrades, so this removes no update path.

## Key rotation

The trusted set (`trustedKeys` in `internal/releasesig`) is a list so a
rotation never strands a node:

1. Generate the new key offline. Add its public key and key id to the trusted
   set. `TestCompiledKeyIDsMatchKeyBytes` checks every entry's id against its
   bytes.
2. Publish that release signed with the **old** key. Nodes install it and now
   trust both keys.
3. Once the fleet runs a binary that trusts the new key, sign subsequent
   releases with the new key.
4. In a later release, signed by the new key, remove the old key from the
   trusted set.

A node that skipped step 2's release still trusts only the old key. It can
update only while some newer release is signed by a key it trusts, so keep
the overlap long enough for the fleet's update cadence. If a key is
compromised, remove it in a release signed by the other key as soon as one is
trusted fleet-wide.

## Threat model

This protects against an attacker who can change release assets, release
metadata, or the bytes served on the download path, but who does not hold an
offline signing key. Such an attacker can deny updates; they cannot get a
node to install an archive whose digest is not in a `checksums.txt` signed by
a trusted key for that exact tag.

It does not protect against compromise of a trusted signing key, of the
machine that runs the release flow, or of the source tree before a release is
built. Downgrade refusal is a separate check in the install transaction
(`update.ApplyRelease`), which refuses a release older than the installed one
on every path. Installers that fetch the first binary outside the updater (the
shell and PowerShell installers, the Homebrew tap) are outside this verifier
and still check only the unsigned checksums.
