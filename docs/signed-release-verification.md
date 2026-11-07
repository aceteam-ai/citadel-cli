# Signed release verification source contract

Refs aceteam-ai/aceteam#12089.

`internal/releaseverify` is an inactive proposed Ed25519 producer and verifier.
It has no caller from `internal/update`, an executable, `scripts/release.sh`, or
the release engine. It loads no key or host path, performs no network request,
and contains no production trust root. The existing updater still accepts
unsigned release checksums. This source slice does not remediate live nodes.

## Authenticated data

`EncodeManifest` and `Sign` produce a version-1 statement binding the exact
`aceteam-ai/citadel-cli` repository, positive GitHub release ID, tag, full source
commit, and each archive's exact name, byte size and SHA-256. The artifact list
is sorted and duplicate-free. Version-1 names cover the six current platform
archives and cannot be paths. The canonical JSON representation rejects unknown
fields, duplicate keys, additional values and alternate whitespace.

Signatures cover a fixed protocol domain, the selected key ID and the complete
manifest bytes. The detached signature file supports bounded overlap during
rotation. A key ID only selects among caller-supplied trusted roots; no asset,
manifest or signature can introduce a trusted key. A manifest needs at least
one valid signature from that policy. Malformed proofs or trust policies refuse.

`Checksums` derives the complete canonical `checksums.txt` from the signed
artifact records. `Verified.VerifyChecksums` compares that exact projection,
including filenames, ordering and all records. A modified checksum, substring
filename, duplicate record or additional entry cannot authorize an archive.
This proposal authenticates the checksum entries through the signed manifest;
it does not accept a detached signature over an unrelated checksum file.

`Verify` separately requires a published immutable release observation, with
matching repository, release ID, tag and resolved commit. Its future adapter
must obtain this observation from the fixed GitHub API origin and resolve the
tag to a full commit. A false/missing immutable value or a draft refuses. The
observation is not a cryptographic proof by itself: the signature supplies the
independent authority. Callers must retain current-version/downgrade checks,
channel policy and their exact requested target before accepting any result.
The module alone does not provide freshness, persisted rollback protection,
transport authorization or revocation discovery.

A zero `Verified` value authorizes nothing. `VerifyArchive` reads at most the
signed archive size plus one byte, verifies exact size and digest, and never
extracts, chmods, launches or installs a binary. The future updater must verify
in its owned staging file before extraction and keep verification and use bound
to the same immutable file descriptor. No unverified URL supplies an install
path. Metadata is capped at 64 KiB, signatures at 8 KiB/eight proofs, records at
32, and a signed archive at 1 GiB. These are source-contract limits for review,
not a ratified runtime configuration.

## Release producer and rollout prerequisites

The proposed production identity and key custody still require Jason's choice.
The local Ed25519 proposal is not a decision to provision an offline key instead
of a keyless identity. No production private key may appear in an issue, source,
console argument, diagnostic log or fixture.

After that decision, a separate paired producer change must implement this
contract in `scripts/release.sh` and its release engine before shipping node
enforcement:

1. Jason enables repository release immutability and the approved tag rules.
   The lane does not change repository settings or tags.
2. Build and hash every archive from the exact tagged commit. Create a draft
   release to obtain its stable release ID; resolve and compare the tag commit.
3. Build the sorted manifest and canonical checksum projection from the same
   owned artifacts. Have the approved isolated signer sign the manifest, then
   verify the result locally against the selected public root. Resume may reuse
   an exact matching draft, never a published release or different tag/commit.
4. Upload all archives, checksums, manifest and signatures to that draft. Verify
   their exact bytes, names and sizes before publication. Publish only once all
   artifacts are present, then require `immutable: true` and matching metadata.
   Do not advertise the updater cutover or advance distribution pointers on
   failure. Immutable releases cannot be repaired by replacing assets.
5. Approve and ship the compiled public roots and the fixed-origin bounded
   fetch/staging adapter together with the verified producer. Require signatures
   and immutable metadata before extraction on every update/install path. Missing
   signatures must refuse rather than fall back to unsigned checksums.

A signer or upload failure must stop publication. The current release engine's
suppressed artifact-hook errors must not survive that integration. Initial
installation, alternate installers and legacy update paths need an explicit
inventory; protecting one function is not sufficient.

GitHub recommends draft creation, asset upload, then publication for immutable
releases. Only assets and the tag are immutable; title, notes, prerelease and
latest markers can still change. See [GitHub's immutable release contract](https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases)
and [the CLI release publishing behavior](https://cli.github.com/manual/gh_release_create).
The cryptography is Go's standard [Ed25519 implementation](https://go.dev/src/crypto/ed25519/ed25519.go), with no new dependency.

## Key rotation and compromise

No root is fetched from the release being verified. Routine rotation first
ships a reviewed trust policy containing both the old and next public keys,
while releases carry proofs under both. After operators verify that the fleet
has the overlapping policy, new releases may use the next signer alone. A later
reviewed binary removes the retired root. Offline fixtures cover each phase,
including refusing a next-only proof before introduction and an old-only proof
after removal. Key IDs and public-key fingerprints belong in the operator's
reviewed record; this module does not discover them.

A compromised old signer is not safely rotated by using that signer to authorize
its replacement. Emergency recovery requires a trusted operator/distribution
channel and a newly reviewed root policy, including decisions for offline or
outdated nodes. Old binaries retain their old trust until replaced. Multiple
proofs implement compatibility overlap, not a threshold quorum or automatic
revocation. No claim of emergency revocation is made by this source slice.

## Offline proof

The committed manifest, signature and plain-text archive fixture use only a
published RFC test key. Tests verify the signature, complete checksum projection,
archive size/hash, release identity and immutable/draft conditions. They refuse
unknown/retired keys, altered signatures, cross-domain signatures, metadata
ambiguity, unlisted archive names and bounded-read violations. No real release,
node, signing credential, archive execution or service is involved.
