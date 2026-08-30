# Design: on-node display of the `node:exec` pairing code (citadel #659)

Status: design only, no implementation. Companion to aceteam-ai/aceteam#6975
(CLOSED — backend grant flow already shipped), which explicitly stubs out the
on-node rendering this issue owns.

> **Part I (§1–§7)** is the current-state survey and option analysis.
> **Part II (§8–§14)** is the implementable design: job contract, rendering
> mechanisms, security invariant, capability field, TTL lifecycle, and the
> phased file-by-file plan. Where the two disagree (notably phasing — §8.6
> supersedes §6, per owner direction that P0 is the minimal *headed-node*
> path that makes `_node_screen_delivery` return True), Part II wins.

## 1. Current state

### 1.1 The node:exec grant flow already exists — entirely on the backend

`citadel-cli` has **zero** existing code for a node:exec grant/pairing flow.
`grep -rn "node_exec\|NodeExec"` across `cmd/` and `internal/` matches nothing
relevant (only an unrelated `workflow.NodeExecutor` type for flow-graph nodes).
This is greenfield on the citadel side.

The flow is fully built and shipped in `aceteam` (issue #6975, closed):
`python-backend/routes/aceteam_mcp_node_exec_grant.py` implements three MCP
tools —

- `node_exec_request_grant(node_id, duration)` — mints an 8-digit numeric code
  (`_generate_code`, line 137; `_CODE_DIGITS = 8`, line 73), stores only its
  SHA-256 hash in Redis keyed `node_exec_grant:challenge:{org}:{node_id}`
  (line 122), with a **10-minute challenge TTL** (`_CHALLENGE_TTL_SECONDS =
  600`, line 68) distinct from the grant's own lifetime (24h / 30d default /
  indefinite). Returns only "code delivered" — never the code itself.
- `node_exec_confirm_grant(node_id, code)` — org-admin-only, verifies the code
  (max 5 attempts, line 76), records a durable grant.
- `node_exec_revoke_grant(node_id, user_id)` — admin-only, audited.

Delivery is a fallback chain in `_deliver_pairing_code` (~line 357): **node
screen first, then `notify`'s `HUMAN_ONLY_CHANNELS`** (push → Telegram → SMS;
WhatsApp/WeChat/email are deliberately excluded because their own
`*_read_messages` tools would hand the code straight back to an agent —
aceteam#7262). The node-screen step is:

```python
async def _node_screen_delivery(*, node_id: str, code: str) -> bool:
    """... this is a structured no-op today: it always reports "not delivered"
    and the caller falls through to the linked-device channels."""
    return False
```

(`aceteam_mcp_node_exec_grant.py:186-194`). This is the exact integration
point #659 exists to fill. Two things follow directly from reading it:

1. **There is no node capability signal on the backend to consult yet.**
   `python-backend/utils/database/fabric_node_status.py` has no
   `capabilities`/`desktop`/`display` field at all. Wiring citadel's
   capability report through to this function is new work in *both* repos,
   not just a citadel-side job handler.
2. **The dispatch mechanism already has a proven shape to copy.**
   `dispatch_node_update` (`aceteam_mcp_fabric.py:398`) pushes an
   `AGENT_UPDATE` job to the target node's per-node Redis stream and awaits a
   bounded result. A new job type for showing/clearing the code should follow
   the same pattern rather than inventing a new transport.

### 1.2 Two same-named-but-unrelated things to not confuse this with

- **`internal/nodeidentity/pairing.go` and `internal/devicemode/pairing.go`**
  are device/node **enrollment** pairing (issue #5959, #4583): a node/laptop
  generates its own code, POSTs it to `/api/fabric/pairing/start`, a human
  approves in a signed-in browser at `aceteam.ai/fabric/pair?code=...`, and
  the platform returns an authkey + fabric CA leaf. This is the *trust-root*
  event that gets a node onto the mesh in the first place — it has nothing to
  do with granting `node:exec` on an already-trusted node. Confirmed by
  reading both files' headers; the prior reviewer's note holds.
- **`display_show` / `display_list` / `display_clear`** (aceteam
  `routes/aceteam_mcp_displays.py`, epic #5582/#6332) are **org display
  casting** — pushing aceteam-hosted content (a published Page, a hosted
  widget, the Agent Ops board) to a *registered shared screen* (e.g. an office
  TV), via a Next.js-owned `displays` registry and its own Redis command
  channel. The module's own docstring states the hard rule: "only
  aceteam-hosted content can ever be cast... an agent pushing arbitrary web
  content to a shared office screen is a phishing/abuse surface." This is a
  **different registry, different trust model, and explicitly disallows
  exactly the kind of one-off, security-sensitive, non-hosted string #659
  needs to show.** Naming a new mechanism anything like "display_*" invites
  confusion with this unrelated feature — recommend distinct terminology
  (e.g. "pairing display", not "display cast").

### 1.3 What display surfaces citadel actually has today

| Surface | What it is | Usable for an async, platform-pushed pairing code? |
|---|---|---|
| `internal/ui/devicecode.go` (`DeviceCodeModel`) | A bubbletea TUI shown **synchronously, in the foreground**, only during an interactive `citadel init`/`citadel login` invocation. | No — it only exists for the duration of that one CLI call. A steady-state `citadel work` service has no such surface running. |
| Control Center TUI (`cmd/controlcenter.go:176 runControlCenter`) | Full bubbletea TUI. Requires a real TTY (`tui.IsTTY()` check, refuses otherwise) and is **single-instance** (`instance.IsRunning(configDir)` — a second launch just prints "already running" and exits). | Only if a human already happens to have it open in a terminal right now. Per this repo's own CLAUDE.md, the TUI collector is already missing several heartbeat fields (`WorkerLiveness`, `PinnedServices`, `SwapActivity`) as "pre-existing gaps" — it is not the primary, always-on surface. |
| Plain process stdout of `citadel work` | The systemd unit sets `StandardOutput=journal+console` (`internal/service/systemd.go:73,127`). | **Reaches the physical console, but also journald** — and journald is read by `citadel logs` (`cmd/logs.go`) and `journalctl`, both reachable by an agent with `SHELL_COMMAND`/`terminal_exec`. Writing the code to stdout would violate the issue's "never agent-readable" requirement even on a node with a real screen attached. This mirrors the exact class of bug already documented in this repo's CLAUDE.md for `citadel mcp`'s `captureStdout` — a shared stdout stream is unsafe to write security-sensitive content to without explicit isolation. |
| Xvfb (`internal/platform/meeting_browser.go`) | A **virtual, headless** framebuffer the meeting/cobrowse browser owns for itself, started explicitly *because* "meeting nodes are typically headless" (line 526-528). | No — there is no physical monitor behind it; showing a code there proves nothing about physical presence and no human is watching it. |
| KVM tools (`kvm_screenshot`/`kvm_type`/`kvm_power`, referenced only in `internal/tui/controlcenter/proxmox_page.go`) | Backend-owned, out-of-band **BMC/Proxmox-hypervisor** access to a physical host's KVM console — a layer *below* the guest OS. | Not citadel's to render into. Citadel usually runs as the guest; it has no code path into the hypervisor's KVM framebuffer. Relevant only for bare-metal Proxmox hosts citadel itself manages, and even then it's a different actor's channel. |
| `internal/desktop/capabilities.go` (`DetectCapabilities`) + `internal/session` (`DetectDesktop`) | Detects whether **the citadel process's own environment** has a reachable X11/Wayland session (`DISPLAY`/`WAYLAND_DISPLAY` + a live socket probe, `session_linux.go:16-36`). Already flows into the heartbeat: `internal/status/collector.go:361` (`status.Desktop = desktop.DetectCapabilities()`) and `:366` (`DesktopCapabilities`). | **Exists, but was built for VNC/screenshot/input capability, not physical-presence proof — see §2.2 for why reusing it as-is is a real hazard.** |

**Net: no surface exists today that (a) an unattended headless service can
render to asynchronously, (b) a human can trust as physically-local, and (c)
doesn't leak through a log an agent can read.** This is genuinely new work.

## 2. Capability model

### 2.1 The realistic population

Per this repo's own CLAUDE.md (deployment topology, systemd units, the
Windows/macOS/Linux platform notes, the Bonsai/vLLM node examples), the
overwhelming majority of citadel nodes are **headless GPU servers running
`citadel work` as a background service** — rented hardware, home GPU boxes, or
cloud instances, administered over SSH/`terminal_exec`, not sat in front of.
A small minority are desktop/workstation-class machines (the `devicemode`
laptop profile is adjacent but out of scope — no worker, no job queues, not a
node `node:exec` targets) or, longer-term, "Citadel OS" devices meant to be
interacted with locally.

The capability model has to be honest about this split rather than assume a
screen exists.

### 2.2 Reporting headed vs. headless — and why the existing signal isn't enough on its own

`session.DetectDesktop()` already answers "can this process reach an X11/
Wayland session," but it answers a **different question** than "is a human
physically at this machine right now." Two gaps matter for a
security-sensitive use:

1. **`DISPLAY`/`WAYLAND_DISPLAY` are process-inherited env vars, not proof of
   a local seat.** An admin's own SSH session with X11 forwarding
   (`ssh -X`), or a stray `Xvfb` a previous job left running, can make
   `HasDesktop=true` (`session_linux.go` only checks the socket is reachable,
   line 28-34) with no physical monitor involved at all.
2. It was built to gate **VNC/screenshot/input** affordances
   (`session.go:26-30`'s own comment: "so a headless node does not advertise
   ... VNC/screenshot/input actions") — a "can I remote-control this" signal,
   which is a materially weaker property than "a human is standing in front
   of this box."

**Recommendation:** report a **new, narrower capability** for pairing display
— do not overload `DesktopCapabilities`. Model it the same way
`CollectorConfig` already threads optional live signals (`WorkerLiveness`,
`SwapStats` — `internal/status/collector.go:56-66`): an explicit,
default-nil/false accessor, so an un-upgraded or non-participating node
reports nothing and the backend's existing fallback (today's hardcoded
`return False`) is unaffected. The three tiers:

| Tier | What it means | How reported |
|---|---|---|
| `server` (headless) | No local screen at all — the default. | Capability absent/false. Falls back to linked device, exactly like today. |
| `console-reachable` | No physical screen, but a human has independent local/SSH shell access to run a **pull** command (see §3). This is the pragmatic minimum — see §6. | Capability true, surface = `pull`. |
| `headed` | A real attached display citadel can render to (desktop/workstation, or eventually Citadel OS). | Capability true, surface = `console`/`tui`/`gui`, gated by a *harder* presence check than bare `session.DetectDesktop()` — see §3.3. |

## 3. Rendering mechanism(s)

Pick per surface, ordered by how much new risk/engineering each adds.

### 3.1 Pull command (recommended primary mechanism)

Instead of pushing pixels to a screen, have the node hold the pending code in
memory (populated by the job in §5) and expose a **local-only, foreground,
human-typed** command to read it back — e.g. `citadel exec-code` or folded
into `citadel status`. Key property: **the requesting agent cannot invoke
this to get the code for itself**, because the whole reason a grant is being
requested is that the agent has *no shell on the node yet* — `node:exec` is
exactly what's being escalated. Only a human with independent access (their
own SSH key, physical login, or an operator credential the agent doesn't
have) can run it. This satisfies the acceptance criterion ("the code is never
present in any agent-readable output") without touching a framebuffer, a VT,
or root privileges, and it works identically on every headless node in the
fleet — i.e., the realistic majority.

Guardrails: refuse when stdin/stdout isn't a real TTY (so a script or an
agent's own subprocess call can't silently harvest it), and never write the
value through `clilog`/anything journald-backed — print directly to the
command's own foreground stdout, which for a one-shot `citadel` invocation
(not the long-running `citadel work` unit) is not captured by
`StandardOutput=journal+console`.

### 3.2 Control Center TUI banner (best-effort)

If Control Center is already attached (`instance.IsRunning`, per §1.3), push
a modal/banner into it via the same instance-server IPC `runControlCenter`
already stands up for attach support. Purely additive: a human who happens to
have the TUI open sees it immediately; nobody else is affected. Not the
primary mechanism because the TUI usually isn't running on a production
headless node.

### 3.3 Physical console / GUI (stretch, headed nodes only)

For genuinely headed machines: either write large text directly to the local
VT (`/dev/tty1`/`/dev/console`, requires root and a getty-owned VT — fragile
across distros/desktop managers) or pop a GUI toast when a desktop session is
confirmed present. **If the GUI path is built, gate it on something stronger
than the existing `session.DetectDesktop()`** — e.g. also require the session
be on the local seat (`loginctl show-seat seat0` on systemd-logind systems)
rather than merely "an X socket answered," per the spoofability gap in §2.2.
Treat this whole tier as lower-assurance than §3.1 until that hardening
exists.

### 3.4 Explicitly not doing

- Reusing `display_show`/`display_clear` (§1.2) — wrong trust model, wrong
  registry, disallows non-hosted content by design.
- KVM rendering — not citadel's channel (§1.3).
- Plain `citadel work` stdout — reaches journald (§1.3).

## 4. TTL / auto-clear contract

- **The TTL is backend-owned, not a citadel constant.** The backend's
  challenge TTL is `_CHALLENGE_TTL_SECONDS = 600` today
  (`aceteam_mcp_node_exec_grant.py:68`) — the job payload dispatched to the
  node should carry `ttl_seconds` explicitly so a future backend-side change
  doesn't silently drift from a hardcoded citadel value.
- The node holds the code **in memory only**, never on disk, never logged —
  same posture as `internal/worker/request_recorder.go`'s process-local,
  unpersisted state (cited in this repo's CLAUDE.md as the existing precedent
  for "ephemeral, in-memory-only signal is the right call here").
- Auto-clear fires a background timer at TTL expiry, same shape as the
  existing `autostop.go` idle-reconciler pattern (runs off an existing tick,
  no new polling loop).
- Support an explicit **clear-early** job (fired on `node_exec_confirm_grant`
  success or `node_exec_revoke_grant`) so the code doesn't linger past its
  usefulness even before the 10-minute TTL — mirrors the grant's own
  revoke path.
- The grant's own lifetime (24h/30d/indefinite) is unrelated and outlives the
  display entirely; only the *challenge* has this TTL.

## 5. Backend coordination (what aceteam#6975 must still provide)

This is explicitly a two-repo change; §1.1 already shows the exact seam:

1. **Capability ingestion**: `fabric_node_status.py` needs a field to receive
   citadel's new capability tier (§2.3) so `_node_screen_delivery` has
   something to branch on. Today there is nothing to consult — the stub's
   `return False` isn't just unimplemented, it's *unimplementable* without
   this.
2. **Job dispatch**: implement the body of `_node_screen_delivery` to push a
   new job type (proposed `SHOW_PAIRING_CODE`) to the node's per-node Redis
   stream, following `dispatch_node_update`'s exact shape (`aceteam_mcp_fabric.py:398`)
   — bounded wait for a result, node round-trip does not block indefinitely.
   Payload: `{code, ttl_seconds, grant_request_id}`. Result:
   `{displayed: bool, surface: "pull"|"tui"|"console", reason?}` — **never
   the code itself**, since the job result is What gets logged/returned
   through the existing fabric job-result path.
3. **Fallback timing**: decide whether the node-screen attempt must complete
   inside `node_exec_request_grant`'s own synchronous MCP response, or can be
   fire-and-forget with the tool response saying "code delivered" the moment
   *either* channel accepts it — the existing fallback chain (§1.1) already
   tries node-screen before the human-only channels, so whatever timeout
   `dispatch_node_update` already tolerates for `AGENT_UPDATE` is a reasonable
   starting budget.
4. **Send a `CLEAR_PAIRING_CODE` job** on confirm/revoke (§4).

## 6. Phased issue breakdown

> **Superseded by §8.6.** This ordering (capability-first, pull-command-second,
> headed rendering as a stretch) was the original recommendation; the owner
> direction is P0 = the minimal headed-node path that makes
> `_node_screen_delivery` return True. Kept for the reasoning; do not build
> from this list.

- **Phase 1 — capability reporting only.** Add the new, narrow pairing-display
  capability tier to the heartbeat (`CollectorConfig`-style optional
  accessor, §2.3). Reports `server`/absent everywhere by default. Zero
  behavior change on the backend until it's wired to read it — safe to ship
  alone.
- **Phase 2 — pull command + job handler.** New `SHOW_PAIRING_CODE`/
  `CLEAR_PAIRING_CODE` `worker.JobHandler` (modeled on `APPLY_DEVICE_CONFIG`'s
  shape, `internal/jobs/config_handler.go`) that populates the in-memory
  TTL'd holder; a `citadel exec-code`-style pull command per §3.1. This alone
  covers the realistic (headless, SSH-administered) fleet majority.
- **Phase 3 — Control Center TUI banner.** Best-effort push into an
  already-attached TUI instance (§3.2).
- **Phase 4 — stretch, headed nodes.** Physical VT / GUI toast rendering
  (§3.3), gated by a hardened local-seat check. Scope only if Phase 1's
  capability data shows a non-trivial population of genuinely headed nodes.
- **Phase 5 — aceteam repo (tracked against #6975 or a reopened follow-up,
  not this repo).** Implement `_node_screen_delivery`'s body + capability
  ingestion per §5. Blocks true end-to-end delivery regardless of how far
  Phases 1-4 get.

## 7. Open questions for Jason

1. **Is the pull command (§3.1/Phase 2) sufficient, or is literal
   on-screen rendering (Phase 4) required for the threat model #6975 cares
   about?** My honest read: for the headless majority, the pull command gets
   ~the same security property ("the requesting agent cannot read the code")
   as a physical screen would, for a fraction of the engineering and none of
   the fragility (root, VT ownership, desktop-manager differences, the
   DISPLAY-spoofing caveat in §2.2). The literal "screen" framing in the
   issue may be optimizing for a node population (desktop/Citadel-OS-class
   machines with a monitor) that's currently small relative to the fleet.
   Worth confirming whether Phase 4 is a real near-term need or a
   later-vintage nice-to-have once Citadel OS ships more broadly.
2. Does Control Center TUI see enough real production usage to justify
   Phase 3, or is it mainly a dev/debug surface today? (This repo's own
   CLAUDE.md already lists it as missing several heartbeat fields other
   collectors get — it may not be the right investment target.)
3. For genuinely headed nodes, should a GUI-toast path exist in v1 at all
   given the spoofability gap (§2.2/§3.3), or should headed nodes just use
   the same pull-command/TUI path as everyone else until a hardened
   local-seat check exists?
4. Should the node-screen attempt block `node_exec_request_grant`'s
   synchronous response, or be async/fire-and-forget (§5.3)? This changes
   the MCP tool's latency characteristics and needs an aceteam-side call.
5. Confirm the safe default: an un-upgraded node (old citadel binary,
   no capability field at all) should report **no capability**, so the
   backend's current fallback-safe stub behavior (linked device only) is
   preserved with zero coordination risk during rollout.

---

# Part II — Implementation design

Everything below was verified against the code on 2026-08-30 (main @ v2.130.0);
file/line references are to that state. The governing rule for the whole
design, stated once here because every mechanism below derives from it:

> **`delivered: true` is a claim that a human can currently see the code, and
> it SUPPRESSES the backend's linked-device fallback.** A false positive means
> the human never receives the code at all — strictly worse than a false
> negative, which merely costs one push notification. Every ambiguous case
> (can't confirm a text console, can't confirm write access, unsupported OS,
> render error) must therefore resolve to `delivered: false` with a `reason`.

## 8. Job / control-message contract

### 8.1 Job types

Two new per-node job types, added to the const block and `allKnownJobTypes` in
`internal/worker/job.go` (the doc comment at line ~99 makes the pairing of
those two lists mandatory — the supported-type set reported on an
unsupported-type failure, issue #382, is probed from `allKnownJobTypes`):

```go
JobTypeShowPairingCode  = "SHOW_PAIRING_CODE"
JobTypeClearPairingCode = "CLEAR_PAIRING_CODE"
```

Verb-first matches the existing style (`DOWNLOAD_MODEL`,
`APPLY_DEVICE_CONFIG`). Deliberately nothing containing `DISPLAY_` — see §1.2
on the unrelated org display-casting feature.

**Watchdog/lane classification** (`internal/worker/deadline.go`,
`gpu_tracker.go`): neither type joins `longSessionJobTypes`,
`unboundedJobTypes`, `serializedLaneJobTypes` (they touch neither
`citadel.yaml` nor `modules.lock`), nor `gpuBoundJobTypes`. Both run inline on
the default 60-min watchdog tier and return in well under a second — the TTL
clear is a background `time.AfterFunc` owned by the manager (§12), **not** a
sleeping handler.

**Privilege gate**: both handlers require `isPerNodeStream(job.SourceQueue)`
(`internal/worker/agent_update.go:334-341` — `strings.Contains(sourceQueue,
":node:")`), failing closed with a structural reason exactly like
`AGENT_UPDATE` (agent_update.go:213-219), `MODULE_SET`, `EXPOSE_SET`, and
`WHATSAPP_PROVISION`. A pairing code must only ever arrive as a
platform-dispatched, node-targeted job, never off a shared org pool where any
capable node could claim it.

### 8.2 Payloads

`SHOW_PAIRING_CODE`:

```json
{
  "code": "12345678",
  "ttl_seconds": 600,
  "grant_request_id": "gr_abc123",
  "requested_by": "Agent Ops (agent) for user jane@…"
}
```

- `code` (required): opaque string, max 16 chars (defense against a hostile
  or buggy dispatcher using the console as a billboard; today's backend sends
  8 digits, `_CODE_DIGITS = 8`). Rendered verbatim — citadel does not parse
  or validate its format beyond the length cap and printable-ASCII check.
- `ttl_seconds` (required): display lifetime. Clamped to `[30, 900]`; when
  absent/zero, default 600 to match the backend's `_CHALLENGE_TTL_SECONDS`
  (`aceteam_mcp_node_exec_grant.py:68`). Per §4 this is backend-owned — the
  backend should send its *remaining* challenge TTL, not a fresh 600, if it
  retries delivery.
- `grant_request_id` (required): correlation handle. This — never the code —
  is what appears in every log line, error string, and the clear-job payload.
- `requested_by` (optional): human-readable requester line rendered under the
  code so the person at the console knows what they'd be approving. Free
  text, rendered with the same length cap discipline (truncate at 80 chars).

`CLEAR_PAIRING_CODE`:

```json
{ "grant_request_id": "gr_abc123" }
```

Clears only if the currently-displayed code belongs to that
`grant_request_id`; a mismatch or nothing-displayed is an idempotent success
(`{"cleared": false, "reason": "not_displayed"}`), never a failure — the
backend fires this on confirm/revoke and must not see spurious job failures
for a code that already expired.

**Idempotency / replacement:** a second `SHOW_PAIRING_CODE` for the same
`grant_request_id` re-renders and resets the TTL timer (delivery retry). A
`SHOW` for a *different* `grant_request_id` replaces the current display
(latest-wins — the newest grant attempt is the one the human is acting on).

### 8.3 Result shape (what `_node_screen_delivery` consumes)

`JobResult.Output` (`internal/worker/job.go`, flows to the backend via
`StreamWriter.WriteEnd`):

```json
// SHOW_PAIRING_CODE
{ "delivered": true,  "surface": "console" }
{ "delivered": false, "surface": "", "reason": "graphical_session" }
// reasons: "unsupported_os", "no_console", "graphical_session",
//          "permission_denied", "render_error", "bad_payload"

// CLEAR_PAIRING_CODE
{ "cleared": true }
{ "cleared": false, "reason": "not_displayed" }
```

**The code never appears in `Output`, in `JobResult.Error`, or in any
formatted string the handler produces** — see §10. Backend mapping: the body
of `_node_screen_delivery` dispatches `SHOW_PAIRING_CODE` to the node's
per-node stream following `dispatch_node_update`'s exact shape
(`aceteam_mcp_fabric.py:398` — push + bounded await), and returns
`bool(result.output.delivered)`; a dispatch failure, node timeout, or job
FAILURE all return `False` and fall through to the linked-device chain,
preserving today's behavior byte-for-byte for every node that can't display.

### 8.4 Where the handler lives and registers

- `internal/worker/pairing_display.go` — a **native** `worker.JobHandler`
  (the `expose_set.go` shape: struct + `CanHandle` for both types +
  `Execute`), NOT a legacy `internal/jobs` handler behind
  `LegacyHandlerAdapter`. Native keeps the code's transit surface minimal
  (no `jobs.JobContext`, no legacy output plumbing) and matches the other
  per-node-stream-gated handlers.
- Registered in `cmd/nodejobs.go`'s `buildNodeJobHandlers` — the shared
  registration site for BOTH `citadel work` (`runWork`) and the control
  center's own consume loop (`runTUIWorker`, cmd/controlcenter.go:1892), so
  a control-center-only node handles the job identically (the
  competing-consumer lesson in that file's header comment).
- The handler delegates all state/rendering to a new leaf package,
  `internal/pairingdisplay` (§9, §12) — `internal/worker` stays free of VT
  ioctls, and the manager is testable without a worker.

Audit note (verified 2026-08-30): `internal/worker`'s runner and both job
sources log job IDs, types, and errors — never `job.Payload` (grep over
`runner.go`, `api_source.go`, `redis_source.go`). The payload is the only
place the code exists in transit on the node, so this must stay true; the
handler's own doc comment should state it so a future "log the payload on
parse failure" debug change trips over the warning.

## 9. Rendering channels, in priority order, with the actual mechanism

The realistic population split (§2.1) drives the order. "Headed" here means
**a physical monitor showing a Linux text console** — the homelab/colo GPU box
with an HDMI monitor and a getty, which is the headed case citadel's actual
fleet has. A desktop-session machine (X/Wayland owning the seat) is
explicitly NOT coverable by the P0 mechanism and reports `delivered: false`.

### 9.1 P0 — Linux virtual-terminal (text console) rendering

Mechanism, precisely:

1. **Resolve the active VT**: read `/sys/class/tty/tty0/active` (kernel-owned,
   world-readable; yields e.g. `tty1`) → target device `/dev/tty1`. Absent or
   unreadable (container, no VT subsystem) → `reason: "no_console"`.
2. **Confirm it is a text console, not a display server's VT**: open the
   device and `ioctl(fd, KDGETMODE)` (`0x4B3B`, via `golang.org/x/sys/unix`).
   `KD_TEXT` → proceed; `KD_GRAPHICS` (X/Wayland/KMS owns the seat) or ioctl
   error → `reason: "graphical_session"`. This is the load-bearing check:
   **`session.DetectDesktop()` (`internal/session/session_linux.go`) is the
   wrong gate here** — it reads `DISPLAY`/`WAYLAND_DISPLAY` from the
   *worker's own environment*, and the fleet worker is a root systemd unit
   (`install.sh` `[Service]` block, ~line 502: no `User=`, `HOME=/root`) with
   neither var set even when a desktop session owns the seat for some user.
   From that process, DetectDesktop reports "headless" unconditionally;
   KDGETMODE asks the kernel what the seat is actually doing. Writing text to
   a `KD_GRAPHICS` VT succeeds and is invisible — the exact false-`delivered:
   true` failure the §8 rule forbids.
3. **Write access**: `os.OpenFile("/dev/ttyN", os.O_WRONLY, 0)`. The fleet
   worker is root → writable. A `citadel service install` unit runs as the
   invoking user (`internal/service/systemd.go:123`, `User=%s`) — writable
   only with `tty` group membership; `EACCES` → `reason:
   "permission_denied"`. No privilege escalation is attempted.
4. **Render**: a single `write()` of an ANSI sequence — clear screen + home
   (`\x1b[2J\x1b[H`), an ASCII box, the code in spaced wide digits (simple
   3-line block digits, ~30 lines of table; prior art for the box style is
   `internal/ui/devicecode.go:166`'s enrollment code box), the
   `requested_by` line, and **both** an absolute expiry (`valid until 14:32
   UTC`) and the TTL (`for the next 10 minutes`) so a stale render after a
   crash is self-describing. Direct `write()` to the char device — this
   bypasses stdout/journald entirely (§10).
5. **Clear** (TTL expiry, `CLEAR_PAIRING_CODE`, or graceful shutdown):
   clear-screen + a one-line `pairing code cleared/expired` note. The getty
   prompt was overwritten at show time; the next keypress redraws it —
   acceptable for a console, and the reason §13's "how aggressive" question
   goes to Jason.

Windows and macOS: build-tagged stubs returning `reason: "unsupported_os"`.
(macOS `osascript` dialog and a Windows toast are P3 — see §11; they are
desktop-session mechanisms and need the §3.3 presence-hardening discussion
first.)

### 9.2 P1 — pull command (`citadel pairing-code`), the headless-fleet answer

§3.1's recommendation stands, but Part I glossed over the hard part: **the
code lives in the memory of the long-running `citadel work` process, and the
pull command is a different process.** A cross-process transport is required,
and the options were checked against the code:

- The worker's status HTTP server is off by default (`--status-port 0`,
  cmd/work.go:3390), is unauthenticated locally, and under `--gateway` its
  `/status` + `/worker` routes are additionally served over the **mesh VPN
  listener** (cmd/work.go:2220-2221) — the code must never ride it, in any
  field, existence-flags included.
- `internal/instance`'s socket (`~/.citadel-cli/citadel.sock`) is a raw PTY
  attach relay for the TUI, not request/response, and lives in the
  invoker-scoped ConfigDir — wrong protocol, wrong directory.

Design: a dedicated one-shot request/response Unix socket,
`<network.GetNodeConfigDir()>/pairing.sock`, mode `0600`, served by the
worker only while a code is pending (listener starts on SHOW, closes on
clear). Machine-convergent dir (the #383/#726/#845 rule) so a root worker and
a non-root operator shell agree on the path; note `0600` + a root worker
means the human runs `sudo citadel pairing-code` — same privilege bar as
every other operation on a fleet node. Client refuses when stdout is not a
TTY (cosmetic guard against casual scripting; the *real* boundary is the
socket's file mode — see §10.4's actor-equivalence). Prints the code to its
own foreground stdout only — a one-shot CLI invocation, not captured by the
worker unit's `StandardOutput=journal`.

**Capability consequence:** shipping this flips every headless node to
"capable," which changes the backend's screen-vs-linked-device economics
(the human must be told, via the linked-device push or web UI, to go run the
command — the pull surface can't announce itself). This interaction is a
genuine product question (§13 Q3) and is why the pull command is P1, not P0.

### 9.3 P2 — Control Center TUI banner

Sharper than §3.2 thought, in the good direction: when no dedicated
`citadel work` holds the worklock, the control center runs its own consume
loop off the same handler set (`runTUIWorker`) — **the handler and the TUI
are the same process**, so a banner needs no IPC at all, just an observer
callback on the `pairingdisplay` manager that posts a tview modal. When a
dedicated worker holds the lock, the handler runs over *there* and pushing
into a separately-running TUI would need a new instance-socket message type —
deferred. P2 ships the same-process case only (delivered stays `false`
unless the console path also succeeded — a TUI banner proves a human *ran a
TUI once*, not that anyone is watching; it is additive UX, not a delivery
surface, until §13 Q4 says otherwise).

### 9.4 Explicitly out (unchanged from §3.4, plus one addition)

No `display_show` reuse, no KVM rendering (not citadel's channel — though
the backend's own `kvm_*` tools could be a *backend-side* delivery arm for
BMC-attached hosts; named here as an aceteam-side idea, not designed), no
`citadel work` stdout, and — new — **no field on `/status` or the heartbeat
that carries or acknowledges a pending code** (the capability field of §11
is static "could display," never "is displaying X").

## 10. The security invariant, enforced structurally

The invariant: the code must never land anywhere an agent can read it back.
Per path:

1. **No stdout / journald / clilog.** The renderer writes with a direct
   `os.OpenFile`+`Write` on the VT character device. The worker's stdout is
   journald (`StandardOutput=journal`, install.sh; `journal+console`,
   systemd.go:73) and journald is agent-readable via `citadel logs` /
   `SHELL_COMMAND` `journalctl` — so *any* print of the code from the worker
   process is a leak by construction. The `pairingdisplay` package takes no
   logger for the code path; its log lines carry `grant_request_id` only.
2. **No disk.** The code exists in exactly two places: the job payload in
   transit (worker memory) and the manager's private field (worker memory).
   The crash marker (§12) deliberately contains VT name + expiry + grant ID —
   never the code. The pull-command socket (P1) transmits it over a `0600`
   unix socket, never writes it.
3. **The job result is `delivered`/`cleared` + `reason` only** (§8.3), and —
   the subtle half — **no error string may embed the code** (error strings
   travel through `WriteError` into backend logs and the requester-visible
   job result). Enforced by a test that runs the handler with a sentinel
   code against every failure branch (fake renderer erroring, bad payload,
   graphics mode) and asserts the marshaled result, the error text, and
   every captured log line do not contain the sentinel. This is the
   load-bearing test of the PR.
4. **Actor equivalence, stated honestly:** these defenses close *passive*
   channels (logs, world-readable state, result echo, mesh endpoints). They
   cannot defend against an actor who already has arbitrary code execution
   as root/the worker user on the node — such an actor can read
   `/dev/vcs1`, the socket, or process memory. That is consistent with the
   threat model: `node:exec` *is* the capability being escalated, so the
   requesting agent by definition lacks it; and an agent that already holds
   an equivalent grant has nothing to gain from the code. The residual gap
   is a *different* agent/user in the org with an existing shell grant on
   this node — it could harvest a code meant for a new grant. That gap is
   inherent to any on-node display (a camera pointed at the monitor beats
   it too) and is accepted; the backend's max-5-attempts + org-admin-only
   confirm bound the blast radius.

## 11. Capability reporting

New heartbeat field, additive/omitempty, following the exact
`CollectorConfig` optional-provider pattern (`internal/status/collector.go:53`
— `WorkerLiveness`/`SwapStats`/`Reservations`):

```go
// internal/status/types.go
type PairingDisplayCapability struct {
    // Surfaces this node can render a pairing code on right now.
    // P0: ["console"]. Later: "pull", "tui", "gui".
    Surfaces []string `json:"surfaces"`
}
// NodeStatus:
PairingDisplay *PairingDisplayCapability `json:"pairing_display,omitempty"`

// CollectorConfig:
PairingDisplay func() *PairingDisplayCapability // optional; nil ⇒ omitted
```

- The probe is `pairingdisplay.DetectSurfaces()` — steps 1–3 of §9.1
  (resolve VT, KDGETMODE, open-for-write probe) without the write. Cheap
  (two opens, one ioctl), run per collection like the other providers.
- Wired in `cmd/work.go`'s two heartbeat collector construction sites only,
  with the projection (`pairingdisplay` type → `status` type) in `cmd`,
  mirroring `swapStatsFrom`/`reservationsFrom` — `internal/status` imports
  neither `internal/worker` nor the new package. The TUI-only collector
  (`cmd/controlcenter.go`) keeps its documented gap, consistent with
  WorkerLiveness/Swap/Reservations/Lanes.
- Deliberately NOT folded into `DesktopCapabilities`
  (`desktop.DetectCapabilities`, collector.go:375) — per §2.2 that signal
  answers "can I remote-control this" from the process env and is
  spoofable/irrelevant here; and per §9.1 it is actually *inverted* for a
  root systemd worker.
- **Cross-repo dependency (named, not designed):** aceteam's
  `fabric_node_status.py` ingest must persist `pairing_display` off the
  heartbeat payload, and `_node_screen_delivery` should consult it to skip
  the node round-trip for the (majority) headless fleet. Until that lands,
  the backend MAY dispatch `SHOW_PAIRING_CODE` blindly and branch on
  `delivered` — correct either way, just paying one bounded job round-trip
  per grant on headless nodes (§13 Q2). An un-upgraded citadel reports no
  field ⇒ backend treats as headless ⇒ today's behavior (§7 Q5, confirmed).

## 12. TTL / auto-clear lifecycle

Owner: `pairingdisplay.Manager`, a process-wide singleton
(`pairingdisplay.Get()`, mirroring the `GetCobrowseManager` precedent —
needed because the handler (`cmd/nodejobs.go` construction) and the
heartbeat probe (`cmd/work.go` construction) must share state), configured
once with the machine-convergent state dir threaded from `cmd`
(`network.GetNodeConfigDir()`), keeping the package a leaf that imports
neither `internal/network` nor `internal/worker` and is fully testable with
an injected fake `Renderer`.

- **Show**: write the crash marker FIRST (`pairing-display-state.json` in
  the node config dir: `{vt, expires_at, grant_request_id}` — no code),
  then render, then arm `time.AfterFunc(ttl)`.
- **Clear** paths, all idempotent, all converging on the same
  `clearLocked()`:
  1. TTL timer fires → render "expired" clear → zero state → delete marker.
  2. `CLEAR_PAIRING_CODE` (backend confirm/revoke, §4's clear-early) →
     render "cleared" clear → same.
  3. Graceful shutdown: `Manager.Shutdown()` deferred in `runWork` (and
     `runTUIWorker`) → clear + marker delete, so SIGTERM/systemd stop never
     strands a code.
- **Killed before clear (SIGKILL, crash, power loss):** VT text is just
  characters — it survives the process. Three-layer mitigation:
  1. The render itself shows absolute expiry + TTL (§9.1.4), so a stale
     screen is self-describing, and the code is cryptographically useless
     after the backend's 600s challenge TTL / 5 attempts regardless.
  2. Startup reconcile: `runWork` (and `runTUIWorker`) call
     `pairingdisplay.ReconcileStale()` before the consume loop — marker
     present ⇒ clear that VT + delete marker. Marker-then-render ordering
     (above) means a kill between the two clears a VT that was never
     written: harmless (one clear-screen on a getty).
  3. Residual: node dies and never comes back up ⇒ code stays on a
     powered-off/frozen screen until the backend TTL kills its value.
     Accepted.
- Reboot: VTs reset; the leftover marker triggers one harmless clear.

## 13. Open questions for Jason (Part II — supersedes §7 where they overlap)

1. **Console aggressiveness.** P0 clears the active VT and holds it for up
   to 10 minutes — on a box where someone is logged in at the physical
   console, that stomps their visible session (input/state unharmed, but
   the screen is taken over). Acceptable for a security prompt, or should
   citadel refuse (`delivered: false, reason: "console_in_use"`) when the
   active VT has a live logged-in session (detectable via
   `utmp`/`loginctl`), only rendering over an idle getty? My lean:
   stomping is *correct* for a pairing prompt (it is precisely a "someone
   wants access to this machine" interrupt), but it's a product call.
2. **Dispatch-blind vs capability-first ordering.** May the backend
   implement `_node_screen_delivery` by dispatching blindly and branching
   on `delivered` (works the day citadel P0 ships; costs one bounded
   round-trip per grant on headless nodes), or must capability ingestion
   (§11's aceteam half) land first?
3. **Pull command posture (P1).** Confirm wanted at all given §10.4's actor
   equivalence, and whether `citadel pairing-code` should demand the
   `grant_request_id` (shown in the requester's UI) as an argument — a weak
   human-in-the-loop binding that makes drive-by harvesting by a co-resident
   agent slightly harder, at the cost of the human typing a handle.
4. **Is the TUI banner (P2) worth building** (§7 Q2 restated — Control
   Center production usage unclear), given it can't honestly claim
   `delivered` on its own?

## 14. Phased plan (supersedes §6) — files and functions

### P0 — headed-node console path (one PR, Sonnet-sized)

Makes `_node_screen_delivery` able to return True end-to-end on a
root-worker Linux node with a text console. New/touched:

| File | Change |
|---|---|
| `internal/pairingdisplay/manager.go` (new) | `Manager` singleton (`Get()`, `Configure(stateDir)`): `Show(code, ttl, grantID, requestedBy)`, `Clear(grantID)`, `Shutdown()`, `ReconcileStale()`, crash marker read/write (no code in marker), `time.AfterFunc` TTL, injected `Renderer` interface. |
| `internal/pairingdisplay/render_linux.go` (new) | `consoleRenderer`: active-VT resolve (`/sys/class/tty/tty0/active`), `KDGETMODE` gate (via `golang.org/x/sys/unix`), open/write/clear; `DetectSurfaces()` probe (§11). Build tag `linux`. |
| `internal/pairingdisplay/render_other.go` (new) | `!linux` stubs → `unsupported_os`; `DetectSurfaces()` → nil. |
| `internal/pairingdisplay/manager_test.go` (new) | Fake-renderer tests: TTL fire, clear-early, replacement, marker lifecycle, reconcile, shutdown. No `/dev` access. |
| `internal/worker/pairing_display.go` (new) | `PairingDisplayHandler` (native, both types): payload validation/clamps, `isPerNodeStream` gate, result shapes (§8.3). |
| `internal/worker/pairing_display_test.go` (new) | Gate test (org-pool queue refused), result shapes, **the §10.3 sentinel-leak test**. |
| `internal/worker/job.go` | Two consts + `allKnownJobTypes` entries. |
| `internal/status/types.go` | `PairingDisplayCapability` + `NodeStatus.PairingDisplay`. |
| `internal/status/collector.go` | `CollectorConfig.PairingDisplay func()` provider + attach in `Collect`. |
| `cmd/nodejobs.go` | Register the handler in `buildNodeJobHandlers` (both consumers get it). |
| `cmd/work.go` | `pairingdisplay.Configure(network.GetNodeConfigDir())` + `ReconcileStale()` in `runWork` (near the existing `ReconcileOrphanedReservations` call), deferred `Shutdown()`, provider wiring at the two heartbeat collector sites + the small projection func (the `swapStatsFrom` pattern). |
| `cmd/controlcenter.go` | Same Configure/Reconcile/Shutdown in `runTUIWorker`. |

Non-goals in P0, stated in the PR: no pull command, no TUI banner, no
GUI/desktop-session rendering, no macOS/Windows, no aceteam-side change.
Manual test plan: on a Linux box/VM with a VT (not a container), dispatch
the job via the direct-Redis debug path with a dummy code; verify render,
TTL clear, `CLEAR_PAIRING_CODE`, `kill -9` + restart reconcile, and
`delivered:false` under a running X session.

### P1 — pull command (headless fleet)

`cmd/pairingcode.go` (new command, TTY-gated), `internal/pairingdisplay/
socket.go` (0600 one-shot unix socket in the node config dir, listener
scoped to code-pending), `DetectSurfaces()` gains `"pull"`. Blocked on §13
Q3 and on the aceteam-side UX for telling the human to run it.

### P2 — Control Center banner

Observer hook on `Manager` + tview modal in the `runTUIWorker` process
(§9.3). Additive UX only; does not set `delivered`.

### P3 — desktop-session + other OS rendering

GUI toast/dialog for X/Wayland seats (needs the §3.3 hardened local-seat
check — `loginctl`-based, since the worker's env is useless for this, §9.1),
macOS `osascript`, Windows toast. Scope only if P0's capability data shows a
real headed-desktop population.

### P4 — aceteam repo (cross-repo, tracked there)

`_node_screen_delivery` body (dispatch + `delivered` branch, §8.3),
`fabric_node_status.py` capability ingestion (§11), `CLEAR_PAIRING_CODE`
dispatch on confirm/revoke (§4). Named contract only — not designed here.
