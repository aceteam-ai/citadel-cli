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

from fastapi import Body, FastAPI, Response
from fastapi.responses import JSONResponse, StreamingResponse
from pydantic import BaseModel


def _truthy(val: str | None) -> bool:
    return (val or "").strip().lower() in {"1", "true", "yes", "on"}

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

# --- virtual microphone (bot -> room speaking path, aceteam#7079) ---
#
# The container boots (citadel.pa) a null sink MIC_SINK whose monitor is remapped
# to a real capture source MIC_SOURCE, set as the default source so the Chromium
# tab publishes it as its mic. Audio played INTO MIC_SINK is therefore heard by the
# meeting. This is strictly ADDITIVE to the capture path (the per-meeting sinks and
# the canary both name `<sink>.monitor` EXPLICITLY, so making MIC_SOURCE the default
# source cannot touch them): a bot that never calls /mic/play publishes silence,
# exactly as before.
MIC_SINK = os.environ.get("MEETING_MIC_SINK", "citadel_mic")
MIC_SOURCE = os.environ.get("MEETING_MIC_SOURCE", "citadel_virtmic")
# By default a missing virtual mic is REPORTED in /health (never fails it): a node
# that pulls the new image but hits a remap-source hiccup must keep its existing
# meeting-CAPTURE capability, which works today. Set MEETING_MIC_REQUIRED truthy on
# a speaking node to make an absent mic a hard 503.
MIC_REQUIRED = _truthy(os.environ.get("MEETING_MIC_REQUIRED"))
# Upper bound on a single /mic/play or /mic/play/pcm playback. A TTS clip is
# seconds-to-a-minute; this is a safety cap, not the expected duration.
MIC_PLAY_TIMEOUT = int(os.environ.get("MEETING_MIC_PLAY_TIMEOUT", "120"))
# Default raw-PCM format for /mic/play/pcm when the caller omits the query params.
MIC_PCM_RATE = int(os.environ.get("MEETING_MIC_PCM_RATE", "24000"))
MIC_PCM_CHANNELS = int(os.environ.get("MEETING_MIC_PCM_CHANNELS", "1"))
# Default raw-PCM format for GET /sessions/{id}/capture/pcm (the room->bot HEAR
# path, aceteam#7079). 24 kHz mono matches the realtime engine's PCM16 format, so
# the converse bridge can forward captured frames without resampling; PulseAudio
# does the monitor->24 kHz resample inside pacat. Read size for one stream chunk
# is kept small so the bridge sees low-latency frames.
CAPTURE_PCM_RATE = int(os.environ.get("MEETING_CAPTURE_PCM_RATE", "24000"))
CAPTURE_PCM_CHANNELS = int(os.environ.get("MEETING_CAPTURE_PCM_CHANNELS", "1"))
CAPTURE_CHUNK_BYTES = int(os.environ.get("MEETING_CAPTURE_CHUNK_BYTES", "1920"))

# Serializes injection so two overlapping clips never garble the mic. One clip at a
# time; a second concurrent request gets 409 (no barge-in / mid-clip stop yet --
# that is the later realtime wave).
_mic_lock = threading.Lock()


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


def _pactl_short(kind: str) -> list[str]:
    """Names of the pulse objects of `kind` ("sinks"/"sources") via `pactl list
    short`. Column 1 is the object name (tab-separated). Returns [] on any failure
    so a probe treats 'pactl unavailable' the same as 'object absent'."""
    if not shutil.which("pactl"):
        return []
    try:
        r = subprocess.run(
            ["pactl", "list", "short", kind],
            env=_pactl_env(),
            capture_output=True,
            timeout=5,
            text=True,
            check=False,
        )
    except (subprocess.SubprocessError, OSError):
        return []
    if r.returncode != 0:
        return []
    names: list[str] = []
    for line in r.stdout.splitlines():
        parts = line.split("\t")
        if len(parts) >= 2:
            names.append(parts[1])
    return names


def virtual_mic_present() -> bool:
    """True when BOTH the virtual-mic null sink and its remapped source exist, i.e.
    citadel.pa's mic topology loaded. Cheap (two `pactl list short` calls), so it is
    safe to call on every /health."""
    return MIC_SINK in _pactl_short("sinks") and MIC_SOURCE in _pactl_short("sources")


def build_mic_decode_ffmpeg_args(in_path: str, out_path: str) -> list[str]:
    """ffmpeg args to decode ANY audio file the caller supplies into a plain WAV
    that paplay streams into the virtual-mic sink. Format-agnostic on input; the
    output is a mono 48 kHz WAV (a normal mic rate). Pure so it is unit-testable."""
    return [
        "ffmpeg",
        "-hide_banner",
        "-loglevel",
        "error",
        "-i",
        in_path,
        "-ac",
        "1",
        "-ar",
        "48000",
        "-y",
        out_path,
    ]


def build_paplay_mic_args(sink: str, wav_path: str) -> list[str]:
    """paplay args to play a WAV into `sink` (the virtual-mic null sink). Mirrors the
    canary's `paplay --device=<sink> <file>` invocation, which is proven to work."""
    return ["paplay", f"--device={sink}", wav_path]


def build_pacat_mic_args(sink: str, rate: int, channels: int) -> list[str]:
    """pacat args to play raw signed-16-bit little-endian PCM (read from stdin) into
    `sink`. This is the low-latency path a realtime TTS engine would stream to. Pure
    so the format flags are unit-testable."""
    return [
        "pacat",
        "--playback",
        f"--device={sink}",
        "--format=s16le",
        f"--rate={int(rate)}",
        f"--channels={int(channels)}",
    ]


def build_pacat_capture_args(source: str, rate: int, channels: int) -> list[str]:
    """pacat args to RECORD a pulse source (a per-session `<sink>.monitor`, i.e. the
    room's mixed audio) as raw signed-16-bit little-endian PCM to stdout, at the
    requested rate/channels (PulseAudio resamples the monitor to `rate` for us). This
    is the low-latency room->bot HEAR path the converse bridge streams from -- the
    mirror of build_pacat_mic_args. `--latency-msec=20` keeps the stream tight.
    Pure so the format flags are unit-testable."""
    return [
        "pacat",
        "--record",
        f"--device={source}",
        "--format=s16le",
        f"--rate={int(rate)}",
        f"--channels={int(channels)}",
        "--latency-msec=20",
    ]


def _play_file_into_mic(src_path: str) -> None:
    """Decode `src_path` to a scratch WAV and play it into MIC_SINK, so it is
    published on the virtual mic into the live meeting. Blocks until playback
    finishes (pulse paces the null sink at wall-clock rate)."""
    tmpdir = tempfile.mkdtemp(prefix="mic_")
    wav = os.path.join(tmpdir, "play.wav")
    # ONE budget shared across decode + playback (not MIC_PLAY_TIMEOUT each), so
    # meetingd's total worst case stays under MIC_PLAY_TIMEOUT and never holds
    # _mic_lock past the Go client's (larger) timeout -- otherwise the next call
    # gets a confusing 409 while a runaway clip is still "playing".
    start = time.monotonic()
    try:
        subprocess.run(
            build_mic_decode_ffmpeg_args(src_path, wav),
            env=_pactl_env(),
            check=True,
            timeout=MIC_PLAY_TIMEOUT,
        )
        remaining = max(1.0, MIC_PLAY_TIMEOUT - (time.monotonic() - start))
        subprocess.run(
            build_paplay_mic_args(MIC_SINK, wav),
            env=_pactl_env(),
            check=True,
            timeout=remaining,
        )
    finally:
        shutil.rmtree(tmpdir, ignore_errors=True)


def _play_pcm_into_mic(pcm: bytes, rate: int, channels: int) -> None:
    """Play raw s16le PCM bytes into MIC_SINK via pacat. Blocks until done."""
    subprocess.run(
        build_pacat_mic_args(MIC_SINK, rate, channels),
        env=_pactl_env(),
        input=pcm,
        check=True,
        timeout=MIC_PLAY_TIMEOUT,
    )


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
    # capture_proc is the live pacat --record streaming the room monitor to an HTTP
    # client (the converse bridge's HEAR path). At most one at a time per session;
    # killed on client disconnect (the StreamingResponse generator's finally) and on
    # teardown. Independent of `recorder` -- a pulse monitor supports many readers,
    # so recording to a WAV and live-streaming can run at once.
    capture_proc: subprocess.Popen[bytes] | None = None
    lock: threading.Lock = field(default_factory=threading.Lock)


class StartSessionRequest(BaseModel):
    session_id: str | None = None
    max_duration_seconds: int = 7200


class RecordRequest(BaseModel):
    out: str


class MicPlayRequest(BaseModel):
    # path is a workspace-relative audio file (any format ffmpeg decodes). It is
    # resolved through _safe_workspace_path, so it cannot escape the /workspace
    # mount -- same guard the recorder uses for its output path.
    path: str


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


def _virtual_mic_health() -> dict[str, object]:
    """The virtual-mic block attached to every /health body. Best-effort: a probe
    failure reports present=False rather than raising."""
    try:
        present = virtual_mic_present()
    except Exception:  # noqa: BLE001 -- report, never fail the whole probe
        present = False
    return {
        "present": present,
        "sink": MIC_SINK,
        "source": MIC_SOURCE,
        "required": MIC_REQUIRED,
    }


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
    mic = _virtual_mic_health()
    # The virtual mic is the SPEAKING path; a missing one never breaks CAPTURE, so by
    # default it is only reported. Only a node explicitly required to speak
    # (MEETING_MIC_REQUIRED) treats its absence as unhealthy.
    if MIC_REQUIRED and not mic["present"]:
        problems.append(f"virtual mic not present (sink={MIC_SINK}, source={MIC_SOURCE})")
    if problems:
        return JSONResponse(
            status_code=503,
            content={"status": "unhealthy", "problems": problems, "virtual_mic": mic},
        )
    try:
        canary = run_canary()
    except Exception as exc:  # noqa: BLE001 -- any canary failure is unhealthy
        return JSONResponse(
            status_code=503,
            content={
                "status": "unhealthy",
                "problems": [f"canary error: {exc}"],
                "virtual_mic": mic,
            },
        )
    if not canary.ok:
        # 503 (5xx) so catalog.ProbeHealth classifies this as UNHEALTHY. A 4xx
        # would be read as healthy by that prober.
        return JSONResponse(
            status_code=503,
            content={
                "status": "unhealthy",
                "canary": canary.model_dump(),
                "virtual_mic": mic,
            },
        )
    return JSONResponse(
        status_code=200,
        content={
            "status": "healthy",
            "canary": canary.model_dump(),
            "virtual_mic": mic,
        },
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


# --- listening path (room -> bot, aceteam#7079) --------------------------------
#
#   GET /sessions/{id}/capture/pcm?rate=<hz>&channels=<n>
#       Streams the session's room audio (the per-session `<sink>.monitor`, which
#       carries the OTHER participants -- NOT the bot's own virtual mic, a different
#       sink) as raw signed-16-bit little-endian PCM. This is the HEAR source the
#       converse bridge forwards to the realtime engine. Defaults 24000/1 to match
#       the engine's PCM16 format. At most one live capture per session; a second
#       concurrent GET returns 409. The pacat subprocess is killed when the client
#       disconnects (generator finally) or the session is torn down.


def _capture_pcm_stream(s: "Session", proc: subprocess.Popen[bytes]):
    """Yield raw PCM chunks from `proc`'s stdout until it ends or the HTTP client
    disconnects, then reap the pacat process and clear it off the session. FastAPI
    closes this generator when the client goes away, so the finally is what prevents
    a leaked pacat holding the monitor across reconnects."""
    try:
        assert proc.stdout is not None
        while True:
            chunk = proc.stdout.read(CAPTURE_CHUNK_BYTES)
            if not chunk:
                break
            yield chunk
    finally:
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
        with s.lock:
            if s.capture_proc is proc:
                s.capture_proc = None


@app.get("/sessions/{session_id}/capture/pcm")
def capture_pcm(
    session_id: str,
    rate: int = CAPTURE_PCM_RATE,
    channels: int = CAPTURE_PCM_CHANNELS,
    # Annotate the base Response (not a Union): FastAPI infers response_model from
    # the return annotation, and a Union isn't a `type`, so it would try to build a
    # Pydantic field from it and raise FastAPIError at route registration (the whole
    # service would fail to start). A Response subclass is passed through as-is.
) -> Response:
    s = _get_session(session_id)
    if s is None:
        return JSONResponse(status_code=404, content={"error": "no such session"})
    if rate <= 0 or channels <= 0:
        return JSONResponse(status_code=400, content={"error": "rate and channels must be positive"})
    if not pulse_ready():
        return JSONResponse(status_code=503, content={"error": "pulse server not ready"})
    with s.lock:
        if s.capture_proc is not None and s.capture_proc.poll() is None:
            return JSONResponse(status_code=409, content={"error": "already capturing"})
        try:
            proc = subprocess.Popen(
                build_pacat_capture_args(f"{s.sink_name}.monitor", rate, channels),
                env=_pactl_env(),
                stdout=subprocess.PIPE,
            )
        except OSError as exc:
            return JSONResponse(status_code=500, content={"error": f"start capture: {exc}"})
        s.capture_proc = proc
    return StreamingResponse(_capture_pcm_stream(s, proc), media_type="application/octet-stream")


# --- speaking path (bot -> room, aceteam#7079) ---------------------------------
#
# Two operations, one body shape each (cleaner than content-type switching):
#   POST /mic/play        JSON {"path": "<workspace-relative audio file>"}
#                         Decodes any ffmpeg-readable file and plays it into the
#                         virtual mic. 200 {"played": true, "source": "file", ...}.
#   POST /mic/play/pcm    raw body = signed-16-bit little-endian PCM
#                         query: ?rate=<hz>&channels=<n> (default 24000/1)
#                         Streams the bytes straight into the virtual mic via pacat.
#                         200 {"played": true, "source": "pcm", "bytes": N}.
#
# Both are node-wide (the mic is one boot-level device, not per session): they are
# serialized by _mic_lock and return 409 while a clip is already playing. Playback
# is SYNCHRONOUS (the request returns when the clip finishes) and there is no
# mid-clip stop / barge-in yet -- that is the later realtime wave.


def _mic_not_ready() -> JSONResponse | None:
    """Shared pre-flight for the speaking endpoints: pulse up and the virtual mic
    present. Returns a 503 response when not ready, else None."""
    if not pulse_ready():
        return JSONResponse(status_code=503, content={"error": "pulse server not ready"})
    if not virtual_mic_present():
        return JSONResponse(
            status_code=503,
            content={"error": f"virtual mic not present (sink={MIC_SINK}, source={MIC_SOURCE})"},
        )
    return None


@app.post("/mic/play")
def mic_play(req: MicPlayRequest) -> JSONResponse:
    not_ready = _mic_not_ready()
    if not_ready is not None:
        return not_ready
    try:
        src = _safe_workspace_path(req.path)
    except ValueError as exc:
        return JSONResponse(status_code=400, content={"error": str(exc)})
    if not os.path.isfile(src):
        return JSONResponse(status_code=404, content={"error": f"no such file: {req.path}"})
    if not _mic_lock.acquire(blocking=False):
        return JSONResponse(status_code=409, content={"error": "already speaking"})
    try:
        _play_file_into_mic(src)
    except subprocess.TimeoutExpired:
        return JSONResponse(status_code=504, content={"error": "mic playback timed out"})
    except subprocess.CalledProcessError as exc:
        return JSONResponse(status_code=500, content={"error": f"mic playback failed: {exc}"})
    finally:
        _mic_lock.release()
    return JSONResponse(content={"played": True, "source": "file", "path": src})


@app.post("/mic/play/pcm")
def mic_play_pcm(
    # Default to empty (not required) so an empty body reaches the explicit 400
    # below instead of FastAPI's generic 422 required-body error.
    pcm: bytes = Body(b"", media_type="application/octet-stream"),
    rate: int = MIC_PCM_RATE,
    channels: int = MIC_PCM_CHANNELS,
) -> JSONResponse:
    not_ready = _mic_not_ready()
    if not_ready is not None:
        return not_ready
    if not pcm:
        return JSONResponse(status_code=400, content={"error": "empty PCM body"})
    if rate <= 0 or channels <= 0:
        return JSONResponse(status_code=400, content={"error": "rate and channels must be positive"})
    if not _mic_lock.acquire(blocking=False):
        return JSONResponse(status_code=409, content={"error": "already speaking"})
    try:
        _play_pcm_into_mic(pcm, rate, channels)
    except subprocess.TimeoutExpired:
        return JSONResponse(status_code=504, content={"error": "mic playback timed out"})
    except subprocess.CalledProcessError as exc:
        return JSONResponse(status_code=500, content={"error": f"mic playback failed: {exc}"})
    finally:
        _mic_lock.release()
    return JSONResponse(content={"played": True, "source": "pcm", "bytes": len(pcm)})


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
        # Kill any live capture stream first so its pacat stops reading the monitor
        # before we unload the sink module (otherwise it fights the unload).
        if s.capture_proc is not None and s.capture_proc.poll() is None:
            s.capture_proc.terminate()
            try:
                s.capture_proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                s.capture_proc.kill()
        s.capture_proc = None
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
