# Design: on-node display of the `node:exec` pairing code (citadel #659)

Status: design only, no implementation. Companion to aceteam-ai/aceteam#6975
(CLOSED — backend grant flow already shipped), which explicitly stubs out the
on-node rendering this issue owns.

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
