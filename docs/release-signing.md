# Release signing

Citadel's self-update installs a binary only when the release's
`checksums.txt` carries a valid signature from a release-signing key compiled
into the running binary. This document describes the signature format, how
the release flow produces it, how the updater checks it, and how keys rotate.

## What is signed

Every release publishes, in the same `gh release create` call:

- the platform archives (`citadel_<tag>_<os>_<arch>.tar.gz` / `.zip`);
- `checksums.txt`: `sha256sum` output, one `<64 hex>  <archive name>` line per
  archive;
- `checksums.txt.sig`: a detached signature over the **exact bytes** of
  `checksums.txt`.

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

`scripts/release.sh` (via hooks in `scripts/release-engine.sh`):

1. `hook_preflight` runs on every invocation, including `--dry-run` and a
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

- A release must carry its signature when it is created, because the
  signature cannot be attached later. The pre-publish check exists for this.
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
