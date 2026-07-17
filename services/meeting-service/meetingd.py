"""meetingd -- the in-container session/media supervisor for the Citadel meeting
module (aceteam-ai/citadel-cli#514).

This is the sidecar control API that packages the meeting media stack
(Chromium + Xvfb + PulseAudio null sink + ffmpeg) as an installable citadel
module. The MEETING_JOIN job handler on the host is UNCHANGED; a later PR wires
it to drive this API over loopback CDP. For the first PR this proves the vertical
slice: a real health gate that guarantees non-silent audio capture, and a
session endpoint that launches Chromium on the managed Xvfb with a live CDP port.

Endpoints (all loopback only -- the compose binds the published ports to
127.0.0.1):

    GET    /health                       canary-tone audio probe (the point of
                                          this PR): loads a null sink, plays a
                                          generated tone into it, records the
                                          sink monitor, and asserts non-silence.
                                          200 when audio capture works, 503 when
                                          it does not (5xx so catalog.ProbeHealth
                                          treats it as unhealthy, never a 4xx
                                          which that prober counts as healthy).
    POST   /sessions                      launch Chromium on Xvfb :99 routed into
                                          a per-meeting null sink; returns the
                                          live CDP port.
    POST   /sessions/{id}/record          start ffmpeg recording the session sink
                                          monitor into a WAV under /workspace.
    POST   /sessions/{id}/record/stop     SIGINT ffmpeg so the WAV trailer is
                                          finalized (valid WAV), same semantics as
                                          the host NullSinkRecorder.
    DELETE /sessions/{id}                 kill Chromium, unload the sink.
    GET    /sessions/{id}                 session status.

The chrome launch flags mirror the host builder (internal/platform/cobrowse.go
buildChromeArgs + meeting_browser.go buildMeetingChromeArgs) so the container and
host record identically. Two flags are load-bearing and MUST NOT drift:
  --autoplay-policy=no-user-gesture-required  (#5098: without it the bot joins but
                                               records pure silence)
  --password-store=basic                      (#5122: build-independent cookie
                                               crypto for the seeded profile)
Two flags are container-specific deltas from the host builder, documented inline:
  --remote-debugging-address=0.0.0.0          (host uses 127.0.0.1; a docker port
                                               publish cannot reach a container
                                               loopback bind, so CDP binds all
                                               interfaces INSIDE the container and
                                               the compose publish restricts it to
                                               host loopback)
  --no-sandbox                                (Chromium's setuid sandbox is
                                               unavailable in the hardened
                                               container; isolation comes from the
                                               container boundary)
"""

from __future__ import annotations

import array
import math
import os
import shutil
import signal
import subprocess
import tempfile
import threading
import time
import uuid
import wave
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from dataclasses import dataclass, field

from fastapi import FastAPI
from fastapi.responses import JSONResponse
from pydantic import BaseModel

# --- configuration (env, with the same defaults the service.yaml declares) ---

MEETINGD_PORT = int(os.environ.get("MEETINGD_PORT", "8102"))
# The CDP port meetingd reports and the compose publishes (via socat). Chromium
# itself binds MEETING_CDP_INTERNAL_PORT on 127.0.0.1; a socat forwarder bridges
# the container-external port to it (see the Dockerfile note).
MEETING_CDP_PORT = int(os.environ.get("MEETING_CDP_PORT", "9223"))
MEETING_CDP_INTERNAL_PORT = int(os.environ.get("MEETING_CDP_INTERNAL_PORT", "9222"))
DISPLAY = os.environ.get("DISPLAY", ":99")
PROFILE_DIR = os.environ.get("MEETING_PROFILE_DIR", "/profile")
WORKSPACE = os.environ.get("CITADEL_WORKSPACE", "/workspace")

# Silence floor for the canary probe, in dBFS. A 440 Hz tone captured through the
# null-sink monitor lands well above -30 dBFS; pure silence (the failure mode that
# recorded nothing in the live crash) is near the WAV noise floor around
# -90 dBFS. -50 dBFS is a wide margin either side.
CANARY_FLOOR_DBFS = -50.0

# The mono/16 kHz WAV format faster-whisper wants; identical to the host
# buildAudioFFmpegArgs so the transcribe sidecar reads it unchanged.
WAV_CHANNELS = "1"
WAV_RATE = "16000"


def _chromium_binary() -> str | None:
    for name in ("chromium", "chromium-browser", "chrome", "google-chrome"):
        path = shutil.which(name)
        if path:
            return path
    return None


def build_chrome_args(cdp_port: int, profile_dir: str) -> list[str]:
    """Chromium command line for a meeting-bot launch inside the container.

    Mirrors internal/platform buildChromeArgs(meeting options) EXCEPT for the
    single documented container delta (--no-sandbox). CDP binds 127.0.0.1 exactly
    like the host builder; a socat forwarder (not a chrome flag) is what exposes it
    on the published port. Pure function so the flag set -- especially the two
    load-bearing flags -- is unit-testable without launching a browser.
    """
    return [
        f"--remote-debugging-port={cdp_port}",
        # Loopback CDP bind, identical to the host builder. Modern Chromium refuses
        # a non-loopback debugging bind, so exposure is done by socat, not here.
        "--remote-debugging-address=127.0.0.1",
        f"--user-data-dir={profile_dir}",
        "--no-first-run",
        "--no-default-browser-check",
        "--start-maximized",
        # stealth (on by default on the host)
        "--disable-blink-features=AutomationControlled",
        "--lang=en-US",
        # merged disable-features, emitted once (host builder note)
        "--disable-features=Translate",
        # softwareGL: managed Xvfb has no GPU
        "--disable-gpu",
        # load-bearing (#5122): build-independent cookie crypto for the profile
        "--password-store=basic",
        # load-bearing (#5098): let incoming WebRTC audio play with no user gesture
        "--autoplay-policy=no-user-gesture-required",
        # container-specific: no setuid sandbox available in the hardened container
        "--no-sandbox",
    ]


def _pactl_env() -> dict[str, str]:
    """Environment for pulse client tools. PULSE_SERVER / XDG_RUNTIME_DIR are set
    on the meetingd process by supervisord and inherited here; passing os.environ
    keeps that wiring intact for every subprocess."""
    return dict(os.environ)


def pulse_ready() -> bool:
    """True when a PulseAudio-protocol server answers (`pactl info`), the same
    pre-flight the host audio stack uses before advertising the meeting tag."""
    if not shutil.which("pactl"):
        return False
    try:
        r = subprocess.run(
            ["pactl", "info"],
            env=_pactl_env(),
            capture_output=True,
            timeout=5,
        )
        return r.returncode == 0
    except (subprocess.SubprocessError, OSError):
        return False


def _load_null_sink(sink_name: str) -> str:
    """Load a null sink and return its pactl module id. Mirrors the host
    NullSinkRecorder.LoadSink pactl invocation."""
    out = subprocess.check_output(
        [
            "pactl",
            "load-module",
            "module-null-sink",
            f"sink_name={sink_name}",
            f"sink_properties=device.description={sink_name}",
        ],
        env=_pactl_env(),
        timeout=10,
    )
    module_id = out.decode().strip()
    if not module_id:
        raise RuntimeError(f"pactl returned no module id for sink {sink_name}")
    return module_id


def _unload_module(module_id: str) -> None:
    try:
        subprocess.run(
            ["pactl", "unload-module", module_id],
            env=_pactl_env(),
            capture_output=True,
            timeout=10,
        )
    except (subprocess.SubprocessError, OSError):
        pass


def build_record_ffmpeg_args(monitor_source: str, out_path: str) -> list[str]:
    """ffmpeg args to record a pulse monitor to mono/16 kHz WAV. Identical to the
    host buildAudioFFmpegArgs so the recorded file is byte-for-byte the format the
    whisper sidecar already consumes."""
    return [
        "ffmpeg",
        "-hide_banner",
        "-loglevel",
        "error",
        "-f",
        "pulse",
        "-i",
        monitor_source,
        "-ac",
        WAV_CHANNELS,
        "-ar",
        WAV_RATE,
        "-y",
        out_path,
    ]


def _wav_rms_dbfs(path: str) -> float:
    """RMS of a 16-bit PCM WAV in dBFS. -120 for empty/silent so the caller can
    treat 'no samples' the same as 'flat silence'."""
    with wave.open(path, "rb") as w:
        if w.getsampwidth() != 2:
            raise ValueError("canary WAV is not 16-bit PCM")
        frames = w.readframes(w.getnframes())
    if not frames:
        return -120.0
    samples = array.array("h")
    samples.frombytes(frames)
    if len(samples) == 0:
        return -120.0
    sum_sq = 0.0
    for s in samples:
        sum_sq += float(s) * float(s)
    rms = math.sqrt(sum_sq / len(samples))
    if rms < 1e-9:
        return -120.0
    return 20.0 * math.log10(rms / 32768.0)


class CanaryResult(BaseModel):
    ok: bool
    rms_dbfs: float
    floor_dbfs: float = CANARY_FLOOR_DBFS
    detail: str = ""


def _generate_tone(path: str) -> None:
    """Render a 1 s 440 Hz tone to a WAV with ffmpeg's lavfi sine source."""
    subprocess.run(
        [
            "ffmpeg",
            "-hide_banner",
            "-loglevel",
            "error",
            "-f",
            "lavfi",
            "-i",
            "sine=frequency=440:duration=1",
            "-ac",
            "2",
            "-ar",
            "44100",
            "-y",
            path,
        ],
        env=_pactl_env(),
        check=True,
        timeout=15,
    )


def run_canary() -> CanaryResult:
    """The load-bearing health probe: prove the container can actually capture
    audio. Load a private null sink, play a tone into it, record its monitor, and
    assert the recording is not silent. This converts 'silent WAV discovered after
    a 1-hour meeting' into 'module unhealthy before dispatch'.

    Ordering matters (#5098-class bug): the recorder must be running before the
    tone plays, and pulse must answer before either. run_canary assumes pulse_ready
    was checked by the caller."""
    sink = f"citadel_canary_{uuid.uuid4().hex[:8]}"
    module_id = _load_null_sink(sink)
    tmpdir = tempfile.mkdtemp(prefix="canary_")
    tone = os.path.join(tmpdir, "tone.wav")
    cap = os.path.join(tmpdir, "cap.wav")
    rec: subprocess.Popen[bytes] | None = None
    try:
        _generate_tone(tone)
        # Record ~1.5 s of the sink monitor.
        rec_args = build_record_ffmpeg_args(f"{sink}.monitor", cap)
        rec_args = rec_args[:1] + ["-t", "1.5"] + rec_args[1:]
        rec = subprocess.Popen(rec_args, env=_pactl_env())
        # Let the recorder attach to the monitor before the tone plays.
        time.sleep(0.3)
        subprocess.run(
            ["paplay", f"--device={sink}", tone],
            env=_pactl_env(),
            check=True,
            timeout=10,
        )
        rec.wait(timeout=5)
        rms = _wav_rms_dbfs(cap)
        ok = rms > CANARY_FLOOR_DBFS
        return CanaryResult(
            ok=ok,
            rms_dbfs=round(rms, 2),
            detail="non-silent capture" if ok else "captured silence",
        )
    finally:
        if rec is not None and rec.poll() is None:
            rec.send_signal(signal.SIGINT)
            try:
                rec.wait(timeout=5)
            except subprocess.TimeoutExpired:
                rec.kill()
        _unload_module(module_id)
        shutil.rmtree(tmpdir, ignore_errors=True)


# --- session state ---


@dataclass
class Session:
    session_id: str
    sink_name: str
    sink_module_id: str
    chrome: subprocess.Popen[bytes]
    cdp_port: int
    created_at: float
    max_duration_seconds: int
    recorder: subprocess.Popen[bytes] | None = None
    record_path: str | None = None
    lock: threading.Lock = field(default_factory=threading.Lock)


class StartSessionRequest(BaseModel):
    session_id: str | None = None
    max_duration_seconds: int = 7200


class RecordRequest(BaseModel):
    out: str


@asynccontextmanager
async def _lifespan(_: FastAPI) -> AsyncIterator[None]:
    threading.Thread(target=_reaper, name="session-reaper", daemon=True).start()
    yield


app = FastAPI(title="meetingd", version="0.1.0", lifespan=_lifespan)

_sessions: dict[str, Session] = {}
_sessions_lock = threading.Lock()


def _safe_workspace_path(rel: str) -> str:
    """Resolve a workspace-relative output path, rejecting traversal outside the
    workspace mount."""
    rel = rel.strip().lstrip("/")
    if not rel:
        raise ValueError("empty output path")
    full = os.path.normpath(os.path.join(WORKSPACE, rel))
    workspace_root = os.path.normpath(WORKSPACE)
    if full != workspace_root and not full.startswith(workspace_root + os.sep):
        raise ValueError("output path escapes the workspace")
    return full


@app.get("/health")
def health() -> JSONResponse:
    chromium = _chromium_binary()
    problems: list[str] = []
    if chromium is None:
        problems.append("chromium not found")
    if not shutil.which("ffmpeg"):
        problems.append("ffmpeg not found")
    if not pulse_ready():
        problems.append("pulse server not ready")
    if problems:
        return JSONResponse(
            status_code=503,
            content={"status": "unhealthy", "problems": problems},
        )
    try:
        canary = run_canary()
    except Exception as exc:  # noqa: BLE001 -- any canary failure is unhealthy
        return JSONResponse(
            status_code=503,
            content={"status": "unhealthy", "problems": [f"canary error: {exc}"]},
        )
    if not canary.ok:
        # 503 (5xx) so catalog.ProbeHealth classifies this as UNHEALTHY. A 4xx
        # would be read as healthy by that prober.
        return JSONResponse(
            status_code=503,
            content={"status": "unhealthy", "canary": canary.model_dump()},
        )
    return JSONResponse(
        status_code=200,
        content={"status": "healthy", "canary": canary.model_dump()},
    )


@app.post("/sessions")
def start_session(req: StartSessionRequest) -> JSONResponse:
    chromium = _chromium_binary()
    if chromium is None:
        return JSONResponse(status_code=503, content={"error": "chromium not found"})
    if not pulse_ready():
        return JSONResponse(status_code=503, content={"error": "pulse server not ready"})

    with _sessions_lock:
        # One profile -> one meeting per node, baked in by the fixed CDP port
        # (mirrors the host one-meeting-at-a-time invariant).
        if _sessions:
            return JSONResponse(
                status_code=409,
                content={"error": "a meeting session is already active"},
            )
        sid = req.session_id or uuid.uuid4().hex[:12]
        sink = f"citadel_meeting_{sid}"
        module_id = _load_null_sink(sink)
        os.makedirs(PROFILE_DIR, exist_ok=True)
        env = dict(os.environ)
        env["DISPLAY"] = DISPLAY
        env["PULSE_SINK"] = sink
        # Chromium binds the loopback-internal CDP port; socat forwards the
        # published MEETING_CDP_PORT to it.
        args = build_chrome_args(MEETING_CDP_INTERNAL_PORT, PROFILE_DIR)
        try:
            chrome = subprocess.Popen(
                [chromium, *args],
                env=env,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
        except OSError as exc:
            _unload_module(module_id)
            return JSONResponse(status_code=500, content={"error": f"launch chromium: {exc}"})
        _sessions[sid] = Session(
            session_id=sid,
            sink_name=sink,
            sink_module_id=module_id,
            chrome=chrome,
            cdp_port=MEETING_CDP_PORT,
            created_at=time.time(),
            max_duration_seconds=max(60, int(req.max_duration_seconds)),
        )
    return JSONResponse(
        status_code=201,
        content={"session_id": sid, "cdp_port": MEETING_CDP_PORT, "sink": sink},
    )


def _get_session(session_id: str) -> Session | None:
    with _sessions_lock:
        return _sessions.get(session_id)


@app.get("/sessions/{session_id}")
def session_status(session_id: str) -> JSONResponse:
    s = _get_session(session_id)
    if s is None:
        return JSONResponse(status_code=404, content={"error": "no such session"})
    return JSONResponse(
        content={
            "session_id": s.session_id,
            "cdp_port": s.cdp_port,
            "sink": s.sink_name,
            "chrome_running": s.chrome.poll() is None,
            "recording": s.recorder is not None and s.recorder.poll() is None,
            "record_path": s.record_path,
            "age_seconds": round(time.time() - s.created_at, 1),
        }
    )


@app.post("/sessions/{session_id}/record")
def start_record(session_id: str, req: RecordRequest) -> JSONResponse:
    s = _get_session(session_id)
    if s is None:
        return JSONResponse(status_code=404, content={"error": "no such session"})
    try:
        out_path = _safe_workspace_path(req.out)
    except ValueError as exc:
        return JSONResponse(status_code=400, content={"error": str(exc)})
    with s.lock:
        if s.recorder is not None and s.recorder.poll() is None:
            return JSONResponse(status_code=409, content={"error": "already recording"})
        os.makedirs(os.path.dirname(out_path), exist_ok=True)
        rec = subprocess.Popen(
            build_record_ffmpeg_args(f"{s.sink_name}.monitor", out_path),
            env=_pactl_env(),
        )
        s.recorder = rec
        s.record_path = out_path
    return JSONResponse(status_code=201, content={"recording": True, "path": out_path})


@app.post("/sessions/{session_id}/record/stop")
def stop_record(session_id: str) -> JSONResponse:
    s = _get_session(session_id)
    if s is None:
        return JSONResponse(status_code=404, content={"error": "no such session"})
    with s.lock:
        if s.recorder is None:
            return JSONResponse(status_code=409, content={"error": "not recording"})
        rec = s.recorder
        if rec.poll() is None:
            # SIGINT (not SIGKILL) so ffmpeg writes the WAV trailer -> valid file,
            # same semantics as the host NullSinkRecorder.Stop.
            rec.send_signal(signal.SIGINT)
            try:
                rec.wait(timeout=10)
            except subprocess.TimeoutExpired:
                rec.kill()
        path = s.record_path
        s.recorder = None
    return JSONResponse(content={"recording": False, "path": path})


@app.delete("/sessions/{session_id}")
def end_session(session_id: str) -> JSONResponse:
    with _sessions_lock:
        s = _sessions.pop(session_id, None)
    if s is None:
        return JSONResponse(status_code=404, content={"error": "no such session"})
    _teardown(s)
    return JSONResponse(content={"session_id": session_id, "ended": True})


def _teardown(s: Session) -> None:
    with s.lock:
        if s.recorder is not None and s.recorder.poll() is None:
            s.recorder.send_signal(signal.SIGINT)
            try:
                s.recorder.wait(timeout=10)
            except subprocess.TimeoutExpired:
                s.recorder.kill()
        if s.chrome.poll() is None:
            s.chrome.terminate()
            try:
                s.chrome.wait(timeout=10)
            except subprocess.TimeoutExpired:
                s.chrome.kill()
    _unload_module(s.sink_module_id)


def _reaper() -> None:
    """Minimal session TTL: if citadel dies mid-meeting, chrome/ffmpeg keep
    running in the container; this sweep stops a session past its max duration so
    orphans consolidate into one bounded lifetime. Deliberately small -- richer
    orphan handling is a follow-up PR."""
    while True:
        time.sleep(30)
        now = time.time()
        expired: list[Session] = []
        with _sessions_lock:
            for sid, s in list(_sessions.items()):
                if now - s.created_at > s.max_duration_seconds:
                    expired.append(_sessions.pop(sid))
        for s in expired:
            _teardown(s)
