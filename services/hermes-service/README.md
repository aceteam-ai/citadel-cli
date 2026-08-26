# Headless Hermes Agent runtime (Citadel BYOC)

The second agent-runtime harness after claudecode (aceteam#8170,
citadel-cli#432), reusing the SAME generic runtime path claudecode proved out.
A [Hermes Agent](https://github.com/NousResearch/hermes-agent) (Nous
Research's open-source, self-hosted AI agent) turn driven headlessly by a tiny
HTTP wrapper.

## Image status (read this first)

- **Base image `nousresearch/hermes-agent` is REAL and actively published**:
  multi-arch (amd64/arm64), on Docker Hub, verified by pulling it directly
  (2026-08-25; digest `sha256:8c1cc8be...`, pushed the same day). This is NOT
  a guessed or invented reference.
- **The derived image `ghcr.io/aceteam-ai/hermes-service` is NOT YET BUILT OR
  PUBLISHED.** This PR adds the `Dockerfile` that builds it; a follow-up PR
  (mirroring how claudecode-service's image was published by #442, after
  #432 added its Dockerfile) needs to actually build and push it. `docker
  build` from this directory works locally today (verified) -- what's missing
  is CI wiring to publish the result.

## How it works

```
  platform  --POST /hooks/agent-->  wrapper (FastAPI)  --hermes chat -q-->  Hermes Agent CLI
  (routing seam)   (fast 200 ack)    async turn         -->  operator's inference provider
  platform  <--POST /api/instances/{id}/reply--  wrapper   (assistant text)
```

- Inbound `POST /hooks/agent` with `Authorization: Bearer hooks_{GATEWAY_KEY}`
  and `{"message","name"}`. The wrapper validates the token, **ACKs fast** with
  `200 {"delivered": true}`, and runs the turn on a background thread.
- The turn shells out to `hermes chat -q "<message>" -Q` (verified against
  `hermes_cli/_parser.py`'s `chat` subparser upstream: `-q/--query` is
  "Single query (non-interactive mode)"; `-Q/--quiet` is "Quiet mode for
  programmatic use: suppress banner, spinner, and tool previews. Only output
  the final response and session info."), capturing stdout.
- On completion it POSTs `{"reply": "<text>"}` (or `{"error": "..."}` on
  failure) to `{PLATFORM_URL}/api/instances/{INSTANCE_ID}/reply` with
  `Authorization: Bearer {GATEWAY_KEY}` (the **raw** key, not `hooks_`-prefixed) --
  **byte-identical wire contract to claudecode-service**, which is the whole
  point of reusing the generic runtime path.
- `GET /health` backs the compose healthcheck and reports the provider wiring
  (booleans/labels only, never secret values).

The wrapper listens on the **fixed internal port 8787** (same number
claudecode uses -- separate containers, no shared network namespace, so this
is not a real collision). The compose file maps the citadel-owned host port
(8205, `CITADEL_HERMES_HOST_PORT`) to it.

## Known limitation: quiet-mode output

Unlike `claude -p --output-format json`, Hermes's `chat -Q` has **no
structured/JSON output mode** (verified: no such flag exists on the `chat`
subparser). Quiet mode's own help text says it prints "the final response
**and session info**" -- observed live as a trailing `session_id: ...` line.
The wrapper returns `stdout.strip()` verbatim rather than guessing a regex to
strip that trailer: a wrong stripper would silently eat real reply content,
which is worse than an occasional stray trailer line reaching the user. If
this becomes a real UX problem, the fix belongs upstream (a `--json` flag
request to Nous Research) or in a follow-up that character-tests the exact
trailer format against a pinned Hermes version.

## Model / provider wiring (differs from claudecode -- read this)

claudecode points the `claude` CLI at an arbitrary Anthropic-compatible base
URL via `ANTHROPIC_BASE_URL` -- that's the official Anthropic SDK's own
override mechanism, so "point it at our fabric proxy" is a one-line env var.

**Hermes has no equivalent generic override for its default provider set.**
It supports many NAMED providers (OpenRouter, Fireworks, Google, GLM, Kimi,
Arcee, MiniMax, ...), each with its own `*_API_KEY` (+ some with a
`*_BASE_URL` override for that SPECIFIC provider's SDK, not a bring-your-own-
endpoint mechanism). This wrapper passes the whole environment through to the
`hermes` subprocess unmodified and does not validate which credential is set
-- **at least one of them must be**, or the turn fails with Hermes's own `No
inference provider configured` error (verified live).

**The `openai-api` provider IS a viable fabric-proxy path, confirmed but not
wired end-to-end:**
- `hermes_cli/providers.py` registers `openai-api` with
  `base_url_env_var="OPENAI_BASE_URL"` -- i.e. `OPENAI_API_KEY` +
  `OPENAI_BASE_URL` genuinely override where Hermes sends inference calls.
- Verified live: running the container with `OPENAI_API_KEY=dummy` +
  `OPENAI_BASE_URL=http://<mock>/v1` and `hermes chat -q ... --provider
  openai-api -m <model>` produced real outbound HTTP requests to the mock,
  with `Authorization: Bearer dummy`.
- **The catch**: those requests hit `POST /v1/responses` (OpenAI's newer
  Responses API), not the classic `/v1/chat/completions` shape claudecode's
  fabric proxy speaks for Anthropic. Whether the AceTeam fabric proxy can
  answer the Responses API (or whether `openai-api` has a chat-completions
  mode we didn't find) is unconfirmed and **out of scope for this repo** --
  it is aceteam-side wiring (see Follow-ups below).

Until that's wired, an operator runs this module with their OWN provider key
(`OPENROUTER_API_KEY` is the simplest single knob) rather than the
no-external-API-key "agent AND model on your own metal" story claudecode
tells today.

## Env it needs

| Env | Meaning |
|-----|---------|
| `ACETEAM_INSTANCE_ID` | instance id, used in the reply URL path |
| `ACETEAM_PLATFORM_URL` | base URL to POST replies to (e.g. `https://aceteam.ai`) |
| `ACETEAM_GATEWAY_KEY` | raw gateway key: inbound validated vs `hooks_`+it, outbound auth with it |
| `OPENROUTER_API_KEY` / `OPENAI_API_KEY` / `FIREWORKS_API_KEY` / `GOOGLE_API_KEY` / `GLM_API_KEY` / `KIMI_API_KEY` / `MINIMAX_API_KEY` | at least ONE inference provider credential (Hermes reads these directly) |
| `OPENAI_BASE_URL` | optional; only affects the `openai-api` provider (see above) |
| `HERMES_MODEL` | optional; mapped onto `hermes chat -m` |
| `HERMES_PROVIDER` | optional; mapped onto `hermes chat --provider` |
| `HERMES_TURN_TIMEOUT` | optional; per-turn timeout in seconds (default 600) |
| `CITADEL_HERMES_HOST_PORT` | host port for the wrapper (standalone compose; citadel supplies it) |

## State volume

Hermes state (config, sessions, memory, skills) lives at `/opt/data` inside
the container -- the upstream image's own declared `VOLUME`, bind-mounted from
`~/citadel-cache/hermes` on the node. **This path differs from claudecode's
`~/citadel-cache/claudecode` -> `/home/claude/.claude`** -- it follows Hermes's
own image contract rather than mirroring claudecode's path literally.

Unlike claudecode, **this wrapper ships no `entrypoint.sh`.** The upstream
Hermes image already does the root-to-non-root chown/UID-remap dance itself
(`docker/stage2-hook.sh`, run via s6-overlay's `cont-init.d`) before dropping
to the `hermes` user (uid 10000) and exec'ing our `CMD`. Verified live: the
wrapper process runs as `uid=10000(hermes)` and `command -v hermes` resolves
on `PATH` inside the running container.

## Run it (standalone / live-proof)

```sh
CITADEL_HERMES_HOST_PORT=8205 \
ACETEAM_INSTANCE_ID=<id> \
ACETEAM_PLATFORM_URL=https://aceteam.ai \
ACETEAM_GATEWAY_KEY=<raw-key> \
OPENROUTER_API_KEY=<your-key> \
docker compose -f services/compose/hermes.yml up
```

## Install as a module

Publish `service.yaml` + `compose.yml` from this directory as
`services/hermes/{service.yaml,compose.yml}` in a catalog repo (e.g.
`aceteam-ai/citadel-services`), plus a `registry.yaml` entry -- see the
citadel-services PR paired with this one.

## Build the image

```sh
docker build -t ghcr.io/aceteam-ai/hermes-service:latest services/hermes-service
```

No build args needed today (unlike claudecode's `CLAUDE_CODE_VERSION` pin) --
the upstream base image is pulled as `:latest`. Pinning both this image's tag
and the upstream base to a digest is a follow-up before production use (see
the Dockerfile header).

## AceTeam-side follow-ups (different repo, NOT done here)

- Wire `runtime_type: "hermes"` in `configGenerator.ts` / the `INSTANCE_TEMPLATES`
  entry aceteam's own `docs/engineering/aceclaws-architecture.md` already
  sketches (it names `ghcr.io/aceteam-ai/hermes-agent:latest` as an EXAMPLE --
  that reference predates this PR and should be corrected to
  `ghcr.io/aceteam-ai/hermes-service` once published).
- Publish the `ghcr.io/aceteam-ai/hermes-service` image via CI (mirrors how
  claudecode-service's image was published after citadel-cli#432/#442).
- Decide whether the fabric proxy should grow an OpenAI **Responses API**
  facade (`/v1/responses`) so Hermes's `openai-api` provider can point at it
  the way claudecode points `ANTHROPIC_BASE_URL` at the Anthropic-compatible
  facade -- this is what would unlock the "no external API key" story for
  Hermes. Confirmed technically viable (see "Model / provider wiring" above);
  not yet built.
