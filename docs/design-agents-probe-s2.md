# Design: agents-probe S2, the owner signal and the `citadel work` wiring (aceteam #8993)

Status: DESIGN ONLY. No implementation in this PR. Part of aceteam-ai/aceteam#8993
(DoR-v2 slice S2, child issue aceteam #9157). Builds on S1 (citadel #995) and the
S2 gate fixes (citadel #1005, shipped in #1012 / v2.152.0).

## Context

S1 shipped `citadel agents probe` and `internal/agentsprobe.Probe`: a read-only,
local detector for four vendor coding-agent CLIs (claude, codex, gemini,
opencode) reporting `{name, installed, version, authed, adapter_class}`. It is
correct as an operator-run command because it runs AS the operator: its own
`HOME` and `PATH` are the right ones to inspect.

The DoR-v2 on #8993 defines S2 as the `node_agents_list` MCP surface: "a new
python-backend route (sibling pattern of `routes/aceteam_mcp_capability_probe.py`)
that requests probe results from the node over the existing gateway/relay path
and returns them." That is a PULL. The node half of it is what this document
designs: `Probe` running inside the long-lived `citadel work` process, where
neither `HOME` nor `PATH` is the right one to inspect, and a way to hand the
result to the backend.

The #1005 review named the crux and deliberately deferred it. #1012 built the
seam (`Probe(ctx, Options{HomeDir, PathEnv})`) and a first resolver
(`ResolveTargetUser`, `SUDO_USER` plus a passwd lookup), and its own doc
comment states the limit plainly: a shipped `citadel-worker.service` is not
launched via `sudo`, `SUDO_USER` is unset, and the resolver falls back to the
process home, `/root`, which is "the SAME wrong answer the #1005 motivation
describes." S2 must choose how a bare-systemd root worker determines WHICH
user's environment to probe. Everything else in S2 follows from that choice.

Vocabulary: the "target user" is the account whose `HOME`/`PATH` the probe
inspects. The "owner signal" is whatever evidence the worker uses to pick it.

## 1. Current state, read from the code

### 1a. The three unit shapes the fleet actually runs

Read before reasoning about "the worker": there are three, and they differ on
exactly the fields that matter here.

| Unit | Written by | `User=` | `HOME` | How `citadel init` ran |
|---|---|---|---|---|
| `citadel-worker.service` (fleet) | `install.sh` (`curl \| sudo -E ... bash`) | none (root) | `/root` | as root, with `SUDO_USER` inherited from `sudo -E` |
| `citadel-worker.service` (VM image) | `packer/scripts/04-citadel.sh` | `citadel` | `/home/citadel` | `su - citadel -c "citadel init ..."` (`06-firstboot.sh`) |
| `citadel.service` (`citadel service install`) | `internal/service/systemd.go` `generateSystemUnit` | the `SUDO_USER` who ran install | that user's home | interactively, usually by the same user |

Only the first shape has the problem. In the other two the worker process IS
the target user, so `os.UserHomeDir()` and the process `PATH` are already
right, and #1012's fallback gives the correct answer with no further signal.

### 1b. How the root fleet worker already finds the human's node directory

`internal/network.GetStateDir` (and `GetNodeConfigDir`, its parent) is the
machine-convergent resolver hardened by #383 for precisely the sudo-vs-service
divergence this document is about. Its resolution order is documented on the
function; the two facts S2 depends on are:

1. Under `install.sh`, `citadel init` runs as root but with `SUDO_USER` set
   (`sudo -E` preserves it). `getOwnerHomeDir` resolves the owner home via
   `platform.GetSudoUser`, so the node directory is created under the HUMAN's
   home (`~<human>/citadel-node`), not `/root`. `EnsureStateDir` then calls
   `fixStateDirOwnership`, whose `resolveChownTargetWith` chowns the node
   config dir and the `network/` state dir to `SUDO_USER`'s uid/gid. Root
   `init` also writes the machine-global pointer file under `/etc/citadel`.
2. The root worker later starts with no `SUDO_USER` and `HOME=/root`, and
   `resolveStateDir` step (1) reads that pointer file, converging on the same
   `~<human>/citadel-node`. That is how the fleet avoids the duplicate-node
   failure today.

The consequence S2 can lean on: on the fleet's primary unit shape, the node
config directory is ALREADY owned by the human whose environment the probe
wants, and the root worker ALREADY resolves that directory on every boot. No
new persisted state is required to recover the target user; the filesystem
owner of a directory the worker already resolves carries it.

### 1c. What the current resolver gets wrong, exactly

With `Probe(ctx, ResolveTargetUser())` on the root fleet worker:

- `HomeDir` = `/root`. `/root/.claude/.credentials.json` is absent, so
  `claudeAuthState` returns a CONFIDENT `AuthStateNo` (`credentialFileState`
  only degrades to `AuthStateUnknown` off-linux or when a relocation env var is
  set in the worker's own environment, which it is not).
- `PathEnv` = `userPathEnv("/root", <systemd minimal PATH>)`. A CLI installed
  with `npm i -g` under the human's `~/.npm-global/bin`, or via nvm, is not on
  it, so `Installed=false`, a confident false in the OTHER dimension too.

Both are the "confident false no" the package's own honesty principle exists to
prevent. A node whose owner has `claude` installed and authenticated would
report `installed:false` for every vendor, and the fleet-wide `node_agents_list`
would be uniformly, quietly wrong on the unit shape most nodes run.

### 1d. The existing sibling patterns S2 should reuse, not reinvent

- Cross-process/cross-context state: `network.GetNodeConfigDir()` (never
  `platform.ConfigDir()`), the rule CLAUDE.md states under `ConfigDir()`.
- Directory-owner uid check: `internal/platform/cobrowse_basedir_unix.go`
  (`validateCobrowseBaseDirPerms`) already reads `info.Sys().(*syscall.Stat_t)`
  behind a `!windows` build tag with a Windows stub. Same shape here.
- Leaf-package seam: `internal/agentsprobe` imports only the standard library.
  It must stay that way (it must not import `internal/network`, which pulls
  tsnet into every importer), so the node config dir is passed IN as a string,
  the same seam shape as `Client.SetStalePendingReclaimMinIdle` and
  `config.DeviceConfigDirsHook`.
- Node-side pull endpoints: `internal/status/agent.go` `registerAgentRoutes`
  (`/agent/node-info`, `/agent/doctor`, ...), all behind `requireVPNOrAuth`,
  fed by closures in `cmd/agent_tools.go`. The backend's
  `aceteam_mcp_citadel_agent.py` `_agent_request(node_id, "GET", path)` already
  validates org ownership, resolves the node's VPN IP, and calls
  `http://<vpn_ip>:<status_port><path>` through `_vpn_http_client` (the SOCKS
  relay, per the Railway caveat), with a clean 503 mapping when no status server
  is up. `citadel_node_info` is a complete worked example of the pattern.
- Heartbeat additive fields: `status.NodeStatus.Lanes`/`Swap`/`Cache` plus a
  hand-mirrored type, a `<x>From` projection in `cmd/work.go`, and a
  reflection shape-parity test. The `CacheReport` comment states the convention
  applies even when the leaf IS importable without a cycle.

## 2. The owner signal (the crux)

### 2a. Options

**(a) Read the active unit's `User=`.** Parse `systemctl show -p User --value
<unit>` for the unit the worker is running under (`INVOCATION_ID` says whether
there is one; the unit name would come from `service.ActiveManagedUnit`'s
enumeration).

- Correctness on the fleet: FAILS on the primary shape. `install.sh`'s unit has
  no `User=` line at all, so the directive is empty (root). It only returns a
  useful value on the two shapes that do not need it (they already run as the
  target user).
- Cost: a `systemctl` exec and a unit-name guess inside the worker; Linux-only;
  nothing on macOS/launchd or Windows.
- Verdict: reject. It answers "whom does systemd run me as", which is the
  process uid the worker already knows. It never answers "whose node is this".

**(b) Owner uid of the node config directory.** `os.Stat(network.GetStateDir())`
(fall back to `GetNodeConfigDir()`), read `Stat_t.Uid`, `user.LookupId` for the
username and passwd home. Note the home comes from passwd, NOT from the
directory's path: the node dir can legitimately live outside the home (the
`/etc/citadel` shapes), and the passwd home is what the vendor CLIs use.

- Correctness on the fleet: CORRECT on the primary shape by construction
  (section 1b): the human ran `sudo -E`, `init` chowned the dir to them, the
  root worker resolves the same dir. Correct on the other two shapes (the dir
  is owned by the process user, which is the target). Agrees with `SUDO_USER`
  under `sudo citadel work`. Zero new state, zero new config, nothing for the
  operator to set.
- Inherited failure modes, stated rather than assumed away. (b) is exactly as
  good as `GetStateDir` convergence and not one bit better:
  - A root worker with NO pointer file and no `SUDO_USER` (the #383 shape,
    e.g. onboarded via a non-root `citadel login` that could not write
    `/etc/citadel`, then a root unit hand-written later) resolves `/root`'s
    node dir, owned by uid 0, so the target is root. That node is ALSO
    registering as a duplicate node today, so the probe is not the first thing
    wrong with it, but it must still report honestly (see the mapping below).
  - `resolveChownTargetWith` defaults the chown target to a user named
    `citadel` when `SUDO_USER` is empty. On a true-root install (`ssh root@`,
    `SUDO_USER` empty or `root`) on a box that happens to have a `citadel`
    account, the dir is chowned to that account and (b) targets
    `/home/citadel`. That is the packer image's intended owner, so it is
    usually right, but it is a guess the chown made, not the probe.
  - `fixStateDirOwnership` skips the chown entirely when the parent basename is
    not `citadel-node` and does not match the global config's node dir. The
    `network/` state dir is chowned in every branch where a chown happens at
    all, which is why S2 stats `GetStateDir()` first and only falls back to the
    parent.
- Multi-user nodes: (b) picks the account that ran `citadel init`. On a box
  where a different human is the one with `claude` installed, that human is
  invisible. This is the case (c) exists for; (b) alone does not cover it and
  should not pretend to.

**(c) Explicit config field.** `agents_probe_user: <username>` in `citadel.yaml`
(next to `pinned_services`, read from the manifest `runWork` already loads).

- Correctness: exact when set. It is the only option that handles the
  multi-user node and the misresolved-owner edge cases above.
- Cost: nobody sets it by default, so on its own it changes nothing on the
  fleet; it is an override, not a signal. A passwd lookup failure for the
  configured name must be an explicit error (target unresolvable), never a
  silent fallback to some other user.
- Verdict: keep, as the top of a layered resolver, not as the answer.

**(d) Enumerate candidate human users** (`/home/*` with a real login shell)
and probe each.

- Correctness: finds everything, including things the node's owner did not ask
  the org control plane to know. On a shared workstation this reports OTHER
  accounts' installed tools and authentication state to the org, which is a
  privacy problem, and it multiplies the exec surface of section 2d by the
  number of accounts.
- Cost: N probes per refresh; an ambiguous result shape (which one is "the"
  node's agent?) that every consumer then has to disambiguate.
- Verdict: reject for S2. If a real multi-tenant node ever needs it, it is a
  deliberate `agents_probe_users: [...]` allowlist under (c), opted into per
  node, not a default scan.

### 2b. Recommendation: a layered resolver, (c) over (b) over the existing chain

`agentsprobe.ResolveTargetUserForNode(in ResolveInputs) (Target, error)` with
inputs `{ConfiguredUser string, StateDir string, NodeConfigDir string,
SudoUser string, ProcessUID int}`, dependency-injected lookups (the
`resolveTargetUser` / `resolveChownTargetWith` testable-core pattern), and
this order, first match wins:

1. **Configured user** (`agents_probe_user`): `user.Lookup(name)`. Lookup
   failure is an error, not a fallthrough.
2. **Node-dir owner uid**: stat `StateDir`, else `NodeConfigDir`; `user.LookupId`.
   Stat failure or a uid with no passwd entry falls through.
3. **`SUDO_USER`** (non-empty, not `root`): the existing #1012 branch.
4. **Process user**: the existing #1012 fallback.

Each `Target` carries `{Username, UID, GID, HomeDir, PathEnv, Signal}` where
`Signal` is one of `config | node-dir-owner | sudo-user | process`. `Signal`
is not decorative: it is what lets a consumer (and a human reading a heartbeat)
tell "probed jason's environment" from "probed root's environment on a
root-owned node", which are the two outcomes that look identical today.

Honesty mapping, the rule every branch protects:

| Situation | Target | What `Probe` reports |
|---|---|---|
| Owner resolves to a non-root human (fleet common case) | that user | full probe of their `HOME`/`PATH`, `signal: node-dir-owner` |
| Owner resolves to uid 0 and the worker is root | root | full probe of `/root`, `signal: node-dir-owner`, `target_user: root`; a consumer can see nobody else's environment was inspected |
| Configured user does not exist in passwd | none | probe with zero `HomeDir`: every installed vendor reports `AuthStateUnknown` (the existing `probeVendor` branch), plus a `resolve_error` string on the snapshot |
| Node dir cannot be stat'ed and no other signal | process user | as #1012 today, `signal: process` |

The design does NOT add a rule of the form "owner is root but other homes
exist, so degrade to unknown". That would be (d) sneaking back in through the
side door, and the `signal`/`target_user` fields already make the root case
attributable instead of ambiguous.

### 2c. PATH reconstruction: extend the static list, never source a profile

`userPathEnv` prepends `~/.npm-global/bin`, `~/.local/bin`, `~/bin`. Its doc
comment names the gap: nvm/volta/bun/asdf per-version bin dirs. The tempting
fix (run the user's login shell and read `$PATH` back) is forbidden here for
the same reason section 2d exists: it executes the user's shell profile with
the worker's privileges. S2 extends the static list with read-only globs
instead (`~/.nvm/versions/node/*/bin`, `~/.volta/bin`, `~/.bun/bin`,
`~/.cargo/bin`, `~/.local/share/pnpm`, plus `/opt/homebrew/bin` and
`/usr/local/bin` if absent from the process PATH). A glob is a directory
listing, not code execution, and it is what makes `Installed` stop being a
confident false for an nvm-installed CLI. Still a known gap after S2: an
install location none of those cover; `Installed=false` there remains a
confident false, documented, not fixed.

### 2d. Privilege: the `--version` exec must run as the target user

This is the finding the task list did not name and a reviewer would block on.
On the fleet shape the worker is root and the target's `PATH` starts with
directories the target user WRITES (`~/.npm-global/bin`). `probeVersion` would
then exec a user-writable binary as root. Any local account that can write its
own home (every account) could plant a `claude` there and get code run as root
on the next probe. That is a local privilege escalation, and it is introduced
by S2, not by S1 (S1 runs as the invoking operator).

Required, not optional, in S2:

- When `os.Getuid()==0` and `Target.UID != 0`, exec `<path> --version` with
  `SysProcAttr.Credential{Uid, Gid, Groups}` (Linux and macOS; build-tagged
  `!windows`) so the child runs with the TARGET's privileges, the same
  privileges the user has when they run it themselves.
- Give the child a minimal env, `HOME=<target home>`, `PATH=<target path>`,
  `USER`/`LOGNAME=<target>`, not the worker's env. Node CLIs side-write on
  `--version` (`~/.claude`, `~/.config/<vendor>`, update-check caches); run as
  root with `HOME=/root` they write to root's home, and run as root with
  `HOME=<user>` they leave ROOT-OWNED files in the user's home, which breaks
  the user's own CLI later with `EACCES`. Both are latent landmines the
  credential drop closes.
- Reading the credential files (`jsonFileNonEmptyObjectState`) stays as the
  worker: root bypasses the `0600` mode, which is acceptable because only
  existence and JSON-object shape are read, never a value, and the S1 contract
  already forbids values reaching the output. State this in the code comment so
  a future "log the parse error with a snippet" edit trips on it.
- The 0600 read as root is the ONLY place the worker's privilege touches the
  target's files. No chown, no mkdir, no write of any kind in the target's home.

What the drop does not do: it does not make a hostile binary safe for the
target user themselves (it runs with their privileges, exactly as if they typed
it), and it does not bound resource use beyond the timeouts in section 4. Both
are the same posture the user already has by installing the tool.

## 3. `node_agents_list` wiring: how results leave the worker

### 3a. The three candidate surfaces

| Surface | Fits DoR-v2 S2? | Freshness | Cost model | Fleet-wide query |
|---|---|---|---|---|
| Pull endpoint (`/agent/*`, backend `_agent_request`) | yes, literally its definition | on demand, can force a refresh | one probe per request, rate-limited | no (fan-out per node) |
| Heartbeat field (`NodeStatus.<x>`) | not named by DoR-v2 | whatever the cache holds, ~30s publish | must read a cache, never probe (section 3c) | yes, if the backend persists it |
| Job type (`AGENTS_PROBE` via the job stream) | no, and no consumer exists | on demand | full job lifecycle for a 5s read | no |

A job type is rejected outright: it is the heaviest path for a read-only
introspection call, there is no backend dispatcher for it, and the `/agent/*`
pattern was built for exactly this class of request (`citadel_node_info`,
`citadel_doctor`).

### 3b. Recommendation: pull endpoint is the S2 contract; heartbeat is an optional read of the same cache

**Primary: `GET /agent/vendor-agents`** registered in `registerAgentRoutes`
(`internal/status/agent.go`) behind `requireVPNOrAuth`, fed by a new
`AgentProvider.VendorAgents func(refresh bool) (any, error)` closure wired in
`cmd/agent_tools.go`. The name avoids "agent agents" and sits beside
`/agent/node-info`; the MCP tool name stays `node_agents_list` per the DoR.
Query `?refresh=1` forces a probe (subject to the min-gap in 3c); the default
serves the cached snapshot. The aceteam half (a `routes/aceteam_mcp_*`
sibling calling `_agent_request(node_id, "GET", "/agent/vendor-agents",
params={"refresh": "1"})`) is #9157 and out of citadel-cli scope; this document
fixes the wire shape only.

Response shape (the snapshot, section 3c):

```json
{
  "probed_at": "2026-09-09T12:00:00Z",
  "stale": false,
  "target": {"user": "jason", "uid": 1000, "home": "/home/jason", "signal": "node-dir-owner"},
  "resolve_error": "",
  "agents": [
    {"name": "claude", "installed": true, "version": "2.1.0", "authed": "authed", "adapter_class": "claude-code-hooks"},
    {"name": "codex", "installed": false, "adapter_class": "codex-exec-headless"},
    {"name": "gemini", "installed": true, "version": "0.9.1", "authed": "unauthenticated", "adapter_class": "zed-acp"},
    {"name": "opencode", "installed": false, "adapter_class": "unknown"}
  ]
}
```

`agents[]` is `agentsprobe.VendorAgent` unchanged (the S1 JSON contract, so
`citadel agents probe --json` and the endpoint agree). `target` and
`resolve_error` are the S2 additions from section 2b. Before the first probe
completes (the worker just booted) the endpoint returns `probed_at` zero,
`stale: true`, and an empty `agents` list, never a fabricated one; `503` is
reserved for "no worker in this process" exactly as the sibling routes use it.

**Secondary: `NodeStatus.VendorAgents *VendorAgentsReport` (`vendor_agents`,
omitempty)**, a hand-mirrored copy of the same snapshot in `internal/status`,
projected by `vendorAgentsReportFrom` in `cmd/work.go`, pinned by a
`TestVendorAgentsShapeParity`, fed through `CollectorConfig.VendorAgents
func() *VendorAgentsReport` (the `LaneActivity`/`CacheReport` pattern). Nil
until the first probe completes and nil under the kill switch, so a legacy or
disabled node's heartbeat is byte-identical. The provider ONLY reads the cache.

Why include the heartbeat at all when the DoR names only the pull: the pull
answers "what is on node N" and requires fan-out for "which nodes have an authed
claude", the question a scheduler or `find_capability`-style tool asks. The
heartbeat answers the fleet question for free once the cache exists. It is the
piece to DROP if the owner wants the narrowest S2 (open question 2). Whether the
backend persists the field is aceteam-side work; every prior additive field
(`lanes`, `swap`, `cache`) landed the same way.

The control-center collector (`cmd/controlcenter.go`) does not get this field,
consistent with the `WorkerLiveness`/`Swap`/`Lanes` gaps already noted there.

### 3c. Cadence and caching: a probe is an exec plus a possible network call, so it is rare

`agentsprobe.Service` (new, in the leaf package, unit-testable with a fake
`Probe`): holds an `atomic.Pointer[Snapshot]`, a singleflight guard (one probe
in flight, concurrent callers wait on it rather than starting a second), and a
min-gap between forced refreshes.

- **Startup**: one probe, launched on its own goroutine from `runWork` after
  the status-publisher goroutines are up. It never blocks boot; the heartbeat
  simply omits the field until it lands. Same construction-order posture as
  `nodeRunner` (the #717 lesson), so the pointer is an `atomic.Pointer`, not a
  plain var.
- **Periodic refresh**: `CITADEL_AGENTS_PROBE_INTERVAL_SECONDS`, default 3600,
  `0` disables the timer. Justification for an hour rather than the ~30s
  heartbeat: each refresh execs up to four binaries, and the package doc
  already records that some vendors run an update check on `--version`. A
  30s cadence would turn every node into an hourly-times-120 update poller
  on the user's behalf. Auth state changes on human timescales (someone runs
  `claude login`); an hour of staleness is fine for discovery, and the pull
  with `refresh=1` covers "I just logged in, check again".
- **On-demand refresh**: `?refresh=1` runs a probe unless one completed within
  `agentsProbeMinRefreshGap` (60s), in which case the cached snapshot is
  returned with `stale: false`. The gap bounds a backend that retries.
- **Kill switch**: `CITADEL_AGENTS_PROBE=off` (falsey values as in
  `WORKER_SELF_HEAL`): no startup probe, no timer, the endpoint returns
  `{"disabled": true}` with 200, the heartbeat field is omitted. Default ON is
  the recommendation and open question 1 explains why that is the owner's call.

The heartbeat's `OnStatus` fan-out and the collector's `Collect` never wait on a
probe: they `Load()` the pointer and return. That single property is what
section 4 relies on.

### 3d. `citadel agents probe` gains `target` in its output

The operator command keeps a zero `Options` by default (S1 behavior is
byte-unchanged for the table and JSON `agents` shape) but adds `--target-user
<name>` and, for JSON, the same `target` block the endpoint returns. Under
`sudo`, an operator can then run `sudo citadel agents probe --target-user
jason` and see exactly what the worker sees, including whether the version
exec ran with dropped privileges. Not `--as-worker` (which would need the
worker's resolved state) and not a silent default change.

## 4. Worker-context safety

Confirmed from the code, not asserted:

- `probeVersion` already runs `exec.CommandContext` under a 5s
  `probeTimeout` with `cmd.WaitDelay = time.Second` (the #1005 minor, landed in
  #1012), so a `--version` that daemonizes or leaves a child holding the stdout
  pipe cannot wedge `Output()` past ctx expiry plus one second.
- `cmd/agents.go` bounds the WHOLE probe at 30s (`agentsProbeTimeout`,
  sequential over four vendors). The worker's `Service` derives every probe ctx
  from the root worker ctx (`cmd/work.go`'s `signal.Notify` cancel) with the
  same 30s cap, so shutdown cancels an in-flight probe and a single slow vendor
  cannot hold the goroutine past 30s.
- What `CommandContext` does NOT do: it kills the direct child only. A vendor
  binary that forks a grandchild (an update-check helper) leaves it running;
  `WaitDelay` only stops the parent from waiting on the pipe. Hardening for S2:
  `Setpgid: true` on the child and a `cmd.Cancel` that signals the whole
  process group, so the grandchild is reaped with the parent. Cheap, and it
  composes with the credential drop (same `SysProcAttr`).
- The heartbeat cannot stall on a probe because no heartbeat path calls
  `Probe`. `CollectorConfig.VendorAgents` and the `/agent/vendor-agents`
  default path read an `atomic.Pointer`; only the startup goroutine, the timer
  goroutine, and an explicit `?refresh=1` call ever probe, and the singleflight
  guard makes even those serialize onto one goroutine. A wedged vendor thus
  costs one leaked goroutine for at most 30s plus `WaitDelay`, never a missed
  heartbeat tick.
- The self-heal monitor is unaffected: the probe is not a job, so it touches no
  `WorkerState` counter and cannot trip STALL/STUCK either way.
- The probe never takes `worklock`, never reads or writes `citadel.yaml` or
  `modules.lock`, and never touches `os.Stdout` (so no `captureStdout`
  concern if a future local MCP tool exposes it).

## 5. Known gaps after S2, stated rather than closed

- `credentialFileState` reads relocation vars (`CLAUDE_CONFIG_DIR`,
  `CODEX_HOME`) from the WORKER's environment. A target user who set one in
  their own shell is invisible under a root worker, so a relocated credential
  file still reads as a confident `unauthenticated` on linux. Reading the
  target's environment would mean sourcing their profile (forbidden, 2c) or
  scraping `/proc/*/environ` of their live processes (fragile, and a privacy
  reach). Documented; a future per-vendor "also check `$XDG_CONFIG_HOME`
  default locations" extension narrows it without either.
- Windows: `lookPath`'s override walk is POSIX-only, there is no credential
  drop, and `Stat_t` does not exist; the Windows stub resolves `signal:
  process`. A Windows worker probes its own account, as S1 does today.
- `install.sh`'s idempotency check (line 472 at v2.152.0) looks for existing
  state at `/root/citadel-node/network`, but under `sudo -E` `init` writes
  under the human's home (section 1b), so that branch may never fire. The
  earlier `status --json` check makes it harmless today. Not S2's to fix;
  filed as a follow-up rather than changed here.
- The `citadel` default chown target (section 2b) is a guess made by
  `resolveChownTargetWith` for the packer image; on a true-root install with a
  coincidental `citadel` account it steers (b). `agents_probe_user` is the
  override.

## 6. Scope boundary and phasing

S2 is discovery-in-the-worker only. In scope: the layered resolver and its
`Target`/`Signal` (2b), the static PATH extension (2c), the credential drop and
minimal child env (2d), `agentsprobe.Service` with startup/timer/on-demand
refresh and the kill switch (3c), `/agent/vendor-agents` (3b), the heartbeat
mirror and parity test (3b, droppable), `--target-user` on the operator command
(3d), and tests with fake `Probe`, fake stat/lookup, and a fake unit shape per
row of section 1a's table.

Out of scope, deferred to the named slices: driving any agent turn (S4), the
`TurnReceipt` signer (S3), the platform template and `vendor_agent_turn` tool
(S5), receipt surfacing (S6), and the aceteam route itself (#9157). No AEP
signing touches S2: a probe result is an observation, not a receipt, and
signing it would be a proof-shaped claim about an environment the node does
not control.

Suggested landing order inside S2: resolver + drop + tests first (it is the
security-relevant piece and independently reviewable), then `Service` +
endpoint, then heartbeat mirror last (or not at all per open question 2).

## Open questions for the owner

1. **Default ON with a kill switch, or default OFF?** Every other advisory
   toggle in this codebase defaults OFF (`CITADEL_GROUNDING_GUARDRAIL`,
   `CITADEL_ENERGY_SAMPLING`, `SERVICE_AUTO_STOP_WHEN_IDLE`). This design
   recommends ON because a default-OFF probe makes `node_agents_list` return
   "disabled" for the entire fleet until operators opt in, which defeats S2's
   purpose, and because the probe is read-only with dropped privileges. The
   costs of ON are real: hourly execs of user binaries on every node and the
   vendor update-check traffic they may generate. #8993 also carries a "no
   feature flags" constraint; whether an operator env kill switch (the
   `WORKER_SELF_HEAL` class) counts as one is the owner's reading.
2. **Heartbeat field: include, or pull-only?** Pull-only is the literal DoR-v2
   S2. The heartbeat mirror is cheap once the cache exists and answers the
   fleet-wide question, but it is a new field the backend must choose to
   persist and the narrowest S2 omits it.
3. **Where does `agents_probe_user` live?** `citadel.yaml` (operator-editable,
   next to `pinned_services`, what this design assumes) or the machine-
   convergent device `config.yaml` (written by `init`, closer to the other
   identity fields). The former is the natural home for an operator override;
   the latter would let a future `init` prompt set it.
4. **Is uid-0-owned-dir plus root worker acceptable as "probe /root and say
   so", or should it degrade to `AuthStateUnknown`?** This design says probe
   and report `target_user: root`; it is honest and attributable, and the only
   alternative that is not (d) in disguise is to refuse. A true-root operator
   who installed `claude` as root is a real, if small, population.
5. **Periodic refresh default: 1h, or timer off (startup plus on-demand
   only)?** Timer-off minimizes background execs and vendor traffic to exactly
   one probe per worker boot, at the cost of a heartbeat that can be days
   stale (mitigated by `probed_at` being visible). 1h is the recommendation.
6. **Should `--target-user` on the operator command require root?** As
   designed it works for any caller who can already read the target's home
   (the kernel decides), which matches `citadel pairing-code`'s posture of
   letting file permissions be the boundary.
