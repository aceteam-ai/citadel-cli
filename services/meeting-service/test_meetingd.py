"""Unit tests for meetingd's pure logic (aceteam-ai/citadel-cli#514).

These need no docker, no display, no pulse -- they pin the parts that would
silently break audio capture if they drifted: the two load-bearing chrome flags,
the record ffmpeg format (must match the host builder byte-for-byte so the whisper
sidecar reads it), workspace path safety, and the RMS math the canary relies on.

Run:  python3 -m pytest services/meeting-service/test_meetingd.py
"""

from __future__ import annotations

import io
import json
import math
import os
import shutil
import struct
import subprocess
import tempfile
import threading
import wave
from types import SimpleNamespace

import pytest
from fastapi.testclient import TestClient

import meetingd


@pytest.fixture
def mic_session(monkeypatch):
    """Install the one active session required by session-scoped mic routes."""
    session = SimpleNamespace(lock=threading.Lock(), ending=False, mic_process=None)
    monkeypatch.setattr(
        meetingd,
        "_get_session",
        lambda session_id: session if session_id == "s1" else None,
    )
    return session


def test_chrome_args_load_bearing_flags():
    args = meetingd.build_chrome_args(cdp_port=9223, profile_dir="/profile")
    # #5098: without this the bot joins the call but records pure silence.
    assert "--autoplay-policy=no-user-gesture-required" in args
    # #5122: build-independent cookie crypto so the seeded profile decrypts.
    assert "--password-store=basic" in args
    # softwareGL: managed Xvfb has no GPU.
    assert "--disable-gpu" in args
    # stealth core signal.
    assert "--disable-blink-features=AutomationControlled" in args
    assert "--user-data-dir=/profile" in args
    assert f"--remote-debugging-port=9223" in args


def test_chrome_args_container_delta():
    """CDP binds loopback exactly like the host builder (exposure is done by socat,
    not a chrome flag). The one documented deviation is --no-sandbox (the setuid
    sandbox is unavailable in the hardened container)."""
    args = meetingd.build_chrome_args(cdp_port=9222, profile_dir="/profile")
    assert "--remote-debugging-address=127.0.0.1" in args
    assert "--no-sandbox" in args
    # A non-loopback bind would be silently refused by modern Chromium.
    assert "--remote-debugging-address=0.0.0.0" not in args


def test_record_ffmpeg_args_match_host_format():
    """mono / 16 kHz WAV from a pulse monitor -- identical to the host
    buildAudioFFmpegArgs so the transcribe sidecar consumes it unchanged."""
    args = meetingd.build_record_ffmpeg_args("citadel_meeting_abc.monitor", "/workspace/meetings/abc.wav")
    assert args[0] == "ffmpeg"
    assert "-f" in args and args[args.index("-f") + 1] == "pulse"
    assert "-i" in args and args[args.index("-i") + 1] == "citadel_meeting_abc.monitor"
    assert args[args.index("-ac") + 1] == "1"
    assert args[args.index("-ar") + 1] == "16000"
    assert args[-1] == "/workspace/meetings/abc.wav"


@pytest.mark.parametrize(
    "rel,ok",
    [
        ("meetings/abc.wav", True),
        ("abc.wav", True),
        ("/meetings/abc.wav", True),  # leading slash is stripped, stays in-workspace
        ("../etc/passwd", False),
        ("meetings/../../etc/passwd", False),
        ("", False),
    ],
)
def test_safe_workspace_path(rel, ok, monkeypatch):
    monkeypatch.setattr(meetingd, "WORKSPACE", "/workspace")
    if ok:
        full = meetingd._safe_workspace_path(rel)
        assert full.startswith("/workspace")
    else:
        with pytest.raises(ValueError):
            meetingd._safe_workspace_path(rel)


def test_entrypoint_maps_to_host_uid_and_migrates_profile():
    """Bug A (host-UID mapping). The entrypoint must (1) remap the `bot` account to
    the node owner's UID/GID -- explicit PUID/PGID, else the /workspace mount owner
    -- so the recorded WAV is written node-owned (no cross-UID perms fixup), and
    (2) chown the bind-mounted PROFILE across on existing nodes where it was created
    by the old bot (uid 10001, mode 700); without that migration the signed-in
    Google session becomes unreadable after the remap. These are root/docker
    behaviors not unit-runnable in CI, so syntax-check the script and pin the
    load-bearing lines against accidental deletion."""
    script = os.path.join(os.path.dirname(__file__), "entrypoint.sh")
    subprocess.run(["sh", "-n", script], check=True)
    body = open(script).read()

    # Remap the bot account to the target UID/GID.
    assert "usermod -o -u" in body
    assert "groupmod -o -g" in body
    # Explicit PUID/PGID honored, with a fallback derived from the workspace owner.
    assert "PUID" in body and "PGID" in body
    assert "stat -c '%u'" in body and "WORKSPACE_DIR" in body
    # Migration: chown the persisted profile to the target.
    assert "PROFILE_DIR" in body
    assert "chown -R bot:bot \"$PROFILE_DIR\"" in body


def test_entrypoint_never_chowns_shared_workspace_root():
    """Bug fixes for citadel-cli#557 + #551. Both bugs traced back to the same
    flawed migration logic: chowning the shared /workspace bind mount at its
    ROOT. #551: when TARGET_UID fell back to the image default (PUID=0/unset,
    e.g. a root-run worker), a `chown -R` of the workspace root re-owned the
    ENTIRE shared workspace to 10001, breaking every host-worker file write.
    #557: the migration only recursed when the TOP dir's owner differed from
    the target, so an already node-owned workspace root silently skipped a
    stale, wrong-owned `meetings/` subdir left by a pre-#549 image.

    Fix: never chown the workspace root at all; confine the migration chown to
    the module's own `meetings/` subdir, unconditionally (not gated on the
    parent's owner, so a node-owned root no longer hides a stale subdir)."""
    script = os.path.join(os.path.dirname(__file__), "entrypoint.sh")
    subprocess.run(["sh", "-n", script], check=True)
    body = open(script).read()

    # No `chown -R` line may target the bare workspace root as one of its
    # arguments. Normalize each candidate line (strip quotes/braces so
    # "$WORKSPACE_DIR", "${WORKSPACE_DIR}", and an unquoted $WORKSPACE_DIR all
    # collapse to the same token) before comparing whitespace-split tokens --
    # a plain substring check would pass trivially against
    # "$WORKSPACE_DIR/meetings" (a superstring of the bad token) and miss
    # unquoted/braced regressions.
    for line in body.splitlines():
        stripped = line.strip()
        if stripped.startswith("#") or "chown -R" not in stripped:
            continue
        normalized = stripped.replace('"', "").replace("{", "").replace("}", "")
        tokens = normalized.split()
        assert "$WORKSPACE_DIR" not in tokens, (
            f"chown -R must not target the bare workspace root: {line!r}"
        )

    # The migration chown is confined to the meetings/ subdir, and creates it
    # first (so the chown can't silently no-op on a missing dir).
    assert 'mkdir -p "$WORKSPACE_DIR/meetings"' in body
    assert 'chown -R "$TARGET_UID:$TARGET_GID" "$WORKSPACE_DIR/meetings"' in body


def _write_wav(path: str, samples: list[int]):
    with wave.open(path, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(16000)
        w.writeframes(b"".join(struct.pack("<h", s) for s in samples))


def test_wav_rms_silence_is_low():
    with tempfile.TemporaryDirectory() as d:
        p = os.path.join(d, "silent.wav")
        _write_wav(p, [0] * 16000)
        assert meetingd._wav_rms_dbfs(p) <= meetingd.CANARY_FLOOR_DBFS


def test_wav_rms_tone_is_high():
    with tempfile.TemporaryDirectory() as d:
        p = os.path.join(d, "tone.wav")
        # half-scale 440 Hz sine -> well above the canary floor
        samples = [int(16000 * math.sin(2 * math.pi * 440 * i / 16000)) for i in range(16000)]
        _write_wav(p, samples)
        assert meetingd._wav_rms_dbfs(p) > meetingd.CANARY_FLOOR_DBFS


def test_wav_rms_empty_is_floor():
    with tempfile.TemporaryDirectory() as d:
        p = os.path.join(d, "empty.wav")
        _write_wav(p, [])
        assert meetingd._wav_rms_dbfs(p) == -120.0


def test_health_returns_503_when_pulse_down(monkeypatch):
    """The health gate is the point of this module: it must report UNHEALTHY (5xx,
    not 4xx -- catalog.ProbeHealth reads 4xx as healthy) when audio can't work, so
    a node that can't capture never accepts a meeting."""
    monkeypatch.setattr(meetingd, "_chromium_binary", lambda: "/usr/bin/chromium")
    monkeypatch.setattr(meetingd.shutil, "which", lambda _: "/usr/bin/ffmpeg")
    monkeypatch.setattr(meetingd, "pulse_ready", lambda: False)
    monkeypatch.setattr(meetingd, "virtual_mic_present", lambda: True)
    with TestClient(meetingd.app) as client:
        r = client.get("/health")
    assert r.status_code == 503
    assert r.json()["status"] == "unhealthy"


def test_health_returns_503_when_canary_silent(monkeypatch):
    """Pulse/chrome/ffmpeg all present but the canary captured silence -> 503. This
    is the exact regression (silent capture) the gate exists to catch."""
    monkeypatch.setattr(meetingd, "_chromium_binary", lambda: "/usr/bin/chromium")
    monkeypatch.setattr(meetingd.shutil, "which", lambda _: "/usr/bin/ffmpeg")
    monkeypatch.setattr(meetingd, "pulse_ready", lambda: True)
    monkeypatch.setattr(meetingd, "virtual_mic_present", lambda: True)
    monkeypatch.setattr(
        meetingd,
        "run_canary",
        lambda: meetingd.CanaryResult(ok=False, rms_dbfs=-95.0, detail="captured silence"),
    )
    with TestClient(meetingd.app) as client:
        r = client.get("/health")
    assert r.status_code == 503
    assert r.json()["status"] == "unhealthy"


def test_health_returns_200_when_canary_passes(monkeypatch):
    monkeypatch.setattr(meetingd, "_chromium_binary", lambda: "/usr/bin/chromium")
    monkeypatch.setattr(meetingd.shutil, "which", lambda _: "/usr/bin/ffmpeg")
    monkeypatch.setattr(meetingd, "pulse_ready", lambda: True)
    monkeypatch.setattr(meetingd, "virtual_mic_present", lambda: True)
    monkeypatch.setattr(
        meetingd,
        "run_canary",
        lambda: meetingd.CanaryResult(ok=True, rms_dbfs=-12.0, detail="non-silent capture"),
    )
    with TestClient(meetingd.app) as client:
        r = client.get("/health")
    assert r.status_code == 200
    assert r.json()["status"] == "healthy"
    # The virtual-mic block rides every /health body (additive; #7079).
    vm = r.json()["virtual_mic"]
    assert vm["present"] is True
    assert vm["sink"] == meetingd.MIC_SINK
    assert vm["source"] == meetingd.MIC_SOURCE


# --- virtual microphone (bot -> room speaking path, aceteam#7079) --------------


def test_health_reports_but_does_not_fail_on_missing_mic(monkeypatch):
    """CRITICAL additive contract: a missing virtual mic must NOT 503 /health by
    default -- that would strip meeting-CAPTURE capability from every node that
    pulls the new image but hits a remap-source hiccup. It is only REPORTED
    (present=false) unless MEETING_MIC_REQUIRED opts in."""
    monkeypatch.setattr(meetingd, "_chromium_binary", lambda: "/usr/bin/chromium")
    monkeypatch.setattr(meetingd.shutil, "which", lambda _: "/usr/bin/ffmpeg")
    monkeypatch.setattr(meetingd, "pulse_ready", lambda: True)
    monkeypatch.setattr(meetingd, "MIC_REQUIRED", False)
    monkeypatch.setattr(meetingd, "virtual_mic_present", lambda: False)
    monkeypatch.setattr(
        meetingd,
        "run_canary",
        lambda: meetingd.CanaryResult(ok=True, rms_dbfs=-12.0, detail="non-silent capture"),
    )
    with TestClient(meetingd.app) as client:
        r = client.get("/health")
    assert r.status_code == 200
    assert r.json()["virtual_mic"]["present"] is False


def test_health_fails_on_missing_mic_when_required(monkeypatch):
    """When MEETING_MIC_REQUIRED is set (a speaking node), an absent mic IS a 503."""
    monkeypatch.setattr(meetingd, "_chromium_binary", lambda: "/usr/bin/chromium")
    monkeypatch.setattr(meetingd.shutil, "which", lambda _: "/usr/bin/ffmpeg")
    monkeypatch.setattr(meetingd, "pulse_ready", lambda: True)
    monkeypatch.setattr(meetingd, "MIC_REQUIRED", True)
    monkeypatch.setattr(meetingd, "virtual_mic_present", lambda: False)
    with TestClient(meetingd.app) as client:
        r = client.get("/health")
    assert r.status_code == 503
    assert r.json()["virtual_mic"]["present"] is False


def test_mic_arg_builders():
    """Pin the pure arg builders for the speaking path (no pulse needed)."""
    dec = meetingd.build_mic_decode_ffmpeg_args("/tmp/play.wav")
    assert dec[0] == "ffmpeg"
    assert dec[dec.index("-protocol_whitelist") + 1] == "pipe"
    assert dec[dec.index("-i") + 1] == "pipe:0"
    assert dec[dec.index("-t") + 1] == str(meetingd.MIC_DECODE_LIMIT_SECONDS)
    assert dec[dec.index("-ac") + 1] == "1"
    assert dec[dec.index("-ar") + 1] == "48000"
    assert dec[dec.index("-c:a") + 1] == "pcm_s16le"
    assert dec[dec.index("-f") + 1] == "wav"
    assert dec[dec.index("-fs") + 1] == str(meetingd.MIC_DECODE_MAX_BYTES)
    assert dec[-1] == "/tmp/play.wav"

    pa = meetingd.build_paplay_mic_args("citadel_mic", "/tmp/play.wav")
    assert pa == ["paplay", "--device=citadel_mic", "/tmp/play.wav"]

    pc = meetingd.build_pacat_mic_args("citadel_mic", 24000, 1)
    assert pc[0] == "pacat"
    assert "--playback" in pc
    assert "--device=citadel_mic" in pc
    assert "--format=s16le" in pc
    assert "--rate=24000" in pc
    assert "--channels=1" in pc

    # The room->bot HEAR path RECORDS the per-session monitor to stdout.
    cap = meetingd.build_pacat_capture_args("citadel_meeting_abc.monitor", 24000, 1)
    assert cap[0] == "pacat"
    assert "--record" in cap
    assert "--playback" not in cap
    assert "--device=citadel_meeting_abc.monitor" in cap
    assert "--format=s16le" in cap
    assert "--rate=24000" in cap
    assert "--channels=1" in cap


def test_capture_pcm_stream_is_session_scoped_and_reaped(monkeypatch):
    session = SimpleNamespace(
        lock=threading.Lock(),
        ending=False,
        sink_name="citadel_meeting_s1",
        capture_process=None,
    )
    monkeypatch.setattr(meetingd, "_get_session", lambda sid: session if sid == "s1" else None)
    monkeypatch.setattr(meetingd, "pulse_ready", lambda: True)

    class CaptureProcess(_FakeProcess):
        def __init__(self):
            super().__init__()
            self.stdout = io.BytesIO(b"\x01\x02\x03\x04")

    proc = CaptureProcess()
    popen_args: list[list[str]] = []

    def fake_popen(args, **kwargs):
        popen_args.append(args)
        assert kwargs["stdout"] is subprocess.PIPE
        return proc

    monkeypatch.setattr(meetingd.subprocess, "Popen", fake_popen)
    with TestClient(meetingd.app) as client:
        response = client.get("/sessions/s1/capture/pcm?rate=24000&channels=1")
    assert response.status_code == 200
    assert response.content == b"\x01\x02\x03\x04"
    assert "--device=citadel_meeting_s1.monitor" in popen_args[0]
    assert proc.terminated is True
    assert session.capture_process is None


def test_capture_pcm_rejects_wrong_ending_and_duplicate_sessions(monkeypatch):
    monkeypatch.setattr(
        meetingd,
        "pulse_ready",
        lambda: pytest.fail("invalid session must be rejected before probing Pulse"),
    )
    monkeypatch.setattr(meetingd, "_get_session", lambda _: None)
    assert meetingd.capture_pcm("missing").status_code == 404

    running = _FakeProcess()
    session = SimpleNamespace(
        lock=threading.Lock(), ending=True, sink_name="sink", capture_process=None
    )
    monkeypatch.setattr(meetingd, "_get_session", lambda _: session)
    assert meetingd.capture_pcm("s1").status_code == 409
    session.ending = False
    session.capture_process = running
    monkeypatch.setattr(meetingd, "pulse_ready", lambda: True)
    assert meetingd.capture_pcm("s1").status_code == 409


def test_mic_play_503_when_mic_absent(monkeypatch, mic_session):
    """The speaking endpoints refuse (503) when the virtual mic is not present, so a
    caller gets a clear signal rather than silently playing into nothing."""
    monkeypatch.setattr(meetingd, "pulse_ready", lambda: True)
    monkeypatch.setattr(meetingd, "virtual_mic_present", lambda: False)
    with TestClient(meetingd.app) as client:
        r = client.post("/sessions/s1/mic/play", json={"path": "tts/hi.wav"})
    assert r.status_code == 503


def test_mic_play_404_on_missing_file(monkeypatch, mic_session):
    monkeypatch.setattr(meetingd, "pulse_ready", lambda: True)
    monkeypatch.setattr(meetingd, "virtual_mic_present", lambda: True)
    monkeypatch.setattr(meetingd, "WORKSPACE", "/workspace")
    with TestClient(meetingd.app) as client:
        r = client.post("/sessions/s1/mic/play", json={"path": "tts/does-not-exist.wav"})
    assert r.status_code == 404


def test_mic_play_rejects_path_traversal(monkeypatch, mic_session):
    monkeypatch.setattr(meetingd, "pulse_ready", lambda: True)
    monkeypatch.setattr(meetingd, "virtual_mic_present", lambda: True)
    monkeypatch.setattr(meetingd, "WORKSPACE", "/workspace")
    with TestClient(meetingd.app) as client:
        r = client.post("/sessions/s1/mic/play", json={"path": "../etc/passwd"})
    assert r.status_code == 400


def test_mic_play_rejects_workspace_symlink_escape(monkeypatch, tmp_path, mic_session):
    workspace = tmp_path / "workspace"
    outside = tmp_path / "outside"
    workspace.mkdir()
    outside.mkdir()
    (outside / "secret.wav").write_bytes(b"RIFF....")
    (workspace / "escape").symlink_to(outside, target_is_directory=True)
    monkeypatch.setattr(meetingd, "pulse_ready", lambda: True)
    monkeypatch.setattr(meetingd, "virtual_mic_present", lambda: True)
    monkeypatch.setattr(meetingd, "WORKSPACE", str(workspace))
    with TestClient(meetingd.app) as client:
        r = client.post("/sessions/s1/mic/play", json={"path": "escape/secret.wav"})
    assert r.status_code == 400


def test_mic_play_file_happy_path(monkeypatch, tmp_path, mic_session):
    """A present mic + a real file plays: the injection helper is stubbed (no pulse
    in CI) so this pins the endpoint wiring/response, not the audio subprocess."""
    ws = tmp_path
    (ws / "tts").mkdir()
    f = ws / "tts" / "hi.wav"
    f.write_bytes(b"RIFF....")
    played: dict[str, str] = {}
    monkeypatch.setattr(meetingd, "pulse_ready", lambda: True)
    monkeypatch.setattr(meetingd, "virtual_mic_present", lambda: True)
    monkeypatch.setattr(meetingd, "WORKSPACE", str(ws))
    monkeypatch.setattr(
        meetingd, "_play_file_into_mic", lambda session, p: played.update(path=p)
    )
    with TestClient(meetingd.app) as client:
        r = client.post("/sessions/s1/mic/play", json={"path": "tts/hi.wav"})
    assert r.status_code == 200
    body = r.json()
    assert body["played"] is True and body["source"] == "file"
    assert played["path"] == str(f)


def test_mic_play_pcm_happy_path(monkeypatch, mic_session):
    captured: dict[str, object] = {}
    monkeypatch.setattr(meetingd, "pulse_ready", lambda: True)
    monkeypatch.setattr(meetingd, "virtual_mic_present", lambda: True)
    monkeypatch.setattr(
        meetingd,
        "_play_pcm_into_mic",
        lambda session, pcm, rate, channels: captured.update(
            n=len(pcm), rate=rate, channels=channels
        ),
    )
    with TestClient(meetingd.app) as client:
        r = client.post(
            "/sessions/s1/mic/play/pcm?rate=16000&channels=1",
            content=b"\x01\x02\x03\x04",
            headers={"content-type": "application/octet-stream"},
        )
    assert r.status_code == 200
    assert r.json() == {"played": True, "source": "pcm", "bytes": 4}
    assert captured == {"n": 4, "rate": 16000, "channels": 1}


def test_mic_play_pcm_rejects_empty_body(monkeypatch, mic_session):
    monkeypatch.setattr(meetingd, "pulse_ready", lambda: True)
    monkeypatch.setattr(meetingd, "virtual_mic_present", lambda: True)
    with TestClient(meetingd.app) as client:
        r = client.post(
            "/sessions/s1/mic/play/pcm",
            content=b"",
            headers={"content-type": "application/octet-stream"},
        )
    assert r.status_code == 400


@pytest.mark.parametrize(
    "query,body,status",
    [
        ("rate=7999&channels=1", b"\x00\x00", 400),
        ("rate=24000&channels=9", b"\x00" * 18, 400),
        ("rate=24000&channels=2", b"\x00\x00", 400),
    ],
)
def test_mic_play_pcm_rejects_invalid_format_before_probe(
    monkeypatch, mic_session, query, body, status
):
    # Input validation is independent of device health and runs before Pulse probes.
    monkeypatch.setattr(
        meetingd,
        "pulse_ready",
        lambda: pytest.fail("invalid PCM must be rejected before probing Pulse"),
    )
    with TestClient(meetingd.app) as client:
        r = client.post(
            f"/sessions/s1/mic/play/pcm?{query}",
            content=body,
            headers={"content-type": "application/octet-stream"},
        )
    assert r.status_code == status


def test_mic_play_pcm_rejects_oversized_body(monkeypatch, mic_session):
    monkeypatch.setattr(meetingd, "MIC_PCM_MAX_BYTES", 4)
    monkeypatch.setattr(
        meetingd,
        "pulse_ready",
        lambda: pytest.fail("oversized PCM must be rejected before probing Pulse"),
    )
    with TestClient(meetingd.app) as client:
        r = client.post(
            "/sessions/s1/mic/play/pcm?rate=24000&channels=1",
            content=b"\x00" * 6,
            headers={"content-type": "application/octet-stream"},
        )
    assert r.status_code == 413


def test_mic_play_pcm_accepts_exact_duration_and_absolute_limit(monkeypatch, mic_session):
    # 8 kHz * mono * 2 bytes * 1 second = exactly 16,000 bytes. Both limits are
    # inclusive; only the first byte beyond either bound is rejected.
    monkeypatch.setattr(meetingd, "MIC_PLAY_TIMEOUT", 1)
    monkeypatch.setattr(meetingd, "MIC_PCM_MAX_BYTES", 16000)
    monkeypatch.setattr(meetingd, "pulse_ready", lambda: True)
    monkeypatch.setattr(meetingd, "virtual_mic_present", lambda: True)
    captured: dict[str, int] = {}
    monkeypatch.setattr(
        meetingd,
        "_play_pcm_into_mic",
        lambda session, pcm, rate, channels: captured.update(size=len(pcm)),
    )
    with TestClient(meetingd.app) as client:
        r = client.post(
            "/sessions/s1/mic/play/pcm?rate=8000&channels=1",
            content=b"\x00" * 16000,
            headers={"content-type": "application/octet-stream"},
        )
    assert r.status_code == 200
    assert captured == {"size": 16000}


def test_mic_routes_reject_wrong_and_ended_sessions_before_probe(monkeypatch, mic_session):
    monkeypatch.setattr(
        meetingd,
        "pulse_ready",
        lambda: pytest.fail("missing sessions must be rejected before probing Pulse"),
    )
    with TestClient(meetingd.app) as client:
        wrong = client.post("/sessions/wrong/mic/play", json={"path": "tts/hi.wav"})
        assert wrong.status_code == 404
        monkeypatch.setattr(meetingd, "_get_session", lambda _: None)
        ended = client.post(
            "/sessions/s1/mic/play/pcm?rate=24000&channels=1",
            content=b"\x00\x00",
            headers={"content-type": "application/octet-stream"},
        )
    assert ended.status_code == 404


def test_mic_file_duration_rejects_over_timeout(monkeypatch, tmp_path):
    wav = tmp_path / "too-long.wav"
    with wave.open(str(wav), "wb") as out:
        out.setnchannels(1)
        out.setsampwidth(2)
        out.setframerate(8000)
        out.writeframes(b"\x00" * 8000 * 2 * 2)
    monkeypatch.setattr(meetingd, "MIC_PLAY_TIMEOUT", 1)
    monkeypatch.setattr(meetingd, "MIC_DECODE_MAX_BYTES", wav.stat().st_size + 1)
    with pytest.raises(meetingd.MicMediaError, match="exceeds 1 seconds"):
        meetingd._validate_decoded_mic_wav(str(wav))


def test_open_workspace_audio_rejects_final_symlink(monkeypatch, tmp_path):
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    outside = tmp_path / "outside.wav"
    outside.write_bytes(b"RIFF")
    link = workspace / "linked.wav"
    link.symlink_to(outside)
    monkeypatch.setattr(meetingd, "WORKSPACE", str(workspace))
    with pytest.raises(meetingd.MicMediaError, match="opened safely"):
        with meetingd._open_workspace_audio(str(link)):
            pytest.fail("unsafe symlink was opened")


@pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="ffmpeg not installed")
def test_pipe_only_decode_rejects_nested_file_reference(tmp_path):
    outside = tmp_path / "outside.wav"
    with wave.open(str(outside), "wb") as wav:
        wav.setnchannels(1)
        wav.setsampwidth(2)
        wav.setframerate(8000)
        wav.writeframes(b"\x00" * 16000)
    manifest = f"#EXTM3U\n#EXTINF:1.0,\nfile://{outside}\n#EXT-X-ENDLIST\n".encode()
    decoded = tmp_path / "decoded.wav"
    result = subprocess.run(
        meetingd.build_mic_decode_ffmpeg_args(str(decoded)),
        input=manifest,
        capture_output=True,
        check=False,
    )
    assert result.returncode != 0
    assert b"not on whitelist" in result.stderr or b"Invalid data" in result.stderr


class _FakeProcess:
    def __init__(self, running: bool = True):
        self.returncode = None if running else 0
        self.terminated = False
        self.killed = False

    def poll(self):
        return self.returncode

    def terminate(self):
        self.terminated = True
        self.returncode = -15

    def kill(self):
        self.killed = True
        self.returncode = -9

    def wait(self, timeout=None):
        return self.returncode


def test_mic_stop_terminates_only_active_playback_and_is_idempotent(monkeypatch):
    mic = _FakeProcess()
    session = SimpleNamespace(lock=threading.Lock(), ending=False, mic_process=mic)
    monkeypatch.setattr(
        meetingd,
        "_get_session",
        lambda session_id: session if session_id == "s1" else None,
    )
    stopped = meetingd.mic_stop("s1")
    again = meetingd.mic_stop("s1")
    missing = meetingd.mic_stop("missing")
    assert stopped.status_code == 200
    assert json.loads(stopped.body) == {"stopped": True}
    assert mic.terminated is True and mic.killed is False
    assert session.mic_process is None
    assert again.status_code == 200 and json.loads(again.body) == {"stopped": False}
    assert missing.status_code == 404


def test_mic_stop_waits_for_playback_lock_release_before_next_play(monkeypatch):
    session = SimpleNamespace(lock=threading.Lock(), ending=False, mic_process=None)
    monkeypatch.setattr(meetingd, "_get_session", lambda _: session)
    monkeypatch.setattr(meetingd, "pulse_ready", lambda: True)
    monkeypatch.setattr(meetingd, "virtual_mic_present", lambda: True)
    started = threading.Event()
    first = _FakeProcess()
    calls = 0

    def play(_session, _pcm, _rate, _channels):
        nonlocal calls
        calls += 1
        if calls != 1:
            return
        with session.lock:
            session.mic_process = first
        started.set()
        while not first.terminated:
            threading.Event().wait(0.001)
        # Deliberately retain the request/global mic lock briefly after process
        # exit; mic_stop must not return until this handler's finally releases it.
        threading.Event().wait(0.02)
        with session.lock:
            if session.mic_process is first:
                session.mic_process = None

    monkeypatch.setattr(meetingd, "_play_pcm_into_mic", play)
    first_done = threading.Event()

    def run_first():
        meetingd._mic_play_pcm_response(session, b"\x00\x00", 24000, 1)
        first_done.set()

    thread = threading.Thread(target=run_first)
    thread.start()
    assert started.wait(timeout=1)
    stopped = meetingd.mic_stop("s1")
    assert stopped.status_code == 200
    assert first_done.wait(timeout=1)
    second = meetingd._mic_play_pcm_response(session, b"\x00\x00", 24000, 1)
    thread.join(timeout=1)
    assert second.status_code == 200
    assert calls == 2


def test_teardown_cancels_active_mic_process_without_holding_session_lock(monkeypatch):
    mic = _FakeProcess()
    session = meetingd.Session(
        session_id="s1",
        sink_name="sink",
        sink_module_id="7",
        chrome=_FakeProcess(running=False),
        cdp_port=9223,
        created_at=0,
        max_duration_seconds=60,
        mic_process=mic,
    )
    unloaded: list[str] = []
    monkeypatch.setattr(meetingd, "_unload_module", unloaded.append)
    meetingd._teardown(session)
    assert session.ending is True
    assert mic.terminated is True
    assert mic.killed is False
    assert session.mic_process is None
    assert unloaded == ["7"]


def test_teardown_cancels_active_capture_process(monkeypatch):
    capture = _FakeProcess()
    session = meetingd.Session(
        session_id="s1",
        sink_name="sink",
        sink_module_id="7",
        chrome=_FakeProcess(running=False),
        cdp_port=9223,
        created_at=0,
        max_duration_seconds=60,
        capture_process=capture,
    )
    monkeypatch.setattr(meetingd, "_unload_module", lambda _: None)
    meetingd._teardown(session)
    assert capture.terminated is True
    assert session.capture_process is None


def test_capture_process_stop_has_single_atomic_owner():
    capture = _FakeProcess()
    session = meetingd.Session(
        session_id="s1",
        sink_name="sink",
        sink_module_id="7",
        chrome=_FakeProcess(running=False),
        cdp_port=9223,
        created_at=0,
        max_duration_seconds=60,
        capture_process=capture,
    )
    # Stream-finally and teardown may race, but only one can detach/stop the
    # process. The loser observes no registered child and does nothing.
    first = meetingd._claim_capture_process(session, capture)
    second = meetingd._claim_capture_process(session)
    assert first is capture
    assert second is None
    meetingd._stop_capture_process(first)
    assert capture.terminated is True
    assert session.capture_process is None


def test_end_session_reserves_slot_until_teardown_finishes(monkeypatch):
    session = SimpleNamespace(lock=threading.Lock(), ending=False)
    monkeypatch.setattr(meetingd, "_sessions", {"s1": session})
    observed: list[bool] = []

    def fake_teardown(s):
        observed.append(meetingd._sessions.get("s1") is s and s.ending)

    monkeypatch.setattr(meetingd, "_teardown", fake_teardown)
    response = meetingd.end_session("s1")
    assert response.status_code == 200
    assert observed == [True]
    assert "s1" not in meetingd._sessions


def test_citadel_pa_declares_virtual_mic():
    """Pin the citadel.pa virtual-mic topology the way the entrypoint test pins its
    load-bearing lines: the null sink, the monitor REMAP to a real source (not the
    raw monitor, which Chromium filters), and the default-source selection."""
    pa = os.path.join(os.path.dirname(__file__), "citadel.pa")
    body = open(pa).read()
    # .nofail keeps a mic-line failure from killing PulseAudio (and CAPTURE) under
    # `pulseaudio -n -F`'s fail-fast default. The line a future cleanup deletes.
    assert ".nofail" in body
    assert "sink_name=citadel_mic" in body
    assert "module-remap-source" in body
    assert "master=citadel_mic.monitor" in body
    assert "source_name=citadel_virtmic" in body
    # device.class=sound is what stops Chromium filtering it as a monitor.
    assert "device.class=sound" in body
    assert "set-default-source citadel_virtmic" in body
