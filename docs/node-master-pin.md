# Node master PIN: access authorization + zero-knowledge at-rest encryption

Design proposal for [citadel-cli#796](https://github.com/aceteam-ai/citadel-cli/issues/796).
Status: **proposal, for review**. No code in this PR.

This document specifies **how** to build the single per-node master PIN decided
in #796: one secret that both (a) authorizes the agent to use the node's
sensitive resources (console, desktop/VNC, files, code, browser sessions) and
(b) is the key that encrypts node-held session data at rest. It supersedes the
existing node passcode gate - the two become one secret, not two.

The requirements in #796 are decided and are not re-litigated here. What this
doc owns is the cryptographic construction, the reconciliation with the code
that exists today, the extensibility seam, the exact truthfulness condition for
an "end-to-end encrypted" indication, and the open questions that are Jason's
call.

It is deliberately adversarial about its own security claim. A 4-digit PIN is
~13 bits of entropy; several sections below exist only to be honest about what
that does and does not buy.

---

## 1. What exists today (the thing this supersedes)

This design is grounded in the current passcode gate, not invented in a vacuum.

**The gate (`internal/config/permissions.go`).**
`Permissions.PasscodeHash` is a **bcrypt** hash (library `DefaultCost`) of the
per-node passcode, stored in `permissions.yaml` (0600) under
`platform.ConfigDir()`. Three functions own it:

- `SetPasscode(pin)` - bcrypt-hashes and stores; empty pin clears.
- `HasPasscode()` - is a hash present.
- `VerifyPasscode(pin)` - fail-CLOSED: no hash or empty pin returns false, so an
  *enabled* sensitive surface with no passcode stays denied rather than opening.

`IsSensitiveCategory` names the gated set: `console`, `desktop`, `files`,
`shell`.

**Enforcement points** (all reload `permissions.yaml` per connection, so a
change takes effect with no worker restart):

- `internal/gateway/gateway.go` (~line 469): `IsSensitiveCategory(category)` →
  `perms.VerifyPasscode(passcodeFromRequest(r))`.
- `internal/terminal/` (server + `passcode_subprotocol`): the console path,
  distinguishing `ReasonPasscodeNotSet` from `ReasonPasscodeInvalid` so clients
  render actionable text.
- `internal/jobs/shell_command.go`: `SHELL_COMMAND` gated on `VerifyPasscode`.

**Three set-paths exist today - the migration must account for all three:**

1. **CLI** - `cmd/passcode.go` (`citadel passcode set` / `clear`), no-echo
   double prompt or piped stdin.
2. **TUI** - `internal/tui/controlcenter/passcode.go` (Control Center).
3. **Platform push** - `APPLY_DEVICE_CONFIG` with `DeviceConfig.NodePasscode
   *string` (`internal/jobs/config_handler.go`), bcrypt-hashed **on the node**
   but sent from AceTeam infrastructure.

**Node-held secrets already on disk** (candidates the KDF could bind to):

- `internal/nodeidentity/` - EC P-256 private key at `ConfigDir()/identity/node.key`
  (0600, never transmitted). Its own header states citadel has **no TPM
  abstraction / no `internal/platform` TPM seam** (see aceteam #4583); the key
  is software-protected by file perms only.
- tsnet WireGuard machine key under `network.GetStateDir()` (`<nodeConfigDir>/network/`).
- `internal/tlscert/` self-signed gateway TLS key.
- `config.yaml` device API token (0600).

**Two facts from the existing code that shape everything below:**

- **bcrypt is a verification hash, not a KDF.** You cannot derive an encryption
  key from `PasscodeHash`. Turning the passcode into *also* an at-rest key is a
  genuinely new primitive, and migrating an existing node **requires re-entering
  the PIN** (see §6). This is a finding, not a detail.
- **The bcrypt cost was deliberately fast** (comment in `permissions.go`:
  per-connection access path). A memory-hard KDF per connection would be a DoS.
  The new design must keep per-connection checks cheap (§5).

---

## 2. Key hierarchy

**Recommendation: envelope encryption (LUKS-style keyslot).**

```
PIN ──Argon2id(salt, pepper)──▶ KEK ──AEAD-unwrap──▶ DEK (random 256-bit)
                                                        │
                                        HKDF(DEK, "context-label")
                                                        │
                                    ┌───────────────────┼───────────────────┐
                                 browser-profile      console-session      files-cache
                                 subkey (#795)         subkey               subkey
```

- A strong **random 256-bit DEK** is generated once at PIN setup. It - never the
  PIN - encrypts data at rest, via per-consumer subkeys derived with HKDF and a
  context label (so #795 holds a browser-scoped subkey, never the master DEK).
- The PIN derives a **KEK** (Argon2id, §2.2), which wraps the DEK in a keyslot
  using an AEAD (AES-256-GCM or XChaCha20-Poly1305). "Verify the PIN" =
  "the AEAD unwrap authenticates." There is **no separate verification hash**.
- Rotating the PIN re-wraps the *same* DEK under a new KEK - existing ciphertext
  is untouched, no bulk re-encryption. This is the property PIN-derives-key-
  directly cannot offer.

### 2.1 Why not PIN-derives-key-directly

If the PIN (via Argon2id) *is* the data key:

- rotating the PIN forces re-encrypting everything;
- multiple/scoped PINs later (§7) are impossible without a rewrite;
- worst: every distinct ciphertext is a fresh oracle for the same 13-bit guess.

Envelope keeps exactly one small keyslot as the brute-force target and cleanly
separates "authorize" from "encrypt." **Recommended: envelope.**

### 2.2 The node-secret binding - a dedicated pepper, NOT the identity key

A 4-digit PIN cannot be sole key material. We combine it with a node-held
secret ("pepper") mixed into the Argon2id input, so ciphertext alone (without
the node's disk) is not attackable by PIN-guessing.

**Recommendation: a dedicated random 32-byte pepper file**, e.g.
`identity/pin-pepper` (0600), created at PIN setup, following the
`nodeidentity` file pattern as precedent.

**Do NOT bind to `node.key`.** It is tempting ("it's already there"), but:

- #4583 is explicitly building *self-healing identity* (re-enrollment, eventual
  key rotation). If that key ever rotates, **all PIN-encrypted data silently
  becomes undecryptable** - a latent data-loss landmine.
- It is circular if the PIN store ever needs to encrypt identity material.
- It buys nothing over a dedicated secret: both sit on the same disk.

A dedicated pepper has the same threat model, a clean lifecycle, and is the
natural place to add TPM sealing later (§2.3).

### 2.3 Hardware sealing (TPM / Secure Enclave): assumed ABSENT in v1

Per `nodeidentity`'s own comment, citadel has no TPM seam today. So v1 assumes
**software-only** protection: the pepper is a 0600 file. The honest consequence:

> An attacker who obtains the node's disk obtains **both** the ciphertext **and**
> the pepper. Security then reduces to Argon2id-hardened brute force of the
> ~13-bit PIN. See §10 (adversarial).

The pepper is designed so that, when a `platform` TPM abstraction lands (#4583's
follow-up), **sealing the pepper to the TPM** is the drop-in hardening: the PIN
+ a TPM-resident secret would then be required, and disk theft alone would no
longer expose the pepper. **Whether TPM sealing is a hard v1 dependency is an
open question for Jason (§10).**

---

## 3. KDF choice and parameters

**Recommendation: Argon2id** (`golang.org/x/crypto/argon2`, already a
dependency - `x/crypto v0.52.0`). Memory-hardness is what matters against a
low-entropy secret: it denies the GPU/ASIC parallelism that would otherwise
make 10⁴ guesses trivial.

Starting parameters, **calibrated at setup** to a wall-clock target rather than
frozen constants:

| Param | Starting value | Notes |
|---|---|---|
| target time | **~1 s** on the setup machine | calibrate `time`/`memory` up to hit it |
| memory | 256–512 MiB | the real cost lever; bound so a small node still unlocks |
| iterations (time) | ≥3 | raised if the memory bound is hit before the time target |
| parallelism | = CPU cores (cap ~4) | |
| salt | 16 B random, **per keyslot** | stored in the slot header |
| output | 32 B KEK | |

**Store the parameters in the keyslot header** (LUKS-style), never in code, so
cost can be raised later without breaking old slots and so a slot migrated from
a weaker machine still records what produced it. `TestKDFParamDefaults`-style
pinning belongs on the *floor* values, not the calibrated ones.

Honest bound: at ~1 s/guess, the full 4-digit space is ~2.7 hours single-
threaded. Argon2id makes each guess cost RAM+time; it **cannot** make 13 bits
be more than 13 bits. Longer/alphanumeric PINs are the only real fix (§10).

scrypt is an acceptable alternative (same memory-hard family) but Argon2id is
the current best-practice default and is already vendored.

---

## 4. Lockout / rate-limit policy

Rate-limiting protects the **online** path only (someone typing guesses at a
live node). It does nothing against offline disk brute force - that is bounded
solely by Argon2id cost (§3). Say this plainly; do not let lockout imply
at-rest safety.

**Recommendation:**

- Count consecutive failures in a **persisted** counter (see §4.1). Per-guess
  Argon2id cost (~1 s) is itself the first rate limit.
- Exponential backoff after a small threshold: e.g. free up to 5 attempts, then
  delay `min(2^(n−5) sec, 5 min)` before the next attempt is even evaluated.
- **Hard lockout** after N total failures (recommend N≈20): refuse further
  attempts until **local presence** is demonstrated (a CLI unlock on the box,
  not a remote/mesh attempt). This defeats remote online guessing without
  destroying data.
- **No auto-wipe by default.** Wiping the keyslot on lockout means a mistyped-
  PIN storm (or a cat on the keyboard) permanently destroys unrecoverable data.
  Auto-wipe is offered as an **opt-in** posture only (§10 - Jason's call on the
  threshold and whether to offer it at all).

### 4.1 Reconciling lockout with "no recovery"

"No recovery" (forgetting the PIN loses the data) and "lockout" are different
axes: recovery is about the *legitimate holder forgetting*; lockout is about an
*attacker guessing*. Hard lockout requiring local presence satisfies both - the
real operator can always get back in at the console with the correct PIN, an
attacker on the mesh cannot grind. Only **opt-in auto-wipe** couples them, and
that coupling is exactly why it is off by default.

**The counter must persist across restart** (§4.1 state placement in §5.3),
or a crash/restart resets the online rate limit and hands an attacker unlimited
fresh attempts.

---

## 5. Unlock lifetime in memory + weekly re-entry

### 5.1 Cached unlocked state (per-connection checks stay cheap)

Running Argon2id per connection would DoS the node (the old bcrypt cost was
deliberately fast for exactly this path). Instead:

- On correct entry, the KDF runs **once**; the unwrapped DEK (and derived
  subkeys) live in the **worker process RAM only**.
- Per-connection gating (console/desktop/files/shell) checks the **cached
  unlocked state** - "is this node currently unlocked?" - not the KDF.
- The enable/disable bits keep their existing **fail-closed per-connection
  reload** of `permissions.yaml`. Enablement without an unlocked master secret
  denies, exactly as `VerifyPasscode` denies today.

### 5.2 Weekly rolling re-entry

- Persist a **`last_entry_at`** timestamp on successful unlock.
- On unlock/first-use, if `now − last_entry_at > 7 days`, the cached state is
  treated as expired: re-prompt, re-run the KDF, refresh `last_entry_at`.
- Re-entry gates **decrypt-dependent access** (anything needing the DEK) and
  re-authorization of the sensitive surfaces. It shrinks the unlocked-window
  attack surface and aids recall (a PIN entered weekly is remembered).

### 5.3 State placement (repo rule, not a choice)

`last_entry_at`, the lockout counter, the keyslot header, and the pepper are
**cross-context state**: written by a systemd-root `citadel work`, read by an
interactive non-root `citadel status`/`unlock`. Per this repo's own hard-won
rule (#383, #726, documented in CLAUDE.md), that means
**`network.GetNodeConfigDir()`**, NOT `platform.ConfigDir()` - the latter
silently resolves to *different* directories for those two callers and the
reader would see nothing forever.

> Migration note: `permissions.yaml` (and its `PasscodeHash`) lives under
> `platform.ConfigDir()` today. The new keyslot/pepper/counters move to
> `GetNodeConfigDir()`. The enable/disable bits may stay where they are; the
> **secret** material must move. Call out the path change in the migration PR.

### 5.4 Zeroization and the exposure window - honest limits

- Best-effort zeroization: overwrite key buffers on lock/expiry/shutdown.
- **Go zeroization is best-effort only.** The GC may copy a `[]byte` before you
  wipe the original, and there is no portable `mlock` to keep keys out of swap.
  Do not oversell this. Mitigations: keep the plaintext key material minimal and
  short-lived, prefer OS-page-locked buffers where a platform allows, and rely
  on the reboot-relock (below) as the real backstop.
- **Exposure window:** while unlocked, the DEK is in live RAM and the node *can*
  decrypt - the node is only zero-knowledge **while locked**. A root-level
  compromise of a live, unlocked node can read the key. This is inherent to
  "the agent must actually use the resource"; the weekly expiry and an explicit
  `citadel lock` (§8) bound it.

---

## 6. Migration from the existing passcode gate

**The hard finding first:** bcrypt (`PasscodeHash`) is one-way and cannot be
turned into a KEK. **There is no silent, zero-touch migration.** A node's
existing passcode cannot become the master PIN's encryption key without the
operator re-entering it once so the KDF can run and a keyslot can be created.
This is a genuine limitation of what exists, not a design shortcut.

**Recommended migration:**

1. **First correct entry after upgrade = enrollment.** The operator enters the
   PIN via the same no-echo prompt. On success, citadel:
   - creates the pepper, generates the DEK, runs Argon2id, writes the keyslot;
   - **deletes `PasscodeHash`** from `permissions.yaml` (see below).
2. Until enrollment, the node keeps gating on the legacy `VerifyPasscode` so
   access is not broken mid-upgrade (backward compat for the *gate*; there is
   simply no at-rest encryption yet on that node).
3. All **three** set-paths converge on the new primitive:
   - CLI `citadel passcode set` becomes (or is aliased by) `citadel pin set`;
   - the TUI Control Center path calls the same API;
   - the **platform-push path is the open question of §10** - a PIN pushed from
     AceTeam has transited AceTeam, so it can never back a truthful E2E badge
     (§8). Recommendation: for the master PIN, the platform push should *prompt
     for local entry* rather than carry the secret, or be disabled outright.

**Delete the bcrypt hash after enrollment.** If `PasscodeHash` survives next to
the Argon2id keyslot, it is the **cheaper** brute-force target (bcrypt
`DefaultCost`, no memory-hardness) and quietly undoes the whole KDF story. One
secret means one keyslot and no legacy hash.

**What breaks (state it in the PR):**

- A node that never re-enters its PIN post-upgrade gets no at-rest encryption
  and shows no E2E badge - correct, but a behavior change to disclose.
- **Reboot relocks everything.** The DEK cache is RAM-only, so every crash or
  reboot leaves encrypted-dependent surfaces (browser profiles #795, etc.)
  **dark** until a human re-enters the PIN. A 3 a.m. systemd auto-restart
  brings the worker back but not the decrypt key. "No unattended decrypt" is
  fundamentally incompatible with unattended recovery - this is the point of the
  feature, and it must be stated, not hidden. The existing **piped-stdin**
  pattern (`echo -n PIN | citadel unlock`) is the operator's explicit opt-out
  for headless auto-start, with its tradeoff named: a PIN readable by whatever
  can write that pipe is only as private as that pipe.

---

## 7. Extensibility hook for scoped PINs (build the seam, not the feature)

The envelope + **keyslot table** IS the extensibility mechanism. v1 ships a
one-slot table; scoped/multiple PINs later add slots - no rewrite, no data
re-encryption (every slot wraps the same DEK, or a scope-restricted subkey).

Proposed internal shape (illustrative - not built in v1):

```go
// Keyslot wraps the node DEK under one PIN-derived KEK. v1 writes exactly one.
type Keyslot struct {
    ID       string      // "master" in v1
    KDF      KDFParams   // algo + calibrated params + salt (per-slot)
    Wrapped  []byte      // AEAD-wrapped DEK (or scope subkey)
    Scope    Scope       // v1: ScopeAll. Later: {Console, Files, ...}
}

// Unlocker is what console/terminal/files/#795 call. v1 has one impl.
type Unlocker interface {
    Unlock(ctx context.Context, pin string) (Session, error) // runs KDF, opens a slot
    IsUnlocked() bool                                          // cheap per-conn check
    Lock()                                                     // zeroize + relock
}

// Session hands out context-scoped subkeys; consumers never see the master DEK.
type Session interface {
    Subkey(label string) ([]byte, error) // HKDF(DEK, label)
    Authorizes(category string) bool      // v1: always true for a valid session
}
```

`Scope` defaulting to `ScopeAll` and `Authorizes` returning true unconditionally
is what keeps v1 a single all-powerful PIN while leaving the exact seam where
scoped enforcement later slots in. **v1 exposes none of this** - the UX is one
PIN, and setup loudly says scoped PINs are "contact us" (#796).

---

## 8. The "end-to-end encrypted" truthfulness gate

A surface (CLI, web console, mobile) may show an "end-to-end encrypted" /
zero-knowledge indication **only when ALL of the following hold**. If any fails,
the badge must not appear.

1. **A master PIN keyslot exists** and backs the node's at-rest encryption
   (enrollment completed; the legacy bcrypt-only state does not qualify).
2. **The node is currently locked** *or* the claim is scoped to "at rest":
   the truthful statement is *"ciphertext at rest when locked; the unwrap key
   exists only in this node's live process RAM while unlocked; locked again on
   reboot or weekly expiry."* A live-unlocked node is not "the provider cannot
   read this right now" - do not imply that.
3. **PIN provenance is local.** The PIN was set by **local/interactive entry**
   (CLI/TUI/console-on-box) and **never transited AceTeam infrastructure**. A
   PIN delivered via `APPLY_DEVICE_CONFIG` platform push fails this - that node
   must not show the badge (and see §10 on whether that path is killed).
4. **No plaintext PIN or DEK escrow leaves the node** (no backup of the pepper
   or unwrapped DEK to the platform).
5. *(If a minimum-entropy policy is adopted, §10)* the PIN meets it - a 4-digit
   PIN arguably should not earn an unqualified "E2E" claim (§10).

Implement this as a single predicate (e.g. `pin.E2EEligible() (bool, reason)`)
so every surface asks the same authority and a future condition is added in one
place - mirroring how `IsSensitiveCategory` centralizes "what is sensitive."

**Web-console caveat (state it honestly):** even with node-terminated TLS over
the relay, a PIN typed into a browser console is only as trustworthy as the JS
the platform serves to that browser. Node-side E2E does not make browser-entry
E2E; that is the standard web-crypto limitation and should be disclosed where
the badge is shown for browser-originated entry.

---

## 9. Consumer API (specified, not implemented)

The one primitive #795 (browser profile), the terminal, the console, and future
surfaces call. Rough Go shape; names illustrative:

```go
package pin // internal/pin (proposed)

// State / lifecycle
func Enrolled() bool                 // a master keyslot exists on this node
func IsUnlocked() bool               // cheap; per-connection gates use this
func LastEntryAt() (time.Time, bool)
func ExpiredForWeekly() bool         // now - LastEntryAt > 7d

// Setup / rotation (CLI, TUI, migration all call these)
func Enroll(ctx, pin string) error   // first-time: pepper+DEK+keyslot; calibrates KDF
func Rotate(ctx, old, new string) error // re-wrap same DEK; no bulk re-encrypt
func Disenroll(ctx, pin string) error   // destroy keyslot (explicit, warned)

// Unlock / lock
func Unlock(ctx, pin string) (Session, error) // runs Argon2id; opens slot; may lock out
func Lock()                                    // zeroize + relock (explicit `citadel lock`)

// Per-consumer key material (consumers NEVER get the master DEK)
type Session interface {
    Subkey(label string) ([]byte, error) // e.g. "browser-profile", "console"
    Authorizes(category string) bool      // console/desktop/files/shell
}

// Truthfulness
func E2EEligible() (bool, reason string)  // the §8 predicate, one authority

// Errors carry the §1 reason distinction (client renders actionable text)
var (
    ErrNotEnrolled  = errors.New("no master PIN set")
    ErrInvalidPIN   = errors.New("incorrect PIN")
    ErrLockedOut    = errors.New("too many attempts; local unlock required")
    ErrExpired      = errors.New("weekly re-entry required")
)
```

Notes for consumers:

- **#795 asks for `Subkey("browser-profile")`**, encrypts its profile with that,
  and never touches the master DEK. Each consumer gets an independent HKDF
  subkey, so a leak of one does not expose others or the DEK.
- The **gateway/terminal/shell** enforcement points swap `VerifyPasscode(pin)`
  for `IsUnlocked()` + `Session.Authorizes(category)`; they keep the
  per-connection fail-closed reload of the enable bits.
- The `Reason`/error split preserves the existing `ReasonPasscodeNotSet` vs
  `ReasonPasscodeInvalid` client behavior (§1).

---

## 10. Open questions for Jason

These are genuinely product/risk calls, not implementation details:

1. **Auto-wipe on lockout - offer it at all, and at what threshold?**
   Recommendation: **off by default**; hard lockout requiring local presence
   instead (a mistyped-PIN storm must not permanently destroy unrecoverable
   data). If offered, it is strictly opt-in. Your call on the threshold and
   whether to ship it in v1.

2. **Is TPM/Secure-Enclave sealing a hard v1 dependency?** Today there is no TPM
   seam (#4583). Recommendation: ship software-only (0600 pepper) and be honest
   in the badge, with TPM sealing as the designed hardening hook. Confirm you
   accept the "disk theft ⇒ 13-bit offline brute force" exposure for v1.

3. **Kill the platform-push PIN path for the master PIN?** A PIN pushed via
   `APPLY_DEVICE_CONFIG` has transited AceTeam and can never back a truthful E2E
   badge. Recommendation: convert it to "prompt for local entry" or disable it
   for the master PIN. Your call.

4. **Minimum PIN length / allow alphanumeric passphrases - and does the E2E
   badge require a minimum entropy?** A 4-digit PIN is ~13 bits; that
   fundamentally caps the security claim. Recommendation: allow (and gently
   encourage) longer/alphanumeric secrets; consider gating the *unqualified*
   E2E badge on a minimum entropy, showing a qualified indicator below it.

5. **Unattended-restart operational stance.** "No unattended decrypt" means a
   reboot leaves encrypted surfaces dark until a human re-enters the PIN. Accept
   that (and document the piped-stdin opt-out with its tradeoff), or carve out a
   narrower posture for specific always-on nodes?

6. **Accept-data-loss setup messaging.** #796 mandates loud disclosure of
   (a) data loss on forget, (b) full-machine power, (c) "contact us for scoped
   PINs." Confirm the exact wording and whether setup should require a typed
   acknowledgement ("I understand this cannot be recovered") before enrolling.

---

## 11. The honest limitation, in one paragraph

A 4-digit PIN is ~13 bits of entropy. Argon2id + a node-held pepper make each
guess cost real memory and ~1 second, and rate-limiting/lockout defeat *online*
guessing - but none of that changes that there are only ten thousand possible
PINs. An attacker who steals the node's disk gets the ciphertext **and** the
pepper (no TPM in v1), so the at-rest guarantee degrades to Argon2id-bounded
brute force of those ten thousand values: roughly hours, not centuries. The
zero-knowledge / E2E claim is therefore **truthful only in the specific senses
enumerated in §8** - most strongly against an adversary who obtains ciphertext
**without** the node's disk (a backup, a cloud sync, a memory-only exfil) or
once the pepper is TPM-sealed, and **only while the node is locked**. It should
be presented that way, not as unconditional secrecy. The real fix for the
entropy ceiling is a longer or alphanumeric secret (§10.4); everything else is
damage control around thirteen bits.
```
