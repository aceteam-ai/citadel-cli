"""Headless Hermes Agent runtime wrapper for Citadel (aceteam#8170).

The second agent-runtime harness after claudecode (citadel-cli#432): this is a
tiny HTTP server that bridges AceTeam's chat routing seam (aceteam #4593) to a
headless Hermes Agent (https://github.com/NousResearch/hermes-agent) turn,
reusing the SAME generic runtime path claudecode proved out. Hermes itself
knows nothing about AceTeam -- this wrapper is the only harness-specific glue.

Contract (pinned by the routing-seam PR #4593 -- do NOT change it; this is
BYTE-IDENTICAL to claudecode-service/wrapper.py's contract, which is the whole
point of "reusing the generic runtime path"):

  Inbound (platform -> this container):
    POST /hooks/agent
    Authorization: Bearer hooks_{GATEWAY_KEY}
    { "message": "<user text>", "name": "<label>" }
  We validate the bearer token equals "hooks_" + the container's gateway key,
  then ACK FAST with 200 {"delivered": true} and process the turn on a
  background thread (the caller expects a quick delivery ack, not a blocking
  model turn).

  Outbound (this container -> platform), when the turn finishes:
    POST {PLATFORM_URL}/api/instances/{INSTANCE_ID}/reply
    Authorization: Bearer {GATEWAY_KEY}          # RAW key, NOT hooks_-prefixed
    { "reply": "<assistant text>" }              # on success
    { "error": "<user-facing message>" }         # on terminal failure
  Expect 200 {"accepted": true}.

Model/provider wiring is harness-specific and deliberately NOT the same as
claudecode's ANTHROPIC_* env (Hermes has no arbitrary-base-URL override for an
"Anthropic-compatible" proxy the way the `claude` CLI does). Hermes reads its
own provider credentials directly from the process environment (we pass the
whole environment through to the subprocess, unmodified) -- see the README for
the verified provider list. HERMES_MODEL / HERMES_PROVIDER, if set, are mapped
onto the CLI's `-m`/`--provider` flags.

Everything else is driven by env (no secrets baked into the image); see the
compose file services/compose/hermes.yml and the README in this directory.
"""

import json
import os
import subprocess
import sys
import threading
import urllib.error
import urllib.request

from fastapi import FastAPI, Header, HTTPException, Request

# --- Container env contract (set by the runtime provider; read here) ----------
# The instance id, used in the reply URL path.
INSTANCE_ID = os.environ.get("ACETEAM_INSTANCE_ID", "")
# Base URL to POST replies to (e.g. https://aceteam.ai). Trailing slash trimmed.
PLATFORM_URL = os.environ.get("ACETEAM_PLATFORM_URL", "").rstrip("/")
# Raw gateway key: inbound is validated against "hooks_"+key; outbound auths with key.
GATEWAY_KEY = os.environ.get("ACETEAM_GATEWAY_KEY", "")

# Model/provider selection, mapped onto `hermes chat`'s -m / --provider flags.
# Left unset by default: an operator with `hermes model` already configured
# (or a single provider key set) needs neither. We do NOT read or validate
# any specific provider API key here -- Hermes' own CLI does that, and it
# accepts many (OPENROUTER_API_KEY, OPENAI_API_KEY, FIREWORKS_API_KEY, ...).
# We only surface which knobs are SET (not their values) on /health.
HERMES_MODEL = os.environ.get("HERMES_MODEL", "")
HERMES_PROVIDER = os.environ.get("HERMES_PROVIDER", "")

# Fixed internal port the wrapper listens on inside the container. The compose
# file maps ${CITADEL_HERMES_HOST_PORT} on the host to this. Kept as an env so
# the Dockerfile CMD and this module agree on one value.
PORT = int(os.environ.get("PORT", "8787"))

# Hard ceiling on a single Hermes turn. A wedged turn posts {error} rather than
# hanging the instance forever. Override via HERMES_TURN_TIMEOUT. Mirrors
# claudecode's CLAUDECODE_TURN_TIMEOUT (same default).
TURN_TIMEOUT_SECONDS = int(os.environ.get("HERMES_TURN_TIMEOUT", "600"))

app = FastAPI(title="citadel-hermes-runtime")


def _expected_inbound_bearer() -> str:
    """The inbound Authorization value the platform must present."""
    return f"hooks_{GATEWAY_KEY}"


def _run_hermes_turn(message: str) -> str:
    """Drive one headless Hermes turn and return the assistant text.

    Raises RuntimeError on non-zero exit / timeout so the caller can post a
    terminal {error}.
    """
    # `hermes chat -q <message>` = single non-interactive query (verified
    # against hermes_cli/_parser.py's chat subparser: `-q/--query "Single
    # query (non-interactive mode)"`). `-Q/--quiet` is documented upstream as
    # "Quiet mode for programmatic use: suppress banner, spinner, and tool
    # previews. Only output the final response and session info." -- i.e.
    # there is NO `--output-format json` equivalent (verified: no such flag
    # exists on the chat subparser). Unlike claudecode's `-p --output-format
    # json`, stdout here is not a clean structured envelope; it may carry a
    # trailing "session_id: ..." line alongside the reply (observed live).
    # We deliberately do NOT attempt to strip a guessed trailer format here:
    # a wrong regex would silently eat real reply content, which is worse
    # than an occasional stray trailer line reaching the user. See the
    # README's "Known limitation: quiet-mode output" section.
    cmd = ["hermes", "chat", "-q", message, "-Q"]
    if HERMES_PROVIDER:
        cmd += ["--provider", HERMES_PROVIDER]
    if HERMES_MODEL:
        cmd += ["-m", HERMES_MODEL]

    try:
        proc = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=TURN_TIMEOUT_SECONDS,
            # Inherit the process env so the operator's provider credentials
            # (OPENROUTER_API_KEY, OPENAI_API_KEY, etc.) reach the CLI.
            env=os.environ.copy(),
        )
    except subprocess.TimeoutExpired:
        raise RuntimeError(f"Hermes turn timed out after {TURN_TIMEOUT_SECONDS}s")

    if proc.returncode != 0:
        # Surface the tail of stderr (Hermes prints user-facing errors to
        # stdout too, e.g. "No inference provider configured" -- include both
        # so the terminal error is actionable) but keep it short: it goes to
        # a user-facing chat.
        err = (proc.stderr or "").strip() or (proc.stdout or "").strip()
        tail = err[-800:] if err else "(no output)"
        raise RuntimeError(f"Hermes exited {proc.returncode}: {tail}")

    stdout = (proc.stdout or "").strip()
    if not stdout:
        raise RuntimeError("Hermes produced no output")

    # No JSON envelope to parse (see the quiet-mode caveat above) -- return
    # stdout verbatim.
    return stdout


def _post_reply(body: dict) -> None:
    """POST the outbound reply/error to the platform callback (best-effort)."""
    if not PLATFORM_URL or not INSTANCE_ID:
        print(
            "hermes: PLATFORM_URL/INSTANCE_ID unset; cannot post reply",
            file=sys.stderr,
            flush=True,
        )
        return
    url = f"{PLATFORM_URL}/api/instances/{INSTANCE_ID}/reply"
    data = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    # Outbound auth is the RAW gateway key -- NOT the hooks_-prefixed inbound token.
    req.add_header("Authorization", f"Bearer {GATEWAY_KEY}")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            resp.read()
    except urllib.error.HTTPError as exc:
        print(
            f"hermes: reply POST to {url} failed: {exc.code} {exc.reason}",
            file=sys.stderr,
            flush=True,
        )
    except Exception as exc:  # network error, DNS, etc.
        print(f"hermes: reply POST to {url} failed: {exc}", file=sys.stderr, flush=True)


def _process_turn(message: str) -> None:
    """Background worker: run the turn, then post reply or error."""
    try:
        reply = _run_hermes_turn(message)
        _post_reply({"reply": reply})
    except Exception as exc:  # any terminal failure -> user-facing {error}
        _post_reply({"error": str(exc)})


@app.get("/health")
def health():
    """Liveness probe for the compose healthcheck.

    Answers immediately and reports the provider wiring an operator can
    confirm without shell access (booleans/labels only -- never the secret
    values). Does not invoke the CLI.
    """
    provider_keys_set = sorted(
        name
        for name in (
            "OPENROUTER_API_KEY",
            "OPENAI_API_KEY",
            "FIREWORKS_API_KEY",
            "GOOGLE_API_KEY",
            "GEMINI_API_KEY",
            "GLM_API_KEY",
            "KIMI_API_KEY",
            "MINIMAX_API_KEY",
        )
        if os.environ.get(name)
    )
    return {
        "status": "ok",
        "instance_id": INSTANCE_ID or None,
        "platform_url": PLATFORM_URL or None,
        "provider": HERMES_PROVIDER or None,
        "model": HERMES_MODEL or None,
        "provider_keys_configured": provider_keys_set,
        "gateway_key_configured": bool(GATEWAY_KEY),
    }


@app.post("/hooks/agent")
async def hooks_agent(request: Request, authorization: str = Header(default="")):
    """Inbound turn from the platform routing seam.

    Validates the bearer token, ACKs fast, and processes the turn asynchronously.
    """
    if not GATEWAY_KEY:
        # Misconfigured container -- fail loud rather than accept unauthenticated turns.
        raise HTTPException(status_code=503, detail="gateway key not configured")

    expected = f"Bearer {_expected_inbound_bearer()}"
    if authorization != expected:
        raise HTTPException(status_code=401, detail="invalid bearer token")

    try:
        body = await request.json()
    except Exception:
        raise HTTPException(status_code=400, detail="invalid JSON body")

    message = body.get("message") if isinstance(body, dict) else None
    if not isinstance(message, str) or not message:
        raise HTTPException(status_code=400, detail="missing 'message'")

    # ACK FAST: hand the (blocking) model turn to a background thread so the
    # inbound request returns a delivery ack immediately. daemon=True so a
    # container stop never hangs on an in-flight turn.
    threading.Thread(target=_process_turn, args=(message,), daemon=True).start()

    return {"delivered": True}


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=PORT)
