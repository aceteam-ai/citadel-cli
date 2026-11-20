# Browser session suspend and reopen (#1821)

## Draft checkpoint, not an enabled capability

This branch starts the node-side implementation with planning, authenticated
start-owner metadata, bounded manifest encryption, private file storage and
per-page CDP capture primitives. Suspend and
reopen have no production caller yet: no new job action,
browser close/relaunch, profile snapshot, sweep timer or advertised capability.
Existing live-browser launch/action behavior is unchanged. This is not a completed suspend feature
or native acceptance proof.

The existing node-wide status and list methods now project validated dormant
registry metadata when no in-memory session takes precedence. They report the
state, live generation, expiry, retained tab count and profile kind, never tab
content, profile paths or a viewer endpoint. Local mode does not read the node
registry. The fallback creates no directory, opens no payload/profile, probes
no process/browser and writes no lifecycle transition. Reads have directory,
record and total-byte bounds. An elapsed suspended capture reports expired
availability without claiming physical deletion; durable expiry and pruning
still belong to the unfinished housekeeping integration. Old expiry metadata
is hidden after its seven-day retention window. This is status wiring, not an
enabled suspend/reopen action or real node-restart acceptance.

The managed worker also owns metadata-only housekeeping under its root context.
It runs a bounded pass on startup and hourly, with one-second follow-ups only
after a successful full batch. Errors back off to the hourly cadence. Each pass
has a five-second cooperative budget, retains separate progress cursors for TTL
and orphan work, and takes the existing store/process and registry exclusions.
An unused registry/payload directory is never created. Local mode does not start
the loop. Cancellation stops waiting and future passes. Cleanup does not stop or
adopt a browser, recover an in-flight transition, unlock a profile, or remove a
saved profile. Those integrations remain unfinished.

## Contract being built

Suspend applies only to running or attached sessions. A pending unlock must be
unlocked or stopped; launching, resuming and suspending sessions are busy. A
human-driven session refuses agent-initiated suspend. The human owner may
explicitly suspend it, but a payload must not be allowed to invent that authority.

The approved paired contract is `owner_user_id`, injected by the platform from
the authenticated session at start and on suspend/reopen. The server must never
forward an agent/client-selected value. `owner_initiated=true` is reserved for a
first-party human action, not a tool argument. The start handler now records the
optional owner before launch and preserves it through registry writes and host
or container re-adoption. The registry's OS process owner is not an account
owner and never substitutes for it. Legacy starts with no assertion keep their
existing behavior but remain unbound: suspend and reopen must fail closed, not
acquire a new owner from a later request. This source change does not implement
the paired server producer or grant ownership/entitlement on the platform.

The planner requires a current, owner-bound live record. Its suspended snapshot
retains session ID, generation, driver, tracking and profile kind/name, but no
PID, scope, display, debug/control port, plaintext work-directory path, container
identity or pending restart intent. This snapshot extends the existing registry
record rather than establishing a second lifecycle store.

The tab manifest contains only current page URLs, titles, scroll coordinates
and one active tab, bound to session ID and capture generation. Full URLs may
carry secrets. They must stay node-local and must not appear in status, job
results, logs or audit events. Errors and diagnostic summaries do not include
tab data. Persistent manifests must be encrypted with the already unlocked
profile vault session under a distinct session-bound context before storing.
The profile handle now has bounded manifest seal/unseal primitives using the
existing vault session, with context bound to profile, session and capture
generation. They expose no key, retain no passphrase and stop working after
the handle closes. Authorization and expiry remain manager responsibilities,
not claims of the crypto helper. The store requires the matching profile handle
before preparing or reading persistent payloads.
Throwaway manifests may be stored in a private 0600 file and contain no cookies,
storage, passwords, form values or browser profile archive.

Bounds for the initial node implementation:

- At most 64 tabs; 8192 UTF-8 bytes per URL and 2048 per title.
- At most 256 KiB of manifest JSON. The decoder checks bytes before allocation,
  refuses unknown fields and trailing documents, and validates URL schemes and
  finite bounded scroll coordinates. Reopen must also apply the live destination
  policy before navigating; syntactic validation alone is not network authority.
- At most five retained suspended/in-flight sessions per account on this node.
- At most 8 MiB of retained manifest storage node-wide, including other owners
  and encryption overhead. Unknown accounting refuses allocation. Expired but
  not yet physically removed storage remains charged.
- The private store anchors access with `os.Root`, takes a stable cross-process
  OS lock and rechecks privacy before reads/writes. Directory scans stream in
  batches of 64 and refuse allocation at 1024 entries including the lock.
  Unreferenced and temporary files remain charged. Unknown file types and
  oversized entries refuse allocation. The manager still owns per-account
  record accounting and registry transitions around this payload store.
- File data is synced before create-only atomic hard-link publication. Existing
  generations and symlinks are not overwritten. Filesystems without hard links
  refuse without authorizing teardown. Directory-sync errors remain errors;
  uncertain published files stay charged. Removal targets one validated payload
  and syncs absence before a later tombstone commit. Windows uses file flush
  plus atomic publication for agent-process restart recovery; no full-machine
  power-loss guarantee is claimed there.
- Throwaway TTL: 24 hours. Persistent TTL: seven days; only the manifest expires,
  never the saved profile. Expired tombstones remain seven days from the original
  expiry, not from the last status read or sweep retry.

Capture generation remains immutable in suspension metadata. A resuming record
advances its live generation but still reads/decrypts the previous capture, not
a nonexistent next-generation file.

Per-page capture reads only URL, title and scroll in an isolated main-frame
world. Its connection requires a finite deadline and a 1 MiB CDP message bound;
every await rechecks a caller-provided lifecycle/identity fence. The manager
still must enumerate/verify all pages, authorize owner/driver and hold its
action/lifecycle locks before invoking these currently unwired primitives.
The scoped capture continuation retains action exclusion until intent staging
returns, rather than leaving an unlock/relock gap after capture. Its trusted
caller still owns registry exclusion and must revalidate around its own awaits;
it must not re-enter ordinary actions or teardown. Cancellation after a durable
effect is an unacknowledged result, not permission to delete referenced data.
The manager transaction is not wired to this continuation yet.
The private throwaway capture-to-intent composition additionally holds registry
exclusion, pins the pre-capture runtime pointer and record, and checks a trusted
manager-incarnation fence during publication. Its continuation starts only after
durable intent staging. Failures retain any referenced intent and capture; a
successful fake stop continuation is a component test, not a runtime proof.
Saved-profile handling and the production manager transaction remain unwired.

Reopen retains the session ID, increments generation without wraparound, keeps
the driver, and consumes a live slot. A persistent session requires a successful
human unlock and the same profile binding, with no throwaway fallback. A
throwaway reopens its tabs signed out. Unsaved forms, in-page JavaScript state,
history stacks and browser process memory are not preserved.

## Remaining implementation before review/activation

1. Finish the approved owner and human-origin assertions through the paired job
   producer and suspend/reopen handler. Preserve the platform's ownership,
   entitlement and approval-expiry checks; the node does not grant them.
2. Integrate all-page capture under the lifecycle/action locks. Commit the
   complete private or encrypted manifest through the store before graceful close and
   existing teardown. Refuse on storage/capture uncertainty without reporting
   resources freed. Cover stop, crash and shutdown races.
3. Relaunch under the existing ID with generation fencing, restore tabs through
   live URL policy, and restore scroll best effort. Do not consume or replay a
   stored unlock credential. Wrong unlock/profile-busy leaves suspension intact.
4. Finish registry recovery with suspended/resuming/interrupted-suspend handling;
   metadata-only bounded expiry/orphan scheduling is wired, but process-aware
   recovery remains incomplete. Do not let the
   old live-record retention path prune suspend tombstones after one day.
5. Add job results/status/capability wiring only when all effects and refusal
   tests are complete. Local mode does not expose suspend in this version.

Unit proofs use fake clocks and owned temporary registry files. They do not
claim that browser/display processes were freed, that a profile was sealed, that
the manager recovered a suspended session, or that tabs reopened on a real node.
