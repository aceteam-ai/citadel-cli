# Design: Engine host-publish `bind:` escape hatch, exposure observability, and the ollama LAN-consumer gap (citadel#1023)

Follow-up design for citadel-cli#1023, the S8 slice of aceteam-ai/aceteam#9498
left open by aceteam-ai/aceteam#9523 (PR #1025). Design only; no Go changes in
this PR. Every file/symbol named below was read on `main` at 2a6acfb.

## Context

PR #1025 hardwired a `127.0.0.1:` literal in front of the host publish of
vllm, llamacpp, bonsai, unlimited-ocr and sglang (kokoro/omnivoice/tei were
already loopback). It also dropped the `:?` guard from those five templates'
`${CITADEL_<SVC>_HOST_PORT}` token — not for any runtime reason, but because
the two test-side host-port parsers (`composeHostPorts` in
`services/embed_test.go`, `hostPortField`/`publishedHostPorts` in
`internal/apps/hostport_collision_test.go`) only peel a single leading
`${...}` group and cannot see past a `:` inside it.

The parent acceptance list still wants: (1) a per-service `bind: all` escape
hatch in `citadel.yaml` with a warning printed at service start, (2) a
`citadel doctor` / `citadel status` warning when an engine is bound to all
interfaces, (3) a real fix for `ollama.yml`, which #1025 deliberately left on
`0.0.0.0` because `internal/apps/catalog.go`'s `ollama-webui` app reaches it
at `http://host.docker.internal:11434`, and (4) a per-entry decision for the
remaining `0.0.0.0` `ServiceMap` entries (extraction, diffusers, transcribe,
lmstudio).

Two findings from reading the code that change the shape of the work, beyond
what the issue anticipated:

- **The #1025 loopback change has not actually landed on any auto-updated
  node.** `cmd/compose_refresh.go`'s `enginePortRecreator` compares only the
  running container's `.HostPort` (`runningPublishedHostPort`), so a bind
  change on the same port never triggers a force-recreate; and
  `composerefresh.Sweep` only invokes the `Recreator` for a file it rewrote
  in that same sweep (`internal/composerefresh/composerefresh.go` — an
  already-current file `continue`s before the recreate branch). The
  auto-updater re-execs the binary without touching containers, so a fleet
  node that took v2.15x via auto-update refreshed `vllm.yml` to the loopback
  template on its next boot but is still serving `citadel-vllm` on
  `0.0.0.0:8201` until something recreates it. This is exactly the drift the
  doctor/status half of this issue exists to make visible — and it needs a
  fix of its own (§5.2), not just a warning.
- **The parser limitation is the reason the `:?` guard was lost, and it is
  also the thing that blocks mechanism (a).** Fixing the parser once (§3.1)
  unblocks the two-substitution form AND lets the `:?` guard come back —
  today a compose-up site that forgets `HostPortEnv()` substitutes the bare
  token to empty, and `127.0.0.1::8000` publishes on a random ephemeral port
  silently (docker treats an empty host port as "allocate one").

## 1. Security goal (the invariant every decision below serves)

An embedded inference engine has no auth of its own. Its host publish must
default to loopback on every node, on every start path, with no way to end
up on `0.0.0.0` by accident — only by an explicit, visible, per-service
operator decision that is (a) recorded in the manifest, (b) warned about at
every start, (c) visible in `citadel status`/`doctor`/the heartbeat, and (d)
undone by removing the decision and restarting. "By accident" includes: a
compose-up site that forgot to inject an env var, a materialized file left
over from an older template, a container left running across an
auto-update, and an operator removing `bind: all` without the on-disk file
following.

Legitimate reach to an engine from off-box is the gateway's model-routed
`/v1/chat/completions` over the mesh (`internal/gateway/chat_route.go`, #581)
— NOT the engine's host port. The escape hatch exists for the operator who
genuinely needs a LAN client on the raw port and accepts the exposure.

## 2. Decision summary

| # | Question | Decision | Rejected |
|---|---|---|---|
| 1 | How does `bind: all` reach the compose file? | **(a)** two-substitution `${CITADEL_<SVC>_BIND:-127.0.0.1}:${CITADEL_<SVC>_HOST_PORT:?…}:<cport>`; env injected per-`up` from the manifest; never persisted in the `.yml` | (b) materialization-time rewrite — pinned forever by `KnownComposeHashes`/stamp preservation, bakes exposure into disk, needs reconcile-on-every-call at two sites |
| 2 | Where does the exposure warning come from? | ONE owner: the env-building helper returns `[]string` warnings; both compose-env sites print them at `up` | five ad hoc `Log` calls, one per compose-up site |
| 3 | Where does detection live? | **Truth** = `docker inspect` `HostIp` on the running container, attached as additive `ServiceInfo.Bind` by the `internal/status` collector, read by status/services/heartbeat/doctor; **intent** = manifest `bind`; doctor reports the diff | parse only the materialized `.yml` (misses the running-container drift above) |
| 4 | Ollama | **(B)** shared external docker network `citadel`; ollama's host publish goes loopback like its siblings; `ollama-webui` joins the network and dials `http://citadel-ollama:11434` | (A) bridge-gateway publish (reachable by every third-party container, gateway IP not stable, fails to bind on Docker Desktop); (C) `bind: all` opt-in sized for the app (LAN exposure of an unauthenticated engine — defeats §1) |
| 5 | Sweep candidates | all four go loopback under the same mechanism; `nonLoopbackServiceMapAllowlist` empties | blanket allowlist forever |

## 3. Mechanism for `bind: all` — (a), two-substitution compose form

### 3.1 The compose form and the parser it requires

Every `ServiceMap` compose with a host publish moves to:

```yaml
ports:
  - "${CITADEL_VLLM_BIND:-127.0.0.1}:${CITADEL_VLLM_HOST_PORT:?citadel must supply CITADEL_VLLM_HOST_PORT}:8000"
```

and for the literal-port services (sglang, transcribe, lmstudio, tei):

```yaml
  - "${CITADEL_SGLANG_BIND:-127.0.0.1}:30000:30000"
```

Compose substitutes `${VAR:-default}` with the default when the var is unset
or empty, so a compose-up site that injects nothing gets loopback. That is
the property (b) cannot offer: under (a) the safe answer is the one you get
by doing nothing.

**Parser change (the exact work the issue asks to spell out).** Both test
parsers today do `strings.HasPrefix(spec, "${")` → find the first `}` →
treat that group as the host field. Under the new form that group is the
BIND, and `publishedHostPorts` then errors with "compose publishes unknown
host-port var" for `CITADEL_VLLM_BIND`. Rather than teach two test copies to
peel two groups, introduce ONE production parser that the doctor (§5) also
needs:

`services/portspec.go` (new, leaf, stdlib only):

- `SplitPortSpec(spec string) []string` — split on top-level `:` only,
  tracking `${` … `}` depth so a `:-`/`:?` inside a group is never a
  separator; strip a trailing `/tcp|/udp` from the last field.
- `ParsePortSpec(spec string) (PortSpec, error)` with
  `PortSpec{Bind, Host, Container string}` (raw tokens, may be empty):
  1 field → container-only (no host publish); 2 → `Host:Container`; 3 →
  `Bind:Host:Container`; anything else → error (the IPv6-bracket form is
  not used by any citadel compose and is deliberately unsupported until it
  is).
- `ResolveToken(token string, env map[string]string) (string, error)` —
  `${VAR}` → `env[VAR]` (empty if unset, mirroring compose); `${VAR:-d}` →
  env or `d`; `${VAR:?m}` → env or error `m`; literal → itself.
- Convenience: `HostPortOf(spec, env)`/`BindOf(spec, env)`.

Then:

- `services/embed_test.go` `composeHostPorts` → `ParsePortSpec` +
  `ResolveToken(p.Host, registryEnv)` where `registryEnv` is built from
  `HostPortEnv()` (drop the hand-copied `envVarHostPort` map — the parity
  it was approximating is now exact).
- `internal/apps/hostport_collision_test.go` `hostPortField` is deleted;
  `publishedHostPorts` calls `services.ParsePortSpec` and resolves `p.Host`
  the same way. The "unknown host-port var" error stays, now correctly
  scoped to the HOST token.
- `TestEngineComposeFilesLoopbackBound`'s two assertions INVERT: it now
  asserts `ResolveToken(p.Bind, nil) == "127.0.0.1"` (default is loopback)
  AND that `p.Host` carries a `:?` guard (the guard is back — see the
  Context finding). The `loopbackBoundEngineHostPorts` map becomes a set of
  service names; the token spelling is derived from the registry.
- `TestServiceMapBindSweep`: for every entry with a host publish,
  `ResolveToken(p.Bind, nil)` must be `127.0.0.1`, else the entry must be in
  `nonLoopbackServiceMapAllowlist` with a reason. After Phase 4 that
  allowlist is asserted EMPTY.
- New `TestBindEnvOverrideReachesCompose`: `ResolveToken(p.Bind,
  map{"CITADEL_VLLM_BIND":"0.0.0.0"}) == "0.0.0.0"` for every entry — proves
  the hatch is actually wired to the template, not just present in Go.
- New registry-parity assertion (mirrors the existing "registry must agree
  with compose" check in `TestHostPortNoCollisions`): the var name inside
  every entry's `p.Bind` equals `services.BindEnvVarName(<svc>)`.
- Every phase that edits a compose file regenerates
  `services/known_hashes.go` (`go run ./services/compose/genhashes`);
  `TestKnownComposeHashesCoverCurrentTemplates` enforces it.

### 3.2 Registry (`services/ports.go`)

Alongside `serviceHostPortEnv`, add `serviceBindEnv map[string]string`
(`"vllm": "CITADEL_VLLM_BIND"`, …, keyed identically — including the
literal-port services sglang/transcribe/lmstudio/tei, which today have no
`serviceHostPortEnv` entry at all), `BindEnvVarName(svc) (string, bool)`,
and:

```go
// BindEnv returns "CITADEL_<SVC>_BIND=<ip>" for every service whose resolved
// policy is NOT loopback. Loopback services are deliberately omitted: the
// template's ${VAR:-127.0.0.1} default is the loopback path, so an absent var
// and an explicit loopback are the same thing and a site that injects nothing
// is safe. Returns the warning text the caller must print, one per exposed
// service.
func BindEnv(policy map[string]string) (env []string, warnings []string)
```

`policy` values: `""`/`"loopback"` → omitted; `"all"` → `0.0.0.0`; an
IPv4 literal (validated with `net.ParseIP(...).To4() != nil`) → itself.
Anything else is a manifest validation error at load time, never a silent
loopback fallback.

Two notes on values worth writing down: `0.0.0.0` is IPv4-only, whereas the
pre-#1025 no-IP form (`"${PORT}:8000"`) published dual-stack (`[::]` too) —
an operator on an IPv6-only LAN gets a narrower hatch than the old default,
which is acceptable and documented, not a bug. An IPv4 literal must exist on
the host at `up` time or docker refuses the bind — the loud direction, but a
boot-order hazard for the one genuinely attractive literal: a `citadel up`
(kernel-TUN, #643) node's mesh IP, giving "mesh-only, not LAN". Document it;
do not paper over it.

### 3.3 Manifest field

```yaml
services:
  - name: vllm
    bind: all          # or an IPv4 literal; absent/"loopback" = default
```

Modeled in BOTH `cmd/manifest.go`'s `Service` and
`internal/jobs/service_handler.go`'s `manifestService` as
`Bind string yaml:"bind,omitempty"`. This is not optional: `Service`'s own
`EvictedByJob` comment documents why — `writeManifest` does a full struct
round-trip, so a field one struct doesn't model is silently dropped by that
tree's next manifest rewrite. Neither struct WRITES the field in v1 (operator
edits it), both must round-trip it.

**Loading, once, in a leaf both trees already import.** `HostPortEnv()` is
manifest-independent; a bind env is not. `internal/compose` is already the
leaf `cmd` and `internal/jobs` share for materialization rules
(`EnsureNamespacedContainerName`), so add `internal/compose/bindpolicy.go`:
`LoadBindPolicy(manifestPath) (map[string]string, error)` — a minimal
`services[].{name,bind}` yaml parse (the same minimal-struct idiom
`serviceManifest` uses) with the validation above. A missing manifest is an
empty policy (loopback everywhere), a malformed `bind:` value is an error the
compose-up site surfaces rather than a fallback.

### 3.4 Injection sites and the start-time warning

Today `HostPortEnv()` is appended at five places (grep `HostPortEnv()`):
`cmd/service.go composeEnv()`, `internal/jobs/service_handler.go
ServiceHandler.composeEnv()`, `internal/jobs/config_handler.go:579`
(APPLY_DEVICE_CONFIG's own compose-up), `internal/jobs/llamacpp_inference.go:65`
(the model-swap `--force-recreate`), and transitively `compose_refresh.go`'s
recreator via `composeEnv()`. Recommended shape:

- `cmd/service.go composeEnv()` takes no `configDir` today; thread one in
  (`composeEnvFor(configDir)`) so it can `LoadBindPolicy`. `composeCommand`
  is already the choke point every cmd/TUI compose call goes through
  (`startService`, `ccStartService`, `citadel run`, module start), so this
  is one edit, not N.
- `ServiceHandler.composeEnv()` already has `h.ConfigDir`; same call.
- Fold `config_handler.go:579` and `llamacpp_inference.go:65` onto
  `ServiceHandler.composeEnv()` (they hand-build the same env today) so the
  warning has TWO owners, not five. Both are `internal/jobs` already.
- The warning is printed only on an `up`/`--force-recreate` path (not on
  `ps`/`logs`/`down`, which share `composeEnv` for the `:?`-guard reason
  #525 documents). Text, one line per exposed service, at every start:
  `WARNING: <svc> is published on ALL interfaces (0.0.0.0:<port>) because
  citadel.yaml sets 'bind: all'; it has no auth of its own — anything on
  the LAN can use it. Remove 'bind:' and restart to go back to loopback.`
  For an IP literal, name the IP instead. Under `citadel mcp`'s local tools
  this goes through `captureStdout` like every other start-path line.

Removing `bind: all` and restarting is the complete undo: nothing on disk
records the old value, so the next `up` resolves the default. Compare (b),
where the operator would ALSO have to know the materialized file needs
regenerating — and where, if they didn't, `composerefresh.Sweep` would
classify the rewritten file as a hand-edit (hash matches neither the stamp
nor `KnownComposeHashes`) and preserve it on every future upgrade, so the
exposure would outlive both the manifest decision and every subsequent
template fix. That is the decisive argument against (b); "fail-safe default"
is the second one.

**Governance (v1):** `bind` is local-manifest-only. It is deliberately NOT
settable through `APPLY_DEVICE_CONFIG`, `MODULE_SET`, or a `SERVICE_START`
payload: widening an unauthenticated engine to the LAN is a local trust
decision about the physical network the node sits on, which the platform
cannot see. Open question §8.

## 4. `citadel doctor` / `citadel status` warning

### 4.1 Where detection lives: truth from the container, intent from the manifest

There are three places the bind can be stated, and they can disagree:

1. the manifest `bind:` (intent);
2. the materialized `<configDir>/services/<svc>.yml` + the env citadel
   would inject (what the NEXT start will do);
3. the running container's `NetworkSettings.Ports[].HostIp` (what is
   exposed RIGHT NOW).

(3) is the only one that is true on an auto-updated node (Context finding
1), so it is the primary signal. `runningPublishedHostPort`
(`cmd/compose_refresh.go`) already inspects `NetworkSettings.Ports` with a
Go template that emits `.HostPort`; generalize it to emit `.HostIp .HostPort`
pairs and move it to `internal/status` (or `internal/compose`) as
`RunningHostBindings(engineBin, container) ([]HostBinding, bool)`.
Classification: `127.0.0.1`/`::1` → loopback; `0.0.0.0`/`::`/`""` → all;
anything else → that IP.

The `internal/status` collector already inspects every managed service's
container per collection (footprint, health), so attach the result there:

```go
// ServiceInfo (internal/status/types.go), additive:
Bind string `json:"bind,omitempty"` // "all" or an IP; omitted when loopback or unknown
```

Omitted-when-loopback keeps a loopback-only heartbeat byte-identical (the
same additive/omitempty posture as `Pinned`, `Footprint`, `Idle`). One field,
four readers: `citadel status`, `citadel services`, the heartbeat (so the
platform can render it), and `doctor`.

### 4.2 What each surface prints

- **`citadel status` / `citadel services`:** one warning line per service
  with a non-loopback `Bind`, under the services table (not a new column —
  the table is already wide): `⚠ vllm is published on all interfaces
  (0.0.0.0:8201); it has no auth of its own.` When the manifest has no
  `bind:` for that service, append `— container predates the loopback
  template; run 'citadel stop vllm && citadel start vllm' to apply` (that
  is the drift case, §5.2). No docker call beyond the inspect the collector
  already performs.
- **`citadel doctor`:** a new `ENGINE EXPOSURE` section rendered from the
  same collection, one check per managed service with a host publish
  (`agentDoctor`'s existing `{name, ok, detail}` shape,
  `cmd/agent_tools.go`), three outcomes:
  - `[OK] vllm: loopback (127.0.0.1:8201)`
  - `[WARN] vllm: all interfaces (0.0.0.0:8201) — bind: all in citadel.yaml`
    (intended, operator decision)
  - `[WARN] vllm: all interfaces (0.0.0.0:8201) but citadel.yaml says
    loopback — running container predates the loopback template (drift)`
  - Cold path (service not running): resolve (2) — parse the materialized
    `.yml` with `services.ParsePortSpec`, apply `LoadBindPolicy` — and print
    `[INFO] vllm: not running; next start publishes on <resolved bind>`.
    This also catches a materialized file older than the bind-var template.

  `doctorReport.ok()` is deliberately docker-only today (its comment
  explains why job-routing checks must not flip the exit code on an idle
  node). Recommendation: neither the intended-`bind: all` nor the drift
  case flips the exit code in v1 — the first is a decision, the second has a
  self-heal (§5.2) so it should be rare. Open question §8.

## 5. Ollama

### 5.1 The constraint, precisely

`internal/apps/docker.go Install` runs every catalog app with `docker run`
on the default bridge, publishes the app itself on `127.0.0.1:<port>`
already, and adds `--add-host host.docker.internal:host-gateway`.
`host-gateway` resolves to the default bridge's gateway (172.17.0.1 unless
`bip` is set), so from inside `ollama-webui` a connection to
`host.docker.internal:11434` arrives at the host with destination
172.17.0.1 — which a `127.0.0.1:11434:11434` publish does not answer (the
docker-proxy/DNAT rule is keyed on the bind IP). That is why #1025 left
ollama on `0.0.0.0`.

Two things the issue did not note. First, `internal/services/native.go`
supports ollama running natively (`ollama serve` under systemd; see also
`internal/compose/psstate.go`), and native ollama's default `OLLAMA_HOST` is
`127.0.0.1:11434` — so the `host.docker.internal` path is ALREADY broken for
native-ollama nodes today unless the operator set `OLLAMA_HOST=0.0.0.0`.
The catalog entry only works against the docker ollama, and only because it
is on `0.0.0.0`. Second, every citadel-side consumer of ollama is host-local
(`internal/worker/llm_inference.go` `localhost:11434`,
`internal/jobs/ollama_inference.go`, `internal/workflow/nodes.go`,
`internal/status/collector.go`'s port table), so loopback costs them nothing.

### 5.2 Options weighed

**(A) Publish on the bridge gateway too** — a second `ports:` line
`${CITADEL_DOCKER_GATEWAY}:11434:11434` with citadel resolving the gateway IP
at `up` (`docker network inspect bridge`). Rejected: reachable by EVERY
container on the default bridge (any third-party container on the box, not
just citadel apps); the IP is deployment-specific (`bip`, rootless docker,
podman's `host.containers.internal` is a different mechanism); on Docker
Desktop there is no docker0 on the host, so the literal fails to bind and
`up` fails outright. It trades LAN exposure for "every container" exposure
and a per-platform failure mode.

**(C) Default loopback, `bind: all` for the app** — rejected on §1: it makes
the ONE catalog app that talks to ollama require LAN exposure of an
unauthenticated engine as its normal operating mode. The escape hatch is for
exceptions, not for the shipped default of a catalog entry.

**(B) Shared user-defined docker network — recommended.** Container-to-
container reach never touches a host publish, so ollama's publish can be
loopback exactly like its siblings, and the app path is identical on Linux,
Docker Desktop, and podman. Concretely:

- `services/compose/ollama.yml`:
  ```yaml
  networks:
    citadel:
      external: true
  services:
    ollama:
      networks: [citadel]
      ports:
        - "${CITADEL_OLLAMA_BIND:-127.0.0.1}:11434:11434"
  ```
  `external: true` (not a compose-created network) because a compose-created
  network's name is project-derived (`services_citadel` under the #528
  no-`-p` default, `citadel-<hash>_citadel` under `--node-dir`) — reasoning
  about project-derived names is exactly what went stale before (#693).
- `services.ServiceNetworks map[string][]string{"ollama": {"citadel"}}`
  declared once in `services/embed.go` next to `ServiceAuxFiles`, and
  `compose.EnsureNetwork(engineBin, name)` (idempotent `<engine> network
  create`, ignore "already exists") called from BOTH sides: the compose-up
  helpers (§3.4) before `up` for any service with a `ServiceNetworks` entry,
  AND `apps.Install` for any app manifest declaring it — an app can be
  installed before ollama has ever started, and `external: true` errors
  when the network is absent.
- `internal/apps`: `AppManifest.Networks []string` → `--network citadel`
  on `docker run`; `ollama-webui`'s env becomes
  `OLLAMA_BASE_URL=http://citadel-ollama:11434`. Use the CONTAINER name for
  DNS, not compose's service-name alias `ollama`: aliases on a shared
  external network collide across projects, container names are unique.
  (Under `--node-dir` the container is `citadel-<hash>-ollama`, #860; apps
  are not `--node-dir`-aware today, so this is not a regression.)
- Install-time resolution for the native case: if the ollama manifest entry
  is `type: native` (or no `citadel-ollama` container/compose exists),
  `apps.Install` keeps today's `http://host.docker.internal:11434` and does
  not join the network — behavior unchanged for that population (still
  requires the operator's `OLLAMA_HOST=0.0.0.0`). Whether to keep that
  fallback at all is open question §8.
- `nonLoopbackServiceMapAllowlist["ollama"]` is removed; ollama joins the
  loopback set in the sweep test; `ollama.yml`'s header comment (which
  currently explains why it is NOT loopback) is rewritten.

Security posture of (B) against §1: no host exposure beyond loopback; the
only additional reach is containers an operator explicitly attached to the
`citadel` network — the same trust boundary as "a container citadel started".

Cross-repo: the live claudecode/hermes module composes are in
`aceteam-ai/citadel-services` (the in-repo `claudecode.yml`/`hermes.yml` are
dead files, per #1023's comment; the dead copy carries no ollama env, so
there is no in-repo evidence either way about the live ones). If a
citadel-services module reaches ollama via `host.docker.internal:11434`, it
needs the same `networks: citadel: external: true` stanza; the `citadel`
network is the sanctioned way for ANY module to reach an engine
container-to-container. Open question §8.

## 6. Broader sweep — a per-entry decision each

With the mechanism in place every remaining `0.0.0.0` entry gets the same
treatment; the decision per entry is only "any consumer that is not
host-local?":

| Service | Today | Consumers found | Decision |
|---|---|---|---|
| extraction | `${CITADEL_EXTRACTION_HOST_PORT:?…}:8100` (0.0.0.0) | host-side only | loopback + `CITADEL_EXTRACTION_BIND`; keeps its `:?` guard |
| diffusers | `${CITADEL_DIFFUSERS_HOST_PORT:?…}:7860` (0.0.0.0) | host-side only | same |
| transcribe | `"8101:8101"` literal | `internal/jobs/transcribe_audio.go` `localhost:8101` | `${CITADEL_TRANSCRIBE_BIND:-127.0.0.1}:8101:8101` (port stays literal; `TranscribePort` is in `fixedComposeHostPorts`, unchanged) |
| lmstudio | `"1234:1234"` literal | `cmd/expose.go servicePorts` lists it (display only) | `${CITADEL_LMSTUDIO_BIND:-127.0.0.1}:1234:1234` |
| tei | already `127.0.0.1:8102:80` | gateway `/v1/embeddings` upstream | bind-var form for uniformity only (no behavior change) |
| sglang | already `127.0.0.1:30000:30000` | worker | bind-var form (Phase 1, with the five #1025 engines) |

After Phase 4 `nonLoopbackServiceMapAllowlist` is empty and
`TestServiceMapBindSweep` asserts it stays empty — a future `ServiceMap`
entry that publishes on all interfaces fails CI with no allowlist to reach
for.

**Adjacent finding, out of scope, file separately:**
`internal/services/native.go`'s `NativeServices["llamacpp"].StartArgs`
passes `--host 0.0.0.0` to a NATIVE (non-container) llama-server — a real
host bind on all interfaces, distinct from `llamacpp_inference.go`'s
`LLAMACPP_COMMAND` `--host 0.0.0.0`, which is container-internal and
harmless (the publish is what matters there). The native path is not covered
by anything in this design.

## 7. Phased implementation plan

Ordered so each phase is independently mergeable and the riskiest
production change (recreating running containers) lands with its own
opt-out.

**Phase 0 — parser, tests only (do first).** `services/portspec.go` +
switch both test parsers to it, no compose change. CI-only, zero runtime
risk; unblocks everything else.

**Phase 1 — the hatch + the drift fix.**
- Compose: bind-var form + restored `:?` guard for vllm/llamacpp/bonsai/
  unlimited-ocr/kokoro/omnivoice, bind-var form for sglang; regen
  `known_hashes.go`.
- Registry: `serviceBindEnv`, `BindEnvVarName`, `BindEnv`.
- Manifest: `Bind` on both structs; `internal/compose/bindpolicy.go`.
- Injection + warning at the two `composeEnv` owners; fold the two ad hoc
  env sites onto `ServiceHandler.composeEnv()`.
- **Drift self-heal:** `cmd/work.go`'s boot-time `startManagedServices`
  drives `startService` (`cmd/service.go`), whose pre-flight
  `<engine> inspect --format {{.State.Status}}` already returns early on a
  `running` container ("Container ... is already running"). That inspect
  is the exact point to also read the container's `HostIp` (via the
  generalized helper) and compare it with the resolved policy;
  `--force-recreate` on mismatch, logged, gated by the existing
  `CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE` opt-out (same policy, same
  operator expectation as #426's port-move recreate). The pre-flight is
  skipped under an active `composeProjectOverride()` (`--node-dir`), which
  is fine: `citadel work` refuses the override at boot anyway. This must NOT be
  bolted onto `composerefresh.Sweep`'s recreator: the sweep is
  version-gated and skips already-current files, so it structurally cannot
  see a node that refreshed the file on the last boot but kept the old
  container. Extend `PortRecreator` to also compare `HostIp` so a
  same-version future template change is covered too, but the per-boot
  pass is the one that heals today's fleet.
- Tests: §3.1's list; `TestBindEnv` (policy → env + warnings, loopback
  omitted, invalid value errors); a `startManagedServices` recreate-on-drift
  test against a fake inspect.

**Phase 2 — observability.** `RunningHostBindings`, `ServiceInfo.Bind`,
the status/services warning line, the doctor `ENGINE EXPOSURE` section
including the cold-path materialized-file parse. Tests: collector attaches
`Bind` from a fake inspect; doctor renders all three outcomes from a
hand-built report (the existing `doctor_test.go` pattern).

**Phase 3 — ollama.** `ServiceNetworks` + `EnsureNetwork` at both sides,
`ollama.yml` rewrite, `AppManifest.Networks`, `ollama-webui` URL +
install-time native resolution, allowlist entry removed, hashes regen.
Verify on node 1297 only with an isolated `CITADEL_NODE_DIR` (never the
real node dir — the #853/#856/#860 incident class), and note that `--node-dir`
as a flag is not visible to `internal/jobs` (only the env var is).

**Phase 4 — sweep.** extraction/diffusers/transcribe/lmstudio/tei to the
bind-var form; allowlist asserted empty; hashes regen. Cross-repo follow-up
issue for citadel-services modules (§5.2); separate issue for the native
llamacpp `--host 0.0.0.0` (§6).

## 8. Open questions for the maintainer

1. **Exit code:** should `citadel doctor` flip to non-zero on the DRIFT
   case (container on 0.0.0.0, manifest says loopback)? Recommendation: no
   in v1 — Phase 1's per-boot self-heal makes it transient, and `ok()`'s
   docker-only contract is deliberate. Intended `bind: all` should never
   flip it.
2. **Governance:** confirm `bind` stays local-manifest-only (not settable via
   `APPLY_DEVICE_CONFIG`/`MODULE_SET`/`SERVICE_START`). If the platform ever
   needs to widen an engine, the right primitive is probably the gateway's
   model routing, not this field.
3. **`citadel run --bind all|<ip>`:** worth a flag that persists the field
   via `findOrCreateManifest`? Cheap, but it is a second writer of the
   field. Default: defer; hand-editing `citadel.yaml` is the v1 UX.
4. **Native-ollama fallback for `ollama-webui`:** keep the
   `host.docker.internal` fallback (already broken unless
   `OLLAMA_HOST=0.0.0.0`), or drop it and document "ollama-webui requires
   the containerized ollama"? Recommendation: drop it and print a clear
   install-time error — a fallback that only works with a hidden
   prerequisite is the confidently-wrong-doc failure mode.
5. **Dual-stack:** is `0.0.0.0` (v4-only) acceptable for `all`, or should
   `all` produce TWO publish lines / an unbracketed no-IP form? The
   template can't express "no IP" via a default, so v4-only is what (a)
   gives for free; IPv6 LANs are the case that would need more.
6. **Docker Desktop:** whether a `127.0.0.1` host publish answers
   `host.docker.internal` on macOS/Windows is unverified from this repo. (B)
   sidesteps it for the app path; it only matters if question 4 keeps the
   fallback.
7. **citadel-services modules:** which live module composes (claudecode,
   hermes, others) reach ollama or any engine via `host.docker.internal`,
   and should the catalog schema grow a `networks:` declaration so
   `citadel module install` calls `EnsureNetwork` the same way?
