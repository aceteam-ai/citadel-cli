"""Unit tests for meetingd's pure logic (aceteam-ai/citadel-cli#514).

These need no docker, no display, no pulse -- they pin the parts that would
silently break audio capture if they drifted: the two load-bearing chrome flags,
the record ffmpeg format (must match the host builder byte-for-byte so the whisper
sidecar reads it), workspace path safety, and the RMS math the canary relies on.

Run:  python3 -m pytest services/meeting-service/test_meetingd.py
"""

from __future__ import annotations

import math
import os
import struct
import tempfile
import wave

import pytest
from fastapi.testclient import TestClient

import meetingd


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


def test_ensure_node_accessible_dir_makes_shared_dir_world_accessible():
    """Bug A (live-prod node 1084, 2026-07-16): the WAV output dir is created by
    meetingd (bot UID 10001) but READ by the node owner (a different UID). Under
    bot's umask it lands 0700, which the node cannot even traverse. The helper must
    force 0o777 so the node can read the recording for the end-of-call transcribe,
    and must relax a PRE-EXISTING 0700 dir (exist_ok makedirs does not)."""
    with tempfile.TemporaryDirectory() as d:
        target = os.path.join(d, "meetings")

        # Fresh create.
        meetingd._ensure_node_accessible_dir(target)
        assert os.path.isdir(target)
        assert (os.stat(target).st_mode & 0o777) == 0o777

        # Pre-existing 0700 dir (an older meetingd left it restrictive) must be
        # relaxed, not left as-is.
        os.chmod(target, 0o700)
        meetingd._ensure_node_accessible_dir(target)
        assert (os.stat(target).st_mode & 0o777) == 0o777


def test_ensure_node_accessible_dir_chmod_failure_is_non_fatal(monkeypatch):
    """When the node created the dir first it OWNS it and meetingd's chmod is
    EPERM -- that must be swallowed (the node relaxed the mode itself), never
    crash the record start."""
    with tempfile.TemporaryDirectory() as d:
        target = os.path.join(d, "meetings")

        def _raise(*_a, **_k):
            raise PermissionError("not the owner")

        monkeypatch.setattr(meetingd.os, "chmod", _raise)
        # Must not raise despite chmod failing.
        meetingd._ensure_node_accessible_dir(target)
        assert os.path.isdir(target)


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
    monkeypatch.setattr(
        meetingd,
        "run_canary",
        lambda: meetingd.CanaryResult(ok=True, rms_dbfs=-12.0, detail="non-silent capture"),
    )
    with TestClient(meetingd.app) as client:
        r = client.get("/health")
    assert r.status_code == 200
    assert r.json()["status"] == "healthy"
