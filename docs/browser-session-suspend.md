# Browser session suspend and reopen (#1821)

## Draft checkpoint, not an enabled capability

This branch starts the node-side implementation with pure planning and bounded
manifest primitives. There is no production caller yet: no new job action,
browser close/relaunch, profile snapshot, sweep timer or advertised capability.
Existing browser behavior is unchanged. This is not a completed suspend feature
or native acceptance proof.

## Contract being built

Suspend applies only to running or attached sessions. A pending unlock must be
unlocked or stopped; launching, resuming and suspending sessions are busy. A
human-driven session refuses agent-initiated suspend. The human owner may
explicitly suspend it, but a payload must not be allowed to invent that authority.

The current node job envelope has no authenticated user identity, and the
registry's existing owner is an OS process owner, not an account owner. Before
job wiring, the paired platform must supply and authorize a stable account
binding at start and on suspend/reopen. Unknown-owner legacy sessions will
refuse suspend, not acquire a new owner from the suspend request. The wire field
names and first-party-human assertion are pending coordinator confirmation.

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
- Throwaway TTL: 24 hours. Persistent TTL: seven days; only the manifest expires,
  never the saved profile. Expired tombstones remain seven days from the original
  expiry, not from the last status read or sweep retry.

Reopen retains the session ID, increments generation without wraparound, keeps
the driver, and consumes a live slot. A persistent session requires a successful
human unlock and the same profile binding, with no throwaway fallback. A
throwaway reopens its tabs signed out. Unsaved forms, in-page JavaScript state,
history stacks and browser process memory are not preserved.

## Remaining implementation before review/activation

1. Confirm and wire the authenticated owner and human-origin assertions through
   the paired job producer and handler. Preserve the platform's ownership,
   entitlement and approval-expiry checks; the node does not grant them.
2. Capture over bounded CDP operations under the lifecycle/action locks. Write
   the complete private or encrypted manifest durably before graceful close and
   existing teardown. Refuse on storage/capture uncertainty without reporting
   resources freed. Cover stop, crash and shutdown races.
3. Relaunch under the existing ID with generation fencing, restore tabs through
   live URL policy, and restore scroll best effort. Do not consume or replay a
   stored unlock credential. Wrong unlock/profile-busy leaves suspension intact.
4. Reuse registry recovery, with suspended/resuming/interrupted-suspend handling,
   bounded deletion, tombstones, hourly sweep and error reporting. Do not let the
   old live-record retention path prune suspend tombstones after one day.
5. Add job results/status/capability wiring only when all effects and refusal
   tests are complete. Local mode does not expose suspend in this version.

Unit proofs use fake clocks and owned temporary registry files. They do not
claim that browser/display processes were freed, that a profile was sealed, that
the manager recovered a suspended session, or that tabs reopened on a real node.
