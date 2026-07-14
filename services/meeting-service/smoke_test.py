#!/usr/bin/env python3
"""End-to-end acceptance smoke test for the Citadel meeting media stack
(aceteam-ai/citadel-cli#514).

This is the reproducible version of the vertical-slice proof: on a box with NO
host Chromium/PulseAudio/Xvfb, it builds the image, runs the container, and
asserts

  1. /health (the canary-tone probe) reports healthy -- audio capture works;
  2. POST /sessions launches Chromium with a live CDP port reachable through the
     published (socat-forwarded) port;
  3. driving that CDP to play a WebAudio tone, then recording via meetingd,
     produces a NON-SILENT WAV under /workspace.

Step 3 is the exact thing the live meeting crash failed to do (it recorded
nothing). It needs docker + the `websocket-client` package, so it is a documented
MANUAL check, not a CI gate.

Run:
    pip install websocket-client
    python3 services/meeting-service/smoke_test.py
"""

from __future__ import annotations

import json
import math
import os
import subprocess
import sys
import tempfile
import time
import urllib.request
import wave

HERE = os.path.dirname(os.path.abspath(__file__))
IMAGE = "meeting-service:smoke"
NAME = "citadel-meeting-smoke"
HOST_MEETINGD = 8207
HOST_CDP = 8208
FLOOR_DBFS = -50.0


def sh(*args: str, **kw) -> subprocess.CompletedProcess:
    return subprocess.run(args, check=True, **kw)


def http(method: str, url: str, body: dict | None = None) -> tuple[int, dict]:
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        url, data=data, method=method, headers={"content-type": "application/json"}
    )
    with urllib.request.urlopen(req, timeout=15) as r:
        raw = r.read().decode()
        return r.status, (json.loads(raw) if raw.strip() else {})


def wait_healthy(timeout: int = 60) -> dict:
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            st, body = http("GET", f"http://127.0.0.1:{HOST_MEETINGD}/health")
            if st == 200:
                return body
            last = body
        except Exception as exc:  # noqa: BLE001
            last = {"error": str(exc)}
        time.sleep(2)
    raise TimeoutError(f"never became healthy: {last}")


def wav_rms_dbfs(path: str) -> float:
    with wave.open(path, "rb") as w:
        frames = w.readframes(w.getnframes())
    import array

    a = array.array("h")
    a.frombytes(frames)
    if not a:
        return -120.0
    rms = math.sqrt(sum(s * s for s in a) / len(a))
    return 20 * math.log10(rms / 32768.0) if rms > 0 else -120.0


def main() -> int:
    from websocket import create_connection

    print("[1/5] build image ...")
    sh("docker", "build", "-t", IMAGE, HERE)

    workdir = tempfile.mkdtemp(prefix="meeting-smoke-")
    os.makedirs(os.path.join(workdir, "profile"), exist_ok=True)
    os.makedirs(os.path.join(workdir, "workspace"), exist_ok=True)
    subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)
    print("[2/5] run container (no host chrome/pulse/xvfb) ...")
    sh(
        "docker", "run", "-d", "--name", NAME, "--shm-size=1g",
        "-p", f"127.0.0.1:{HOST_MEETINGD}:8102",
        "-p", f"127.0.0.1:{HOST_CDP}:9223",
        "-v", f"{workdir}/profile:/profile",
        "-v", f"{workdir}/workspace:/workspace",
        "-e", "CITADEL_WORKSPACE=/workspace",
        IMAGE,
    )
    try:
        print("[3/5] wait for canary-healthy ...")
        h = wait_healthy()
        assert h["canary"]["ok"], h
        print("      healthy:", h["canary"])

        print("[4/5] start session + drive CDP WebAudio tone ...")
        _, sess = http("POST", f"http://127.0.0.1:{HOST_MEETINGD}/sessions",
                       {"session_id": "smoke"})
        time.sleep(6)
        _, tgt = http("PUT", f"http://127.0.0.1:{HOST_CDP}/json/new?about:blank")
        ws = create_connection(tgt["webSocketDebuggerUrl"], timeout=10,
                               suppress_origin=True)
        osc = (
            "(()=>{const c=new AudioContext();const o=c.createOscillator();"
            "const g=c.createGain();o.type='sine';o.frequency.value=440;"
            "g.gain.value=0.5;o.connect(g).connect(c.destination);o.start();"
            "return c.state;})()"
        )
        ws.send(json.dumps({"id": 1, "method": "Runtime.evaluate",
                            "params": {"expression": osc, "returnByValue": True}}))
        while True:
            m = json.loads(ws.recv())
            if m.get("id") == 1:
                print("      AudioContext:", m["result"]["result"]["value"])
                break

        http("POST", f"http://127.0.0.1:{HOST_MEETINGD}/sessions/smoke/record",
             {"out": "meetings/smoke.wav"})
        time.sleep(3.5)
        http("POST", f"http://127.0.0.1:{HOST_MEETINGD}/sessions/smoke/record/stop")
        ws.close()

        print("[5/5] assert non-silent WAV ...")
        wav = os.path.join(workdir, "workspace", "meetings", "smoke.wav")
        db = wav_rms_dbfs(wav)
        print(f"      WAV RMS = {db:.2f} dBFS (floor {FLOOR_DBFS})")
        if db <= FLOOR_DBFS:
            print("FAIL: recording is silent")
            return 1
        print("PASS: end-to-end non-silent capture on a bare box")
        return 0
    finally:
        subprocess.run(["docker", "logs", NAME], capture_output=True)
        subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)


if __name__ == "__main__":
    sys.exit(main())
