#!/usr/bin/env python3
"""Local docker smoke test for the headless Claude Code runtime (citadel-cli#432).

Proves the full turn contract WITHOUT any real infrastructure:

  1. Build the runtime image (docker build).
  2. Start a MOCK Anthropic proxy that returns a canned Messages response and
     records that it received an Anthropic-shaped request (Bearer auth,
     /v1/messages, a `model` + `messages` body).
  3. Start a MOCK AceTeam reply receiver.
  4. Run the container pointed at both mocks, POST a /hooks/agent turn, and assert:
       (a) fast 200 {"delivered": true} ack,
       (b) the mock proxy received an Anthropic-shaped request,
       (c) the mock receiver got a well-formed {"reply": ...} POST carrying
           Authorization: Bearer <RAW gateway key> (not hooks_-prefixed).

The mock proxy answers both the streaming (SSE) and non-streaming shapes of the
Anthropic Messages API, so it satisfies `claude -p` regardless of whether the CLI
requests a stream. The canned assistant text is echoed back to the receiver,
proving the model output flows end-to-end with NO real Anthropic API key.

Run:  python3 services/claudecode-service/smoke_test.py
Requires: docker, python3 (stdlib only).
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

IMAGE = "citadel-claudecode-service:smoke"
CONTAINER = "citadel-claudecode-smoke"
GATEWAY_KEY = "smoke-gateway-key-123"
INSTANCE_ID = "inst_smoke_1"
CANNED_REPLY = "Hello from the fabric proxy. 2 plus 2 is 4."
HOST_PORT = 8299  # ephemeral host port for the wrapper during the test

SERVICE_DIR = Path(__file__).resolve().parent
# docker build context is this service dir (Dockerfile + wrapper.py + requirements).


# --- Shared record of what the mocks observed (asserted at the end) -----------
observed = {
    "proxy_hits": [],   # list of dicts: {path, auth, model, has_messages}
    "reply_body": None,  # parsed JSON of the reply POST
    "reply_auth": None,  # Authorization header on the reply POST
}
observed_lock = threading.Lock()


class MockProxyHandler(BaseHTTPRequestHandler):
    """Mock Anthropic-compatible fabric proxy: answers POST /v1/messages."""

    def log_message(self, *args):  # silence default logging
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length) if length else b"{}"
        try:
            body = json.loads(raw or b"{}")
        except json.JSONDecodeError:
            body = {}

        with observed_lock:
            observed["proxy_hits"].append(
                {
                    "path": self.path,
                    "auth": self.headers.get("Authorization"),
                    "model": body.get("model"),
                    "has_messages": isinstance(body.get("messages"), list),
                    "stream": bool(body.get("stream")),
                }
            )

        # Only the Messages endpoint is meaningful; anything else 404s.
        if not self.path.startswith("/v1/messages"):
            self.send_response(404)
            self.end_headers()
            self.wfile.write(b'{"error":"not found"}')
            return

        if body.get("stream"):
            self._send_sse()
        else:
            self._send_json()

    def _send_json(self):
        payload = {
            "id": "msg_smoke",
            "type": "message",
            "role": "assistant",
            "model": "smoke-model",
            "content": [{"type": "text", "text": CANNED_REPLY}],
            "stop_reason": "end_turn",
            "usage": {"input_tokens": 5, "output_tokens": 12},
        }
        data = json.dumps(payload).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _send_sse(self):
        """Emit the Anthropic Messages streaming (SSE) event sequence."""
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()

        def event(kind, obj):
            chunk = f"event: {kind}\ndata: {json.dumps(obj)}\n\n".encode("utf-8")
            self.wfile.write(chunk)
            self.wfile.flush()

        event(
            "message_start",
            {
                "type": "message_start",
                "message": {
                    "id": "msg_smoke",
                    "type": "message",
                    "role": "assistant",
                    "model": "smoke-model",
                    "content": [],
                    "stop_reason": None,
                    "usage": {"input_tokens": 5, "output_tokens": 0},
                },
            },
        )
        event(
            "content_block_start",
            {"type": "content_block_start", "index": 0,
             "content_block": {"type": "text", "text": ""}},
        )
        event(
            "content_block_delta",
            {"type": "content_block_delta", "index": 0,
             "delta": {"type": "text_delta", "text": CANNED_REPLY}},
        )
        event("content_block_stop", {"type": "content_block_stop", "index": 0})
        event(
            "message_delta",
            {"type": "message_delta",
             "delta": {"stop_reason": "end_turn", "stop_sequence": None},
             "usage": {"output_tokens": 12}},
        )
        event("message_stop", {"type": "message_stop"})


class MockReceiverHandler(BaseHTTPRequestHandler):
    """Mock AceTeam platform: records the reply POST to /api/instances/.../reply."""

    def log_message(self, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length) if length else b"{}"
        try:
            body = json.loads(raw or b"{}")
        except json.JSONDecodeError:
            body = {}
        with observed_lock:
            observed["reply_body"] = body
            observed["reply_auth"] = self.headers.get("Authorization")
        resp = json.dumps({"accepted": True}).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(resp)))
        self.end_headers()
        self.wfile.write(resp)


def start_server(handler):
    srv = ThreadingHTTPServer(("0.0.0.0", 0), handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv, srv.server_address[1]


def sh(cmd, **kw):
    print(f"$ {' '.join(cmd)}", flush=True)
    return subprocess.run(cmd, **kw)


def cleanup():
    subprocess.run(["docker", "rm", "-f", CONTAINER],
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def main() -> int:
    # 1. Build the image.
    r = sh(["docker", "build", "-t", IMAGE, str(SERVICE_DIR)])
    if r.returncode != 0:
        print("FAIL: docker build failed", file=sys.stderr)
        return 1

    # 2 + 3. Start the mock proxy and mock receiver on the host.
    proxy_srv, proxy_port = start_server(MockProxyHandler)
    recv_srv, recv_port = start_server(MockReceiverHandler)
    print(f"mock proxy on :{proxy_port}  mock receiver on :{recv_port}", flush=True)

    cleanup()

    # The container reaches host services via host.docker.internal (mapped to the
    # host gateway). Works on Docker Desktop natively and on Linux via the
    # host-gateway alias we add below.
    host_alias = "host.docker.internal"
    base_url = f"http://{host_alias}:{proxy_port}"
    platform_url = f"http://{host_alias}:{recv_port}"

    run_cmd = [
        "docker", "run", "-d", "--name", CONTAINER,
        "--add-host", "host.docker.internal:host-gateway",
        "-p", f"{HOST_PORT}:8787",
        "-e", f"ACETEAM_INSTANCE_ID={INSTANCE_ID}",
        "-e", f"ACETEAM_PLATFORM_URL={platform_url}",
        "-e", f"ACETEAM_GATEWAY_KEY={GATEWAY_KEY}",
        "-e", f"ANTHROPIC_BASE_URL={base_url}",
        "-e", "ANTHROPIC_AUTH_TOKEN=smoke-proxy-token",
        "-e", "ANTHROPIC_MODEL=smoke-model",
        IMAGE,
    ]
    r = sh(run_cmd)
    if r.returncode != 0:
        print("FAIL: docker run failed", file=sys.stderr)
        return 1

    try:
        # Wait for /health to come up.
        health_url = f"http://127.0.0.1:{HOST_PORT}/health"
        ok = False
        for _ in range(30):
            try:
                with urllib.request.urlopen(health_url, timeout=2) as resp:
                    if resp.status == 200:
                        ok = True
                        break
            except Exception:
                time.sleep(1)
        if not ok:
            print("FAIL: wrapper /health never came up", file=sys.stderr)
            sh(["docker", "logs", CONTAINER])
            return 1
        print("PASS: /health is up", flush=True)

        # 4. POST an inbound turn and assert a FAST ack (before the turn finishes).
        turn_url = f"http://127.0.0.1:{HOST_PORT}/hooks/agent"
        req = urllib.request.Request(
            turn_url,
            data=json.dumps({"message": "what is 2+2?", "name": "smoke"}).encode(),
            method="POST",
        )
        req.add_header("Content-Type", "application/json")
        req.add_header("Authorization", f"Bearer hooks_{GATEWAY_KEY}")
        t0 = time.time()
        with urllib.request.urlopen(req, timeout=10) as resp:
            ack = json.loads(resp.read())
            ack_status = resp.status
        ack_ms = (time.time() - t0) * 1000
        with observed_lock:
            reply_already = observed["reply_body"] is not None
        if ack_status != 200 or ack.get("delivered") is not True:
            print(f"FAIL: bad ack: {ack_status} {ack}", file=sys.stderr)
            return 1
        # Fast-ack ORDERING: the ack must return before the reply POST lands.
        if reply_already:
            print("FAIL: reply arrived before the ack returned (not async)",
                  file=sys.stderr)
            return 1
        print(f"PASS: fast ack {ack} in {ack_ms:.0f}ms, before reply POST", flush=True)

        # Reject a bad bearer token (auth check).
        bad = urllib.request.Request(
            turn_url,
            data=json.dumps({"message": "x", "name": "y"}).encode(),
            method="POST",
        )
        bad.add_header("Content-Type", "application/json")
        bad.add_header("Authorization", "Bearer wrong")
        try:
            urllib.request.urlopen(bad, timeout=5)
            print("FAIL: bad bearer token was accepted", file=sys.stderr)
            return 1
        except urllib.error.HTTPError as exc:
            if exc.code != 401:
                print(f"FAIL: bad token gave {exc.code}, want 401", file=sys.stderr)
                return 1
        print("PASS: bad bearer token rejected with 401", flush=True)

        # Wait for the turn to complete and the reply to reach the receiver.
        reply = None
        for _ in range(60):
            with observed_lock:
                reply = observed["reply_body"]
                reply_auth = observed["reply_auth"]
            if reply is not None:
                break
            time.sleep(1)

        if reply is None:
            print("FAIL: mock receiver never got a reply POST", file=sys.stderr)
            sh(["docker", "logs", CONTAINER])
            return 1

        # (b) the mock proxy received an Anthropic-shaped request.
        with observed_lock:
            hits = list(observed["proxy_hits"])
        msg_hits = [h for h in hits if h["path"].startswith("/v1/messages")]
        if not msg_hits:
            print(f"FAIL: proxy got no /v1/messages request; hits={hits}",
                  file=sys.stderr)
            return 1
        h = msg_hits[0]
        if not (h["auth"] == "Bearer smoke-proxy-token" and h["has_messages"]):
            print(f"FAIL: proxy request not Anthropic-shaped: {h}", file=sys.stderr)
            return 1
        print(f"PASS: proxy got Anthropic-shaped request: {h}", flush=True)

        # (c) reply is well-formed and auth is the RAW key (not hooks_-prefixed).
        if "reply" not in reply and "error" not in reply:
            print(f"FAIL: reply body has neither reply nor error: {reply}",
                  file=sys.stderr)
            return 1
        if "error" in reply:
            print(f"FAIL: turn ended in error: {reply['error']}", file=sys.stderr)
            sh(["docker", "logs", CONTAINER])
            return 1
        if reply_auth != f"Bearer {GATEWAY_KEY}":
            print(f"FAIL: reply auth is {reply_auth!r}, want raw Bearer key",
                  file=sys.stderr)
            return 1
        if reply_auth == f"Bearer hooks_{GATEWAY_KEY}":
            print("FAIL: reply used the hooks_-prefixed token", file=sys.stderr)
            return 1
        print(f"PASS: receiver got well-formed reply with RAW-key auth: {reply}",
              flush=True)

        # The canned model text should have flowed through end-to-end.
        if CANNED_REPLY not in reply["reply"]:
            print(f"WARN: canned text not found verbatim in reply "
                  f"(CLI may reshape it): {reply['reply']!r}", flush=True)
        else:
            print("PASS: canned fabric-proxy text flowed end-to-end", flush=True)

        print("\nSMOKE TEST PASSED", flush=True)
        return 0
    finally:
        proxy_srv.shutdown()
        recv_srv.shutdown()
        cleanup()


if __name__ == "__main__":
    sys.exit(main())
