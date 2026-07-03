"""Headless Claude Code agent-runtime wrapper for Citadel (aceteam-ai/citadel-cli#432).

This is the phase-1 flagship BYOC agent runtime: a tiny HTTP server that bridges
AceTeam's chat routing seam (aceteam #4593) to a headless Claude Code CLI turn.
The model is pointed at the AceTeam fabric proxy (an Anthropic-compatible
endpoint) via the ANTHROPIC_* env, so a full turn runs with NO external Anthropic
API key -- the "agent AND model on your own metal" story that Fly cannot match.

Contract (pinned by the routing-seam PR #4593 -- do NOT change it):

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

Everything is driven by env (no secrets baked into the image); see the compose
file services/compose/claudecode.yml and the README in this directory.
"""

import json
import os
import subprocess
import sys
import threading
import time
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

# Model env is consumed by the `claude` CLI directly (it speaks the Anthropic
# Messages API): ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN / ANTHROPIC_MODEL /
# ANTHROPIC_SMALL_FAST_MODEL. We do not read them here -- we only pass the
# process environment through to the subprocess -- but we surface them on /health
# so an operator can confirm the fabric-proxy wiring without shell access.
ANTHROPIC_BASE_URL = os.environ.get("ANTHROPIC_BASE_URL", "")
ANTHROPIC_MODEL = os.environ.get("ANTHROPIC_MODEL", "")

# Fixed internal port the wrapper listens on inside the container. The compose
# file maps ${CITADEL_CLAUDECODE_HOST_PORT} on the host to this. Kept as an env
# so the Dockerfile CMD and this module agree on one value.
PORT = int(os.environ.get("PORT", "8787"))

# Hard ceiling on a single Claude Code turn. A wedged turn posts {error} rather
# than hanging the instance forever. Override via CLAUDECODE_TURN_TIMEOUT.
TURN_TIMEOUT_SECONDS = int(os.environ.get("CLAUDECODE_TURN_TIMEOUT", "600"))

# Where Claude Code keeps its state (config, sessions, projects). Mounted as a
# node-disk volume by the compose file so it accumulates across restarts.
CLAUDE_STATE_DIR = os.environ.get("CLAUDE_CONFIG_DIR", "/home/claude/.claude")

app = FastAPI(title="citadel-claudecode-runtime")


def _expected_inbound_bearer() -> str:
    """The inbound Authorization value the platform must present."""
    return f"hooks_{GATEWAY_KEY}"


def _run_claude_turn(message: str) -> str:
    """Drive one headless Claude Code turn and return the assistant text.

    Raises RuntimeError on non-zero exit / timeout / unparseable output so the
    caller can post a terminal {error}.
    """
    # `claude -p` = non-interactive "print" mode: run one turn and exit.
    # --output-format json gives a single, stable JSON envelope whose "result"
    # field is the final assistant text (far more robust to parse than scraping
    # free-form stdout). We pass the user message as the positional prompt.
    #
    # --dangerously-skip-permissions: the container IS the sandbox (isolated,
    # non-root, its own state volume), and there is no human to answer an
    # interactive tool-approval prompt in a headless turn. Without it a turn
    # that reaches for bash/edit would block forever.
    cmd = [
        "claude",
        "-p",
        message,
        "--output-format",
        "json",
        "--dangerously-skip-permissions",
    ]

    try:
        proc = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=TURN_TIMEOUT_SECONDS,
            # Inherit the process env so ANTHROPIC_* reaches the CLI. Force the
            # state dir so it matches the mounted volume regardless of $HOME.
            env={**os.environ, "CLAUDE_CONFIG_DIR": CLAUDE_STATE_DIR},
        )
    except subprocess.TimeoutExpired:
        raise RuntimeError(
            f"Claude Code turn timed out after {TURN_TIMEOUT_SECONDS}s"
        )

    if proc.returncode != 0:
        # Surface the tail of stderr so the terminal error is actionable, but
        # keep it short -- it goes to a user-facing chat.
        err = (proc.stderr or proc.stdout or "").strip()
        tail = err[-800:] if err else "(no output)"
        raise RuntimeError(f"Claude Code exited {proc.returncode}: {tail}")

    stdout = (proc.stdout or "").strip()
    if not stdout:
        raise RuntimeError("Claude Code produced no output")

    # Parse the JSON envelope. The `-p --output-format json` envelope carries the
    # assistant text under "result"; be liberal about a couple of alternative
    # shapes so a CLI-version change in the field name doesn't break the turn.
    try:
        payload = json.loads(stdout)
    except json.JSONDecodeError:
        # Not JSON (e.g. text output-format) -- treat the raw stdout as the reply.
        return stdout

    if isinstance(payload, dict):
        if payload.get("is_error"):
            raise RuntimeError(
                f"Claude Code reported an error: {payload.get('result') or payload}"
            )
        for key in ("result", "text", "response"):
            val = payload.get(key)
            if isinstance(val, str) and val:
                return val
    # Fell through -- return the serialized payload rather than losing the turn.
    return stdout


def _post_reply(body: dict) -> None:
    """POST the outbound reply/error to the platform callback (best-effort)."""
    if not PLATFORM_URL or not INSTANCE_ID:
        print(
            "claudecode: PLATFORM_URL/INSTANCE_ID unset; cannot post reply",
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
            f"claudecode: reply POST to {url} failed: {exc.code} {exc.reason}",
            file=sys.stderr,
            flush=True,
        )
    except Exception as exc:  # network error, DNS, etc.
        print(f"claudecode: reply POST to {url} failed: {exc}", file=sys.stderr, flush=True)


def _process_turn(message: str) -> None:
    """Background worker: run the turn, then post reply or error."""
    try:
        reply = _run_claude_turn(message)
        _post_reply({"reply": reply})
    except Exception as exc:  # any terminal failure -> user-facing {error}
        _post_reply({"error": str(exc)})


@app.get("/health")
def health():
    """Liveness probe for the compose healthcheck.

    Answers immediately and reports the fabric-proxy wiring so an operator can
    confirm the model path without shell access. Does not invoke the CLI.
    """
    return {
        "status": "ok",
        "instance_id": INSTANCE_ID or None,
        "platform_url": PLATFORM_URL or None,
        "anthropic_base_url": ANTHROPIC_BASE_URL or None,
        "model": ANTHROPIC_MODEL or None,
        "gateway_key_configured": bool(GATEWAY_KEY),
        "state_dir": CLAUDE_STATE_DIR,
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
