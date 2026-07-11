# Headless Claude Code agent-runtime (Citadel BYOC)

Phase-1 flagship runtime for the BYOC agent-hosting story
(aceteam-ai/citadel-cli#432, Child C of aceteam-ai/aceteam#4588). A Claude Code
CLI turn driven headlessly by a tiny HTTP wrapper, with the model pointed at the
AceTeam fabric proxy. A full turn runs on your own node with **no external
Anthropic API key** -- agent AND model on your metal, the differentiator Fly
cannot match.

## How it works

```
  platform  --POST /hooks/agent-->  wrapper (FastAPI)  --claude -p-->  Claude Code CLI
  (routing seam)   (fast 200 ack)    async turn         --Messages API-->  ANTHROPIC_BASE_URL
                                                                            (fabric proxy)
  platform  <--POST /api/instances/{id}/reply--  wrapper   (assistant text)
```

- Inbound `POST /hooks/agent` with `Authorization: Bearer hooks_{GATEWAY_KEY}`
  and `{"message","name"}`. The wrapper validates the token, **ACKs fast** with
  `200 {"delivered": true}`, and runs the turn on a background thread.
- The turn shells out to `claude -p "<message>" --output-format json
  --dangerously-skip-permissions`, capturing the assistant text from the JSON
  `result` field.
- On completion it POSTs `{"reply": "<text>"}` (or `{"error": "..."}` on
  failure) to `{PLATFORM_URL}/api/instances/{INSTANCE_ID}/reply` with
  `Authorization: Bearer {GATEWAY_KEY}` (the **raw** key, not `hooks_`-prefixed).
- `GET /health` backs the compose healthcheck and reports the proxy wiring.

The wrapper listens on the **fixed internal port 8787**. The compose file maps
the citadel-owned host port to it.

## Env it needs

| Env | Meaning |
|-----|---------|
| `ACETEAM_INSTANCE_ID` | instance id, used in the reply URL path |
| `ACETEAM_PLATFORM_URL` | base URL to POST replies to (e.g. `https://aceteam.ai`) |
| `ACETEAM_GATEWAY_KEY` | raw gateway key: inbound validated vs `hooks_`+it, outbound auth with it |
| `ANTHROPIC_BASE_URL` | fabric proxy base URL (Anthropic-compatible) |
| `ANTHROPIC_AUTH_TOKEN` | Bearer token for the proxy (sent as `Authorization: Bearer`) |
| `ANTHROPIC_MODEL` | model id served by the proxy |
| `ANTHROPIC_SMALL_FAST_MODEL` | optional haiku-tier model for Claude Code sub-tasks |
| `CITADEL_CLAUDECODE_HOST_PORT` | host port for the wrapper (standalone compose; citadel supplies it) |

The image bakes in `DISABLE_AUTOUPDATER=1` and `CLAUDE_CODE_ENABLE_TELEMETRY=0`
so the only external traffic is to `ANTHROPIC_BASE_URL` and the reply callback.

## State volume ("permanent home")

Claude Code state (config, sessions, projects) lives at
`/home/claude/.claude` inside the container (via `CLAUDE_CONFIG_DIR`) and is
bind-mounted from `~/citadel-cache/claudecode` on the node, so it accumulates
across restarts.

The container starts as root, its entrypoint chowns this mount to the non-root
`claude` user (uid 10001), then drops privileges via `gosu` -- so a fresh
host-owned mount "just works" with no manual pre-create. `claude` never runs as
root (which it refuses under `--dangerously-skip-permissions` anyway).

## Run it (standalone / live-proof)

```sh
CITADEL_CLAUDECODE_HOST_PORT=8204 \
ACETEAM_INSTANCE_ID=<id> \
ACETEAM_PLATFORM_URL=https://aceteam.ai \
ACETEAM_GATEWAY_KEY=<raw-key> \
ANTHROPIC_BASE_URL=<fabric-proxy-url> \
ANTHROPIC_AUTH_TOKEN=<proxy-token> \
ANTHROPIC_MODEL=<model-id> \
docker compose -f services/compose/claudecode.yml up
```

## Install as a module

Publish `service.yaml` + `compose.yml` from this directory as
`services/claudecode/{service.yaml,compose.yml}` in a catalog repo
(e.g. `aceteam-ai/citadel-services`). The module `compose.yml` publishes the
literal host port `8204` (the module path does not inject
`CITADEL_CLAUDECODE_HOST_PORT`).

## Build the image

```sh
docker build -t ghcr.io/aceteam-ai/claudecode-service:latest \
  services/claudecode-service
# pin the CLI: --build-arg CLAUDE_CODE_VERSION=<x.y.z>
```

## Local smoke test (no real infra)

```sh
python3 services/claudecode-service/smoke_test.py
```

Builds the image, starts a mock Anthropic proxy + mock reply receiver, runs a
turn, and asserts the fast ack, the Anthropic-shaped proxy request, and the
well-formed `{reply}` POST with raw-key auth. See the PR "Testing" section for
sample output.

## Verifying the chown+gosu path under rootless podman

The entrypoint (`entrypoint.sh`) starts as container-root, `chown`s the
bind-mounted state dir to the in-container `claude` user (uid 10001), then drops
privileges with `gosu`. This was validated under **docker**, where container-root
is real host root and the chown is unconditional. Citadel prefers **rootless
podman**, where the id-mapping differs and must be checked separately:

- Container-root (uid 0) maps to the **operator's host uid**, not real root.
- In-container uid 10001 maps to a **subuid** from the operator's
  `/etc/subuid` range (must span >= 10001; the podman default 65536 does).
- A host-owned bind-mount source appears inside the container owned by a
  mapped uid, so the `chown -R claude:claude` is still needed and must succeed
  under the userns (podman keeps `CAP_CHOWN` within the mapped range).

CI cannot exercise this: GitHub's `ubuntu-latest` runner has no rootless-podman
userns set up and this repo's constraint forbids `go test ./...`, so treat the
following as the **documented manual verification** (run once on any Linux host
with rootless podman, e.g. a citadel node):

```sh
# 0. Prereqs (rootless podman + a subuid range covering 10001):
podman info --format '{{.Host.Security.Rootless}}'   # must print: true
grep "^$(id -un):" /etc/subuid                        # count must be >= 10001

# 1. Build (or pull the published image) and prep a host-owned state dir:
podman build -t claudecode-service:podman-check services/claudecode-service
STATE="$(mktemp -d)"; ls -land "$STATE"               # owned by your host uid

# 2. Boot the entrypoint's fixup path only (override CMD to inspect ownership),
#    mounting the host dir on the state volume exactly as compose does:
podman run --rm \
  -v "$STATE":/home/claude/.claude:Z \
  -e CLAUDE_CONFIG_DIR=/home/claude/.claude \
  claudecode-service:podman-check \
  sh -c 'id && stat -c "%u:%U owns %n" /home/claude/.claude && touch /home/claude/.claude/probe && echo OK'

# Expect: the entrypoint chowns the dir, gosu drops to `claude` (uid=10001),
# and the `touch` succeeds (no EACCES) -> prints `OK`.

# 3. Confirm the write is visible on the host under the mapped subuid:
ls -lan "$STATE"/probe && rm -rf "$STATE"
```

Pass criteria: step 2 prints `uid=10001(claude) ... OK` (privilege drop worked
and the mounted state dir is writable), and step 3 shows the probe file on the
host owned by the mapped subuid (not your uid), confirming the userns chown took
effect. If the `touch` fails with `EACCES`, the operator's `/etc/subuid` range
does not cover 10001 -- widen it (`podman system migrate` after editing) and
re-run.
