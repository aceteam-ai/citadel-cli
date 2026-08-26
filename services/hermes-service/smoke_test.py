#!/usr/bin/env python3
"""Local docker smoke test for the headless Hermes Agent runtime (aceteam#8170).

Proves the AceTeam-side wire contract WITHOUT any real inference provider:

  1. Build the runtime image (docker build) from THIS directory's Dockerfile,
     on top of the real, published `nousresearch/hermes-agent` base image.
  2. Start a MOCK AceTeam reply receiver.
  3. Run the container with NO provider credential configured, POST a
     /hooks/agent turn, and assert:
       (a) fast 200 {"delivered": true} ack,
       (b) the mock receiver got a well-formed {"error": ...} POST carrying
           Authorization: Bearer <RAW gateway key> (not hooks_-prefixed) --
           Hermes's own "No inference provider configured" error, surfaced
           through the wrapper's terminal-failure path.
  4. Separately assert GET /health reports the wrapper's own liveness
     (instance id, gateway-key-configured, provider fields) without invoking
     the CLI.

This deliberately does NOT mock a fake inference provider and assert a
SUCCESSFUL turn: unlike claudecode's `claude -p` (which speaks the well-known
Anthropic Messages API over a base-URL override), Hermes's `openai-api`
provider was confirmed live (see README "Model / provider wiring") to speak
OpenAI's Responses API (`/v1/responses`), whose exact expected response shape
this repo has not pinned. A guessed mock response would risk a smoke test that
passes for the wrong reason. What this test DOES prove -- the fast-ack +
background-turn + authenticated-reply-POST plumbing -- is exactly the generic
runtime path this module reuses from claudecode, which is the part in scope
for this repo.

Run:  python3 services/hermes-service/smoke_test.py
Requires: docker, python3 (stdlib only). Pulls nousresearch/hermes-agent:latest
(~950MB) on first run if not already cached.
"""

import json
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

IMAGE = "citadel-hermes-service:smoke"
CONTAINER = "citadel-hermes-smoke"
GATEWAY_KEY = "smoke-gateway-key-456"
INSTANCE_ID = "inst_smoke_hermes_1"
HOST_PORT = 8298  # ephemeral host port for the wrapper during the test

SERVICE_DIR = Path(__file__).resolve().parent
# docker build context is this service dir (Dockerfile + wrapper.py + requirements).


observed = {
    "reply_body": None,  # parsed JSON of the reply POST
    "reply_auth": None,  # Authorization header on the reply POST
}
observed_lock = threading.Lock()


class MockReceiverHandler(BaseHTTPRequestHandler):
    """Mock AceTeam platform: answers POST /api/instances/{id}/reply."""

    def log_message(self, *args):  # silence default logging
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        if self.path == f"/api/instances/{INSTANCE_ID}/reply":
            with observed_lock:
                observed["reply_body"] = json.loads(body) if body else None
                observed["reply_auth"] = self.headers.get("Authorization")
            resp = json.dumps({"accepted": True}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(resp)))
            self.end_headers()
            self.wfile.write(resp)
            return
        self.send_response(404)
        self.end_headers()


def run(cmd, **kwargs):
    print(f"+ {' '.join(cmd)}", flush=True)
    return subprocess.run(cmd, **kwargs)


def http_get(url, timeout=5):
    with urllib.request.urlopen(url, timeout=timeout) as resp:
        return resp.getcode(), json.loads(resp.read())


def http_post_json(url, payload, headers=None, timeout=15):
    data = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.getcode(), json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read())


def wait_for_health(url, timeout_s=60):
    deadline = time.time() + timeout_s
    last_err = None
    while time.time() < deadline:
        try:
            code, body = http_get(url, timeout=3)
            if code == 200:
                return body
        except Exception as exc:  # noqa: BLE001
            last_err = exc
        time.sleep(1)
    raise TimeoutError(f"wrapper never became healthy: {last_err}")


def wait_for_reply(timeout_s=60):
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        with observed_lock:
            if observed["reply_body"] is not None:
                return observed["reply_body"], observed["reply_auth"]
        time.sleep(0.5)
    raise TimeoutError("mock receiver never got a reply POST")


def main() -> int:
    failures = []

    print("=== Building image ===", flush=True)
    build = run(
        ["docker", "build", "-t", IMAGE, str(SERVICE_DIR)],
        capture_output=True,
        text=True,
    )
    if build.returncode != 0:
        print(build.stdout[-4000:])
        print(build.stderr[-4000:])
        print("FAIL: docker build failed")
        return 1

    print("=== Starting mock AceTeam reply receiver ===", flush=True)
    receiver = ThreadingHTTPServer(("0.0.0.0", 0), MockReceiverHandler)
    receiver_port = receiver.server_address[1]
    receiver_thread = threading.Thread(target=receiver.serve_forever, daemon=True)
    receiver_thread.start()

    run(["docker", "rm", "-f", CONTAINER], capture_output=True)

    print("=== Starting hermes-service container ===", flush=True)
    run_result = run(
        [
            "docker",
            "run",
            "-d",
            "--name",
            CONTAINER,
            "--add-host=host.docker.internal:host-gateway",
            "-p",
            f"{HOST_PORT}:8787",
            "-e",
            f"ACETEAM_INSTANCE_ID={INSTANCE_ID}",
            "-e",
            f"ACETEAM_PLATFORM_URL=http://host.docker.internal:{receiver_port}",
            "-e",
            f"ACETEAM_GATEWAY_KEY={GATEWAY_KEY}",
            # Deliberately NO provider credential set -- see module docstring.
            IMAGE,
        ],
        capture_output=True,
        text=True,
    )
    if run_result.returncode != 0:
        print(run_result.stderr)
        print("FAIL: docker run failed")
        return 1

    try:
        print("=== Waiting for /health ===", flush=True)
        health = wait_for_health(f"http://localhost:{HOST_PORT}/health")
        print(f"health: {health}")
        if health.get("gateway_key_configured") is not True:
            failures.append("health: gateway_key_configured should be true")
        if health.get("instance_id") != INSTANCE_ID:
            failures.append("health: instance_id mismatch")
        if health.get("provider_keys_configured") != []:
            failures.append(
                "health: provider_keys_configured should be empty in this smoke test "
                f"(got {health.get('provider_keys_configured')!r})"
            )

        print("=== POST /hooks/agent (fast-ack check) ===", flush=True)
        t0 = time.monotonic()
        code, body = http_post_json(
            f"http://localhost:{HOST_PORT}/hooks/agent",
            {"message": "what is 2+2?", "name": "smoke-test"},
            headers={"Authorization": f"Bearer hooks_{GATEWAY_KEY}"},
        )
        ack_latency = time.monotonic() - t0
        print(f"ack: {code} {body} ({ack_latency:.2f}s)")
        if code != 200 or body.get("delivered") is not True:
            failures.append(f"expected fast 200 {{delivered:true}}, got {code} {body}")
        if ack_latency > 5:
            failures.append(f"ack took {ack_latency:.2f}s -- not a fast ack")

        print("=== Waiting for the reply/error POST to the mock receiver ===", flush=True)
        reply_body, reply_auth = wait_for_reply(timeout_s=30)
        print(f"reply_body={reply_body!r} reply_auth={reply_auth!r}")
        if reply_auth != f"Bearer {GATEWAY_KEY}":
            failures.append(
                f"reply POST used wrong auth: {reply_auth!r} "
                f"(want raw key 'Bearer {GATEWAY_KEY}', NOT hooks_-prefixed)"
            )
        if not isinstance(reply_body, dict) or "error" not in reply_body:
            failures.append(
                f"expected a terminal {{error: ...}} reply (no provider configured), "
                f"got {reply_body!r}"
            )
        elif "provider" not in reply_body["error"].lower() and "inference" not in reply_body["error"].lower():
            failures.append(
                f"error message doesn't look like Hermes's 'no provider configured' "
                f"error: {reply_body['error']!r}"
            )

        print("=== Auth rejection check (wrong bearer) ===", flush=True)
        code, body = http_post_json(
            f"http://localhost:{HOST_PORT}/hooks/agent",
            {"message": "hi", "name": "x"},
            headers={"Authorization": "Bearer wrong-token"},
        )
        if code != 401:
            failures.append(f"expected 401 for bad bearer, got {code} {body}")

    finally:
        print("=== Container logs (tail) ===", flush=True)
        logs = run(["docker", "logs", "--tail", "60", CONTAINER], capture_output=True, text=True)
        print(logs.stdout)
        print(logs.stderr)
        run(["docker", "rm", "-f", CONTAINER], capture_output=True)
        receiver.shutdown()

    if failures:
        print("\n=== FAILURES ===")
        for f in failures:
            print(f" - {f}")
        return 1

    print("\n=== PASS ===")
    print("Fast ack, authenticated failure-reply POST, and /health all verified.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
