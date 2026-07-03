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
