"""Node-local speech-to-text sidecar for Citadel.

A tiny FastAPI service (faster-whisper on CPU) that loads a model once and
transcribes audio files placed in the node workspace, reusing the proven
node-local faster-whisper STT pattern already shipped elsewhere in Citadel.
Used by the citadel-cli TRANSCRIBE_AUDIO job handler, which POSTs a
workspace-relative audio path here.

Audio never leaves the node: the workspace is mounted read-only at /workspace
and the resulting transcript is returned to the local Go worker, which relays
it back over the VPN mesh to the user's own AceTeam org.

Diarization (Phase 1): BASIC. faster-whisper produces timestamped segments but
no speaker identities. We label segments heuristically (a new speaker after a
silence gap) so distinct speakers read better than unlabelled imports. Full
diarization (pyannote/whisperx) is deferred to a later phase.

citadel#1045: this service is the SENSOR half of the low-SNR-hallucination fix.
It accepts an optional model_size (loaded on demand), an optional denoise
preprocessing step (ffmpeg afftdn), and passes VAD/threshold tuning through to
faster-whisper. It also EXPOSES the per-segment probability signals
(no_speech_prob, avg_logprob, compression_ratio) that faster-whisper already
computes — the Go handler's no-speech/low-confidence guard reasons over those.
This service makes no policy decision itself; it only reports the signals.
"""

import os
import subprocess
import tempfile
import threading

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

# Workspace mount inside the container (see services/compose/transcribe.yml).
WORKSPACE_ROOT = os.environ.get("WORKSPACE_ROOT", "/workspace")

WHISPER_MODEL = os.environ.get("WHISPER_MODEL", "base")
WHISPER_DEVICE = os.environ.get("WHISPER_DEVICE", "cpu")
WHISPER_COMPUTE_TYPE = os.environ.get("WHISPER_COMPUTE_TYPE", "int8")

# Allowed faster-whisper model sizes. MUST stay in sync with the Go handler's
# allowedWhisperModelSizes (internal/jobs/transcribe_audio.go), which is the
# first line of defense; this set is defense-in-depth so the service never
# trusts the wire blindly. Matches faster-whisper 1.0.3's _MODELS keys.
ALLOWED_MODELS = {
    "tiny",
    "tiny.en",
    "base",
    "base.en",
    "small",
    "small.en",
    "medium",
    "medium.en",
    "large-v1",
    "large-v2",
    "large-v3",
    "large",
    "distil-large-v2",
    "distil-medium.en",
    "distil-small.en",
    "distil-large-v3",
}

# A pause longer than this (seconds) between segments is treated as a likely
# speaker change for BASIC diarization. Tuned conservatively; better than
# tagging every line the same.
SPEAKER_GAP_SECONDS = float(os.environ.get("WHISPER_SPEAKER_GAP_SECONDS", "2.0"))

app = FastAPI(title="citadel-whisper-service")

# Single-slot model cache (citadel#1045). Keeping every requested size resident
# would OOM a memory-tight node, so we hold ONE model at a time and swap when a
# different size is requested. The lock guards the load/swap only; an in-flight
# transcribe holds its own reference to the model object it was handed, so a
# concurrent swap never pulls the model out from under it (CTranslate2 models
# support concurrent transcribe calls). FastAPI runs sync endpoints on a
# threadpool, so concurrent /transcribe is real, not theoretical.
_model = None
_model_name = None
_model_lock = threading.Lock()


def _resolve_model_name(requested: str | None) -> str:
    name = requested or WHISPER_MODEL
    if name not in ALLOWED_MODELS:
        raise HTTPException(400, f"invalid model_size {name!r}")
    return name


def _get_model(model_size: str | None):
    """Return (model, resolved_name), loading/swapping under the lock on demand."""
    name = _resolve_model_name(model_size)
    with _model_lock:
        global _model, _model_name
        if _model is None or _model_name != name:
            from faster_whisper import WhisperModel

            # Drop the old reference before loading the new one so the previous
            # model can be freed once any in-flight call using it returns.
            _model = None
            _model_name = None
            _model = WhisperModel(
                name,
                device=WHISPER_DEVICE,
                compute_type=WHISPER_COMPUTE_TYPE,
            )
            _model_name = name
        return _model, name


class TranscribeRequest(BaseModel):
    # Workspace-relative path to the audio file (the Go handler strips the
    # host workspace prefix before sending). Joined under WORKSPACE_ROOT.
    audio_path: str
    # Optional ISO language hint (e.g. "en"); None = auto-detect.
    language: str | None = None
    # BASIC speaker labelling when True.
    diarize: bool = False

    # citadel#1045 tuning params, all optional. Absent = today's behavior.
    model_size: str | None = None
    denoise: bool = False
    vad_filter: bool | None = None
    no_speech_threshold: float | None = None
    compression_ratio_threshold: float | None = None
    # Wire name is logprob_threshold; faster-whisper's kwarg is
    # log_prob_threshold (mapped at the call site below).
    logprob_threshold: float | None = None
    condition_on_previous_text: bool | None = None


def _resolve_audio_path(audio_path: str) -> str:
    """Resolve an audio path safely under the workspace mount.

    Rejects paths that escape WORKSPACE_ROOT (defense in depth: the Go handler
    already validates, but the service must not trust the wire blindly).
    """
    candidate = os.path.normpath(os.path.join(WORKSPACE_ROOT, audio_path))
    root = os.path.normpath(WORKSPACE_ROOT)
    if candidate != root and not candidate.startswith(root + os.sep):
        raise HTTPException(400, "audio_path resolves outside the workspace")
    if not os.path.isfile(candidate):
        raise HTTPException(404, f"audio file not found: {audio_path}")
    return candidate


def _denoise_to_tmp(src_path: str) -> str:
    """Denoise src_path with ffmpeg (afftdn + band-pass) into a container-local
    temp WAV, returning its path. The workspace is mounted read-only, so the
    output MUST NOT go under WORKSPACE_ROOT — tempfile.gettempdir() is
    container-local and writable. The caller is responsible for cleanup.

    afftdn is ffmpeg's FFT denoiser; the 80 Hz high-pass drops rumble and the
    8 kHz low-pass drops hiss above the speech band. Resampled to 16 kHz mono
    to match whisper's expected input.
    """
    fd, tmp = tempfile.mkstemp(suffix=".wav", prefix="citadel-denoise-")
    os.close(fd)
    cmd = [
        "ffmpeg",
        "-y",
        "-nostdin",
        "-i",
        src_path,
        "-af",
        "afftdn=nf=-25,highpass=f=80,lowpass=f=8000",
        "-ac",
        "1",
        "-ar",
        "16000",
        tmp,
    ]
    proc = subprocess.run(cmd, capture_output=True, check=False)
    if proc.returncode != 0:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        stderr = proc.stderr.decode(errors="replace")[-500:]
        raise HTTPException(500, f"denoise (ffmpeg) failed: {stderr}")
    return tmp


def _label_speakers(segments: list[dict]) -> list[dict]:
    """Assign BASIC speaker labels by silence-gap heuristic.

    Increments the speaker number whenever the gap since the previous segment
    exceeds SPEAKER_GAP_SECONDS. This is intentionally simple; it just beats a
    single `[None]` label. Full diarization is deferred.
    """
    speaker_idx = 1
    prev_end: float | None = None
    for seg in segments:
        if prev_end is not None and (seg["start"] - prev_end) > SPEAKER_GAP_SECONDS:
            speaker_idx += 1
        seg["speaker"] = f"Speaker {speaker_idx}"
        prev_end = seg["end"]
    return segments


@app.get("/health")
def health():
    return {"status": "ok", "model": WHISPER_MODEL}


@app.post("/transcribe")
def transcribe(req: TranscribeRequest):
    path = _resolve_audio_path(req.audio_path)
    model, model_name = _get_model(req.model_size)

    denoised_tmp: str | None = None
    try:
        if req.denoise:
            denoised_tmp = _denoise_to_tmp(path)
            path = denoised_tmp

        kwargs: dict = {"beam_size": 5, "language": req.language}
        if req.vad_filter is not None:
            kwargs["vad_filter"] = req.vad_filter
        if req.no_speech_threshold is not None:
            kwargs["no_speech_threshold"] = req.no_speech_threshold
        if req.compression_ratio_threshold is not None:
            kwargs["compression_ratio_threshold"] = req.compression_ratio_threshold
        if req.logprob_threshold is not None:
            # Wire name logprob_threshold -> faster-whisper log_prob_threshold.
            kwargs["log_prob_threshold"] = req.logprob_threshold
        if req.condition_on_previous_text is not None:
            kwargs["condition_on_previous_text"] = req.condition_on_previous_text

        segments_iter, info = model.transcribe(path, **kwargs)

        segments = [
            {
                "start": round(s.start, 3),
                "end": round(s.end, 3),
                "text": s.text.strip(),
                # citadel#1045: expose the per-segment probability signals the
                # Go guard reasons over. faster-whisper computes these for every
                # segment regardless of tuning params.
                "no_speech_prob": round(s.no_speech_prob, 4),
                "avg_logprob": round(s.avg_logprob, 4),
                "compression_ratio": round(s.compression_ratio, 4),
            }
            for s in segments_iter
        ]
    finally:
        if denoised_tmp is not None:
            try:
                os.unlink(denoised_tmp)
            except OSError:
                pass

    if req.diarize:
        segments = _label_speakers(segments)

    text = " ".join(s["text"] for s in segments).strip()

    return {
        "text": text,
        "language": info.language,
        "language_probability": round(info.language_probability, 3),
        "duration": round(info.duration, 3),
        "model": model_name,
        "denoised": bool(req.denoise),
        "segments": segments,
        # Surface diarization status so callers know what they got.
        "diarization": "basic" if req.diarize else "none",
    }
