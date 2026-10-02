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
  4. (bot -> room SPEAKING path, aceteam#7079) the tab's getUserMedia sees the
     virtual mic (citadel_virtmic) as an audioinput, and audio POSTed to meetingd's
     /mic/play is HEARD by the tab -- the live-validation of the mic topology.

Step 3 is the exact thing the live meeting crash failed to do (it recorded
nothing). Step 4 is the honest live proof of the speaking path (item that cannot
be unit-tested without pulse + a browser). It needs docker + the
`websocket-client` package, so it is a documented MANUAL check, not a CI gate.

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


def write_tone_wav(path: str, seconds: float = 1.5, freq: int = 440, rate: int = 48000) -> None:
    """Write a half-scale mono sine WAV (no ffmpeg dependency on the host)."""
    import array

    os.makedirs(os.path.dirname(path), exist_ok=True)
    n = int(seconds * rate)
    samples = array.array(
        "h", (int(16000 * math.sin(2 * math.pi * freq * i / rate)) for i in range(n))
    )
    with wave.open(path, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(samples.tobytes())


def cdp_eval(ws, cid: int, expression: str, await_promise: bool = False):
    """Run one Runtime.evaluate over an open CDP page ws and return the by-value
    result, draining unrelated events until the matching id comes back."""
    ws.send(
        json.dumps(
            {
                "id": cid,
                "method": "Runtime.evaluate",
                "params": {
                    "expression": expression,
                    "returnByValue": True,
                    "awaitPromise": await_promise,
                },
            }
        )
    )
    while True:
        m = json.loads(ws.recv())
        if m.get("id") == cid:
            return m.get("result", {}).get("result", {}).get("value")


def mic_check(ws, workdir: str) -> None:
    """Step 4: prove the SPEAKING path end to end. Grant mic permission, confirm the
    virtual mic enumerates as an audioinput, then start a getUserMedia analyser,
    POST a tone to /mic/play, and assert the tab HEARD non-silence."""
    # /health must now report the virtual mic present.
    _, h = http("GET", f"http://127.0.0.1:{HOST_MEETINGD}/health")
    assert h.get("virtual_mic", {}).get("present") is True, f"virtual_mic not present: {h}"
    print("      /health virtual_mic:", h["virtual_mic"])

    # Grant mic permission so getUserMedia resolves and device labels populate.
    ws.send(json.dumps({"id": 90, "method": "Browser.grantPermissions",
                        "params": {"permissions": ["audioCapture"]}}))
    while True:
        if json.loads(ws.recv()).get("id") == 90:
            break

    inputs = cdp_eval(
        ws, 91,
        "navigator.mediaDevices.enumerateDevices()."
        "then(ds=>ds.filter(d=>d.kind==='audioinput').map(d=>d.label||d.deviceId))",
        await_promise=True,
    )
    print("      audioinput devices:", inputs)
    assert inputs, "no audioinput devices enumerated (Chromium filtered the virtual mic?)"

    # Start a getUserMedia analyser that tracks the peak RMS into window.__micMax.
    started = cdp_eval(
        ws, 92,
        "navigator.mediaDevices.getUserMedia({audio:{echoCancellation:false,"
        "noiseSuppression:false,autoGainControl:false}}).then(s=>{"
        "const c=new AudioContext();const src=c.createMediaStreamSource(s);"
        "const an=c.createAnalyser();an.fftSize=2048;src.connect(an);"
        "const buf=new Float32Array(an.fftSize);window.__micMax=0;"
        "window.__micTimer=setInterval(()=>{an.getFloatTimeDomainData(buf);"
        "let sum=0;for(const v of buf)sum+=v*v;"
        "const rms=Math.sqrt(sum/buf.length);"
        "if(rms>window.__micMax)window.__micMax=rms;},50);return true;})",
        await_promise=True,
    )
    assert started is True, f"getUserMedia did not start: {started}"

    # Inject a tone into the virtual mic (blocks ~1.5s while it plays).
    write_tone_wav(os.path.join(workdir, "workspace", "tts", "mic_tone.wav"))
    st, _ = http("POST", f"http://127.0.0.1:{HOST_MEETINGD}/mic/play",
                 {"path": "tts/mic_tone.wav"})
    assert st == 200, f"/mic/play returned {st}"
    time.sleep(0.5)  # let the analyser drain the tail

    peak = cdp_eval(ws, 93, "(window.__micMax||0)")
    print(f"      mic peak RMS = {peak}")
    # Silence sits at ~0; a real tone through the virtual mic lands well above 1e-3.
    assert isinstance(peak, (int, float)) and peak > 1e-3, (
        f"virtual mic captured silence (peak={peak}); the tab did NOT hear /mic/play"
    )
    print("PASS: bot -> room speaking path (tab heard the injected tone)")


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

        print("[5/6] assert non-silent WAV ...")
        wav = os.path.join(workdir, "workspace", "meetings", "smoke.wav")
        db = wav_rms_dbfs(wav)
        print(f"      WAV RMS = {db:.2f} dBFS (floor {FLOOR_DBFS})")
        if db <= FLOOR_DBFS:
            print("FAIL: recording is silent")
            ws.close()
            return 1
        print("PASS: end-to-end non-silent capture on a bare box")

        print("[6/6] assert bot -> room speaking path (virtual mic) ...")
        mic_check(ws, workdir)
        ws.close()
        return 0
    finally:
        subprocess.run(["docker", "logs", NAME], capture_output=True)
        subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)


if __name__ == "__main__":
    sys.exit(main())
