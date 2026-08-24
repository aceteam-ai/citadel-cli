# Node Master PIN + At-Rest Encryption — Design Doc

**Status:** Design only. No implementation in this PR. Implementation follows
in a separate PR after this doc is approved.

**Issue:** aceteam-ai/citadel-cli#796
**Consumer:** aceteam-ai/citadel-cli#795 (encrypted, PIN-unlocked browser profile)
**Reconciles:** aceteam-ai/citadel-cli#753 (existing node passcode gate)

## TL;DR

#796 asks for one PIN that both gates online access and encrypts data at
rest. That can't be built as specified and still be truthful: a 4-digit PIN
is 10,000 guesses, and an attacker who has stolen the disk also has the
salt, the KDF params, and any node-held secret co-located with them — a slow
KDF raises the cost per guess, not the guess space, so 10,000 x
(KDF cost) is still a bounded, offline, parallelizable search. "The node
can't decrypt unattended" is not a true claim if the key that unlocks it is
a 4-digit number sitting on the same disk.

So this doc splits the single secret into two, and treats that split as the
central design decision, not an implementation detail:

- **Access PIN** (4 digits) — gates online use of the node (console, VNC,
  files, shell, browser sessions). Rate-limited, lockout-gated, always
  online-only. Reuses the existing passcode primitive (`config.Permissions`,
  #753/#755/#757/#758/#760) rather than adding a second one.
- **At-rest passphrase** — a separate, higher-entropy secret. It is the only
  key material for encrypting data at rest (`internal/nodevault`, new). It is
  never derivable from the access PIN.

This is a deliberate deviation from the issue's "single secret, single UX"
framing, made explicitly, not silently — see §1. Everything below explains
why, what the split costs in UX, and how the two primitives share one node
without becoming two separate products.

## Resolved decisions

The maintainer reviewed this design and resolved the questions this doc
originally left open (§10 below). These are now locked; the implementation
PR should follow them as written, not re-derive them:

1. **Boot-critical secrets (device bearer token, tsnet machine key) are
   excluded from v1's vault scope**, confirmed (§2). A future phase may seal
   them behind a "prompt on every reboot" trade-off; that is out of scope
   for this design and its implementation PR.
2. **The access-PIN lockout counter is a single node-wide counter**, not
   scoped per caller identity. It is persisted under
   `network.GetNodeConfigDir()` in the sibling state file proposed in §5
   (e.g. `passcode_state.yaml`), alongside the lockout-until timestamp and
   last-verified-at.
3. **`permissions.yaml` / `PasscodeHash` stays under `platform.ConfigDir()`
   in v1 — not migrated.** It works today because each gate's reader and
   writer are co-located per-process (§5). Only the *new* lockout/attempt
   state (item 2) moves to `network.GetNodeConfigDir()`; the migration of
   `permissions.yaml` itself is deferred, not part of v1.
4. **The DEK-wrap and data-encryption primitive is AES-256-GCM (stdlib)**,
   not NaCl secretbox.
5. **Session lifecycle for v1 is explicit `Lock()` + process exit only —
   no auto-idle timeout.** A `Session` (and therefore `vault_unlocked` in
   the heartbeat, §9) stays true until the caller explicitly locks it or the
   process exits; there is no shorter idle-based expiry in v1.
6. **The weekly re-prompt window is sliding**: it resets on every correct
   entry, and re-prompts once more than 7 days have passed since the *last*
   correct entry (not a fixed period from first entry).
7. **Heartbeat fields are `has_vault_passphrase` and `vault_unlocked`**,
   both without `omitempty` — `false` is a meaningful, reportable state for
   both, mirroring the existing `has_passcode` field's reasoning.
8. **Argon2id v1 defaults (`time=3, memory=64MiB, threads=4, keyLen=32`)
   are kept as proposed.** Validating them against the real range of node
   hardware (a headless 3090 box vs. a modest laptop) is a **pre-ship task
   for the implementation PR**, not a design blocker here.

## 1. Two secrets, not one — the deviation and why

The issue text says "single secret, single UX." This doc deliberately does
not build that, because the issue's other requirement — a truthful
"zero-knowledge / node can't decrypt unattended" claim — is incompatible
with a 4-digit key. Both requirements are in the issue; they conflict; this
doc picks correctness over UX minimalism and designs the UX to make the cost
bearable instead of hiding the conflict.

**The reconciliation:** the at-rest passphrase is prompted **only when a
user actually touches an encrypted surface** — today that means #795's
encrypted browser profile, at profile-mount time. A user who never uses an
encrypted surface never sees the passphrase prompt at all; their entire
experience is the 4-digit access PIN, which is what the issue's UX
expectation actually optimizes for in practice (most sessions are
console/VNC/shell, not the encrypted profile). The passphrase is opt-in by
usage, not by a separate setup step nobody takes.

This is the one place this doc knowingly overrides the issue's stated
design. Everything else below (weekly cache, lockout, loud disclosures, no
recovery, extensibility for scoped PINs) follows the issue as written.

## 2. Threat model

| Protected | Not protected |
|---|---|
| Disk theft or backup exfiltration of the vault + encrypted blobs, **when the passphrase is unknown to the attacker** — ciphertext only, DEK is not recoverable from disk contents alone | The vault's own files (salt, wrapped DEK, encrypted blobs) sitting *next to* a known or guessed passphrase — at that point decryption is trivial; passphrase secrecy is the entire boundary |
| The node encrypting data while genuinely unattended (no session unlocked) — nothing on disk decrypts without the passphrase being supplied fresh | A live, unlocked session: the DEK is resident in process memory for the session's duration; malware or an attacker with code-exec as the same user during that window reads plaintext same as the user does |
| A stolen/powered-off node — vault stays sealed; no cached plaintext key survives a reboot (resolved: no unlock persists across restarts — v1 session lifecycle is explicit `Lock()` + process exit only, see Resolved decisions) | The access PIN, at any point — it is an online authorization gate only and never touches at-rest key material. A leaked or brute-forced PIN grants console/VNC/shell/files access but decrypts nothing |
| Offline brute force of the passphrase, to the extent Argon2id cost + real passphrase entropy make it infeasible in practice | A weak or reused passphrase — Argon2id raises cost, it does not manufacture entropy that isn't there. This doc does not propose enforced passphrase complexity, only setup-time disclosure (see §6) |
| — | Boot-critical secrets the node needs with **no human present** — the device bearer token (`internal/config/devicecreds.go`) and the tsnet machine key under the network state dir. These are explicitly **out of scope for v1** (see below) |

**Why boot-critical secrets are excluded, not "the gap this fills."** The
worker needs the device bearer token at process start, and tsnet needs its
machine key to bring the mesh connection up — both with zero human
interaction, on every boot including an unattended reboot after a power
loss. Sealing either behind a user-supplied passphrase would mean the node
cannot rejoin the network or authenticate to the backend without a human
present at every boot, which contradicts what "unattended" means for a
fabric node's core function. Covering them is a **product decision**
(accept "prompt on every reboot" as a trade-off) rather than a crypto gap
this design should silently claim to close. **Resolved: excluded from v1**
(see Resolved decisions) — a future phase may revisit the trade-off, but it
is not part of this design or its implementation PR.

## 3. Key hierarchy — envelope encryption

```
passphrase --Argon2id(salt, params)--> KEK (32 bytes)
KEK  --wrap-->  DEK (32 bytes, random, generated once at SetPassphrase)
DEK  --AES-256-GCM-->  ciphertext (per-blob random nonce)
```

**Why envelope, not `passphrase -> key -> data` directly:** changing the
passphrase must re-wrap the DEK, not re-encrypt every blob under a new key.
For #795's browser profile this matters immediately — a profile reset or a
passphrase rotation should be a small, fast operation, not a full re-encrypt
of session data. It's also the only structure that lets a second unlock
method (recovery key, hardware token) wrap the *same* DEK later without
touching already-encrypted data — see §7 (Extensibility).

**Primitives:**
- Argon2id (`golang.org/x/crypto/argon2`) for passphrase -> KEK. **No new
  dependency** — `golang.org/x/crypto v0.52.0` is already a direct
  dependency (it backs the existing `bcrypt` passcode hash in
  `internal/config/permissions.go`), and `argon2` ships in the same module.
- **AES-256-GCM (`crypto/aes` + `crypto/cipher`, stdlib) for both
  DEK-wraps-KEK and DEK-encrypts-data — resolved (see Resolved decisions).**
  Introduces no new dependency and gets hardware AES-NI acceleration on
  essentially every node this runs on. `golang.org/x/crypto/nacl/secretbox`
  was considered as a drop-in alternative (same vendored module) but is not
  the chosen primitive.
- **Argon2id v1 defaults — resolved (see Resolved decisions):** `time=3,
  memory=64*1024 (64 MiB), threads=4, keyLen=32`. Validating these against
  real node hardware (a headless 3090 box and a laptop are very different
  KDF budgets) is a pre-ship task for the implementation PR, not an open
  design question. The chosen params are stored **in the vault header
  itself**, so a future default change never breaks an existing vault —
  each vault always records the params it was actually created with.

**On-disk layout.** The vault lives under `network.GetNodeConfigDir()`, not
`platform.ConfigDir()`. `platform.ConfigDir()` is invoker-scoped —
`platform.resolveConfigDir` picks a different path depending on whether the
caller is root, and CLAUDE.md is explicit that this makes it wrong for state
one process writes and a different invocation context reads.
`network.GetNodeConfigDir()` is the machine-convergent directory built for
exactly that cross-context case (per #383/#726). The vault is a clean case
of it: an interactive `citadel` invocation sets the passphrase, and a
long-lived `citadel work` (often systemd-root) is the process that later
needs to unseal — the same divergence #726 already documents for the
heartbeat freshness marker.

Proposed: `{GetNodeConfigDir()}/vault/vault.yaml`, directory mode `0700`,
file mode `0600`. Contents: a version byte, the Argon2id salt + params used,
the wrapped DEK, and the wrap nonce. No plaintext key material is ever
written to disk.

## 4. Seal/Unseal API sketch (`internal/nodevault`)

Interface only — no implementation, no method bodies. This is the shape a
consumer like #795 codes against.

```go
package nodevault

// Vault is the sealed at-rest secret store for this node. One vault per
// node, backed by network.GetNodeConfigDir().
type Vault interface {
	// IsConfigured reports whether an at-rest passphrase has been set.
	IsConfigured() bool

	// SetPassphrase initializes the vault: generates a random DEK, derives a
	// KEK from passphrase via Argon2id, wraps the DEK, and persists the
	// vault file. Errors if already configured — use ChangePassphrase to
	// rotate.
	SetPassphrase(passphrase string) error

	// ChangePassphrase re-derives the KEK from newPassphrase and re-wraps
	// the EXISTING DEK. Does not touch already-encrypted data. Requires the
	// current passphrase.
	ChangePassphrase(oldPassphrase, newPassphrase string) error

	// Unlock derives the KEK from passphrase, unwraps the DEK, and returns a
	// Session for Seal/Unseal. Fails closed on a wrong passphrase; no
	// partial or degraded unlock exists.
	Unlock(passphrase string) (Session, error)

	// Lock discards any cached session/DEK material held by this Vault.
	Lock()
}

// Session is a short-lived unlocked handle. The DEK lives only in the
// memory backing a Session — Seal/Unseal never touch disk except through
// the caller's own ciphertext storage.
type Session interface {
	// Seal encrypts plaintext under the DEK. Returns a self-describing
	// ciphertext (nonce + params needed to Unseal it later).
	Seal(plaintext []byte) (ciphertext []byte, err error)

	// Unseal decrypts a ciphertext previously produced by Seal. Errors
	// (does not return partial plaintext) on any integrity failure.
	Unseal(ciphertext []byte) (plaintext []byte, err error)

	// IsUnlocked reports whether this session's DEK is still resident.
	IsUnlocked() bool
}
```

A consumer (e.g. #795) calls `Unlock(passphrase)` at the moment a user
mounts the encrypted browser profile, holds the returned `Session` for the
lifetime of that use, and calls `Lock()` when done. **Resolved (see Resolved
decisions): v1 has no auto-idle timeout** — a `Session` stays unlocked until
the caller explicitly calls `Lock()` or the process exits, full stop. A
consumer that wants a shorter effective window (e.g. locking a browser
profile when its own session ends) is responsible for calling `Lock()`
itself; `internal/nodevault` does not time it out on their behalf.
`Seal`/`Unseal` only work through a live `Session`; there is no bare
"decrypt with passphrase" call, so nothing outside this package ever holds
the KEK or DEK directly.

## 5. Access-PIN behavior

**Reuse, don't duplicate.** The access PIN should be implemented as the
*same* field as today's passcode: `config.Permissions.PasscodeHash`
(`internal/config/permissions.go:51`), verified via
`Permissions.VerifyPasscode` and set via `Permissions.SetPasscode`. This
achieves the issue's "supersedes the node passcode gate" with **zero
migration**: every existing gate call site keeps working unchanged —
`internal/terminal/server.go:599-620` (terminal), `cmd/work.go:1332-1333`
and `:1793-1799` (desktop + terminal wiring), `cmd/controlcenter.go:610-616`
(Control Center), `internal/gateway/gateway.go:470` (gateway HTTP),
`internal/jobs/shell_command.go:301,311` (SHELL_COMMAND) — as do all three
writers (`cmd/passcode.go`, `internal/tui/controlcenter/passcode.go`,
`internal/jobs/config_handler.go`'s `APPLY_DEVICE_CONFIG` → `NodePasscode`)
and the `has_passcode` heartbeat field
(`internal/heartbeat/redis.go:61-69`, `internal/heartbeat/api.go:376`). The
"PIN" is not a new concept sitting beside the passcode; it *is* the
passcode, with the new behavior below layered on top of the same field.

**New behavior needed on top of the existing primitive:**
- **4-digit format enforcement.** Today `SetPasscode` accepts any string;
  v1 of this design should constrain new entries to numeric, 4-digit input
  at the setter boundary (CLI prompt, Control Center form, and the
  `APPLY_DEVICE_CONFIG` handler), without breaking `VerifyPasscode` for any
  existing longer passcode until it's rotated.
- **Rate limiting + lockout after N attempts.** `VerifyPasscode` today has
  no notion of attempt history — it's a pure bcrypt compare. A 4-digit
  space is only safe to gate online at all *because* it's rate-limitable;
  without a persisted counter, "lockout after N attempts" is decorative.
- **Weekly re-prompt cache.** Cache a successful verification and skip
  re-prompting for repeat use, but force re-entry once more than 7 days
  have passed since the last correct entry.

**Both need a cross-process, on-disk home — and the verifier sites already
prove why.** The four independent gate call sites above
(`terminal/server.go`, `gateway.go`, `shell_command.go`,
`controlcenter.go`) each call `config.LoadPermissions(...).VerifyPasscode(...)`
fresh, in their own process or goroutine, specifically so a passcode
rotated at runtime is honored without a restart. An in-memory attempt
counter or cache in any one of them would (a) reset on worker restart and
(b) not apply across the other three surfaces — exactly the failure mode
"lockout after N attempts" is supposed to prevent. This state should live
under `network.GetNodeConfigDir()` (same cross-context reasoning as the
vault, §3) as a small sibling file — e.g. `passcode_state.yaml` — recording
a failed-attempt counter, a lockout-until timestamp, and last-verified-at.

**Resolved: node-wide, not per-identity (see Resolved decisions).** The
terminal path already carries a caller identity (`tokenInfo.UserID`); the
gateway and SHELL_COMMAND paths do not obviously have an equivalent. The
maintainer confirmed a single node-wide counter — simplest, matches "one
PIN, one node," and does not require every gate site to carry a caller
identity. The trade-off (a legitimate user's failed typo counts toward the
same lockout as a genuine attacker's guesses) is accepted.

`has_passcode` in the heartbeat needs no change under this plan — it
already reports "is `PasscodeHash` set," and the PIN *is* `PasscodeHash`.

## 6. No recovery — and a consequence worth calling out

**Passphrase forgotten → the vault's data is permanently unrecoverable.**
This is the zero-knowledge property working as designed, not a bug: if the
node (or anyone) could recover the data without the passphrase, the "node
can't decrypt unattended" claim would be false. Loud, explicit disclosure
at vault setup (first use of an encrypted surface): *"If you forget this
passphrase, this data cannot be recovered — not by you, not by AceTeam."*

**Access PIN forgotten → re-enroll, with no data loss**, because under the
two-secret split (§1) the PIN never encrypts anything. This is a real
consequence of splitting the secret and is *more forgiving* than the
issue's stated behavior ("forgetting the PIN means the encrypted data is
unrecoverable") — flagging explicitly because it changes an acceptance
criterion from the issue, in the safe direction. PIN reset reuses whatever
reset/clear path `SetPasscode` already supports.

**Loud power disclosure, at PIN setup** (this is the issue's disclosure
requirement, and it belongs on the PIN, not the passphrase, since the PIN
is what grants "full use of every resource on the machine"): state plainly
that the PIN grants full console/desktop/files/shell access, that anyone
holding it can do the same, and that scoped/multiple PINs are not available
in v1 ("contact us if wanted").

## 7. Extensibility (structure only — not built in v1)

- **Vault:** the wrapped-DEK layout should reserve room for *multiple*
  wrap entries (each an independent KEK-derivation method wrapping the same
  DEK) rather than a single field, even though v1 populates exactly one.
  That's what lets a future unlock method (a recovery key, a hardware
  token) be added without re-encrypting any existing ciphertext — the new
  method just wraps the already-existing DEK a second way.
- **PIN:** v1 keeps one global `PasscodeHash` per node. The natural
  extension point for scoped/multiple PINs is that several gate call sites
  already thread a caller identity through (`tokenInfo.UserID` in the
  terminal path) — a later version could key a map of passcodes by identity
  or surface without restructuring how `VerifyPasscode` is called at each
  site. Not designed further here, per the issue's explicit "not in v1."

## 8. Consumers

- **#795 (encrypted browser profile)** is the first and only v1 consumer of
  `internal/nodevault`. It calls `Unlock(passphrase)` at profile-mount time
  and uses the returned `Session` for the profile's encrypted contents.
  Nothing about the access PIN changes for it — profile *access* isn't
  gated by the PIN, only the profile's at-rest bytes are gated by the
  passphrase.
- **Terminal, console (Control Center), gateway, SHELL_COMMAND** continue
  exactly as today, on the access-PIN/passcode primitive (§5). None of them
  touch `internal/nodevault`.

## 9. Heartbeat additions

`has_passcode` (§5) is unaffected. The vault needs its own, **distinct**
signal, because "a passphrase has been configured" and "this node can
decrypt right now" are different claims — a truthful E2E indication on the
platform side needs to be able to tell them apart. Proposed additions to
the same `PermissionState`-shaped struct (`internal/heartbeat/redis.go`,
mirrored in `api.go`), following the existing `has_passcode` field's own
reasoning (no `omitempty` — `false` is a meaningful, reportable state, not
an absent one):

- `has_vault_passphrase bool` — a passphrase has been set for this node.
- `vault_unlocked bool` — a session is currently unlocked in this process.
  This is the field that actually backs an "encrypted" vs. "unlocked right
  now" UI distinction; `has_vault_passphrase` alone cannot.

**Resolved (see Resolved decisions):** these field names and the
no-`omitempty` semantics on both are locked as written above.

## 10. Open questions for the maintainer

All questions originally raised here have been reviewed and resolved by the
maintainer — see **Resolved decisions** near the top of this doc for the
locked answers. Kept below for traceability; none are still open.

- ~~Boot-critical secrets (device bearer token, tsnet machine key) excluded
  from v1's vault scope (§2)?~~ **RESOLVED** — excluded from v1 permanently;
  a future phase may revisit with a "prompt on every reboot" trade-off.
- ~~Should `permissions.yaml` (and `PasscodeHash`) move from
  `platform.ConfigDir()` to `network.GetNodeConfigDir()`?~~ **RESOLVED** —
  not migrated in v1; stays under `platform.ConfigDir()`. Only the new
  lockout/attempt-counter state (§5) goes under `GetNodeConfigDir()`.
- ~~Scope of the lockout counter / weekly re-prompt cache (§5): per node or
  per caller identity?~~ **RESOLVED** — single node-wide counter, not
  scoped per identity.
- ~~Exact heartbeat field names/shape for vault presence vs. unlock state
  (§9)?~~ **RESOLVED** — `has_vault_passphrase` and `vault_unlocked`, both
  without `omitempty`.
- ~~AES-256-GCM (stdlib) vs. NaCl secretbox for the DEK-wrap and
  data-encryption primitive?~~ **RESOLVED** — AES-256-GCM (stdlib).
- ~~Argon2id parameter defaults need validation against real node
  hardware?~~ **RESOLVED (defaults kept as proposed)** — `time=3,
  memory=64MiB, threads=4, keyLen=32` ships as the v1 default; validating
  against real hardware is a **pre-ship task for the implementation PR**,
  not an open design question.
- ~~Session/Lock lifecycle: auto-idle timeout, explicit `Lock()`/process
  exit only, or both?~~ **RESOLVED** — v1 is explicit `Lock()` + process
  exit only, no auto-idle timeout.
- ~~Weekly cache semantics: sliding window from last entry, or fixed period
  from first entry?~~ **RESOLVED** — sliding window, resets on every
  correct entry.

No genuinely open design questions remain for this doc. Any new questions
that surface during implementation should be raised on the implementation
PR, not resolved by silently re-interpreting this doc.
