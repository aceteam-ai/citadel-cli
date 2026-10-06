"""Node-local speech-to-text sidecar for Citadel.

A tiny FastAPI service (faster-whisper on CPU) that loads a model once and
transcribes audio files placed in the node workspace, reusing the proven
node-local faster-whisper STT pattern already shipped elsewhere in Citadel.
Used by the citadel-cli TRANSCRIBE_AUDIO job handler, which POSTs a
workspace-relative audio path here.

Audio never leaves the node: the workspace is mounted read-only at /workspace
and the resulting transcript is returned to the local Go worker, which relays
it back over the VPN mesh to the user's own AceTeam org.

Diarization has two fail-soft tiers. Basic mode labels silence-separated
segments. Speaker mode uses gated pyannote voice diarization and emits stable
raw `SPEAKER_NN` join keys. Missing credentials, model access, or a transient
runtime failure returns the basic tier so transcription itself remains usable
and the orchestrator can retry the speaker pass later.

citadel#1045: this service is the SENSOR half of the low-SNR-hallucination fix.
It accepts an optional model_size (loaded on demand), an optional denoise
preprocessing step (ffmpeg afftdn), and passes VAD/threshold tuning through to
faster-whisper. It also EXPOSES the per-segment probability signals
(no_speech_prob, avg_logprob, compression_ratio) that faster-whisper already
computes — the Go handler's no-speech/low-confidence guard reasons over those.
This service makes no policy decision itself; it only reports the signals.
"""

import logging
import os
import subprocess
import tempfile
import threading
import time

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

logger = logging.getLogger("citadel-whisper-service")

# pyannote 4 enables anonymous telemetry by default. Keep the node-local
# privacy boundary unless an operator deliberately opts in before startup.
os.environ.setdefault("PYANNOTE_METRICS_ENABLED", "false")

# Workspace mount inside the container (see services/compose/transcribe.yml).
WORKSPACE_ROOT = os.environ.get("WORKSPACE_ROOT", "/workspace")

WHISPER_MODEL = os.environ.get("WHISPER_MODEL", "base")
WHISPER_DEVICE = os.environ.get("WHISPER_DEVICE", "cpu")
WHISPER_COMPUTE_TYPE = os.environ.get("WHISPER_COMPUTE_TYPE", "int8")

# Allowed faster-whisper model sizes. MUST stay in sync with the Go handler's
# allowedWhisperModelSizes (internal/jobs/transcribe_audio.go), which is the
# first line of defense; this set is defense-in-depth so the service never
# trusts the wire blindly. Preserves the established model set after upgrading
# the engine to faster-whisper 1.2.1.
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

# Real speaker diarization is opt-in because its model is gated. Initialization
# failures are retried after a bounded cooldown so repaired network access or
# newly accepted model terms recover without a container restart.
HF_TOKEN = os.environ.get("HF_TOKEN") or os.environ.get("HUGGING_FACE_HUB_TOKEN")
DIARIZE_MODEL = os.environ.get(
    "DIARIZE_MODEL", "pyannote/speaker-diarization-community-1"
)
DIARIZE_RETRY_SECONDS = max(1.0, float(os.environ.get("DIARIZE_RETRY_SECONDS", "30")))

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

_diarizer = None
_diarizer_lock = threading.Lock()
_diarizer_run_lock = threading.Lock()
_diarizer_next_retry_at = 0.0
_diarizer_last_error: str | None = None


def _resolve_model_name(requested: str | None) -> str:
    # Only the CALLER-supplied model_size is whitelisted. The env default
    # (WHISPER_MODEL) is returned untouched: an operator may legitimately set it
    # to a HuggingFace repo id or a local path (both accepted by WhisperModel),
    # and whitelisting it would 400 every default request on such a node —
    # breaking the "no new params behaves exactly as today" contract there.
    if requested:
        if requested not in ALLOWED_MODELS:
            raise HTTPException(400, f"invalid model_size {requested!r}")
        return requested
    return WHISPER_MODEL


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
    # Request gated pyannote diarization. Failure falls back to BASIC and the
    # response reports that tier so the orchestrator can retry safely.
    speaker: bool = False
    word_timestamps: bool = False

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
    root = os.path.realpath(WORKSPACE_ROOT)
    candidate = os.path.realpath(os.path.join(root, audio_path))
    try:
        contained = os.path.commonpath((root, candidate)) == root
    except ValueError:
        contained = False
    if not contained:
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


def _speaker_label(speaker_id: str) -> str:
    """Return a human-friendly label without changing the roster join key."""
    if speaker_id.startswith("SPEAKER_"):
        try:
            return f"Speaker {int(speaker_id.rsplit('_', 1)[1]) + 1}"
        except (IndexError, ValueError):
            pass
    return speaker_id


def _summarize_speakers(segments: list[dict]) -> list[dict]:
    """Build a stable roster from the finest speaker timing available.

    Speaker diarization can assign more than one speaker inside a single
    transcription segment.  When word timings carry speaker IDs, use those
    timings for both roster membership and talk-time percentages instead of
    charging the whole segment to its majority speaker.  BASIC diarization
    and requests without word timestamps retain the segment-level behavior.

    Word confidence is deliberately not a duration weight: it describes the
    transcription token, not the diarizer's speaker decision.
    """
    totals: dict[str, float] = {}
    order: list[str] = []

    def add(speaker: str | None, start: float, end: float) -> None:
        if not speaker:
            return
        duration = max(0.0, float(end) - float(start))
        if duration <= 0.0:
            return
        if speaker not in totals:
            totals[speaker] = 0.0
            order.append(speaker)
        totals[speaker] += duration

    for segment in segments:
        attributed_words = [
            word
            for word in segment.get("words", [])
            if word.get("speaker") and float(word["end"]) > float(word["start"])
        ]
        if attributed_words:
            for word in attributed_words:
                add(word["speaker"], word["start"], word["end"])
        else:
            add(segment.get("speaker"), segment["start"], segment["end"])
    total = sum(totals.values())
    return [
        {
            "id": speaker,
            "label": _speaker_label(speaker),
            "talkTimePct": round(100.0 * totals[speaker] / total, 1) if total else 0.0,
        }
        for speaker in order
    ]


def _load_diarizer():
    """Load pyannote once, retrying transient failures after a cooldown."""
    global _diarizer, _diarizer_last_error, _diarizer_next_retry_at
    if not HF_TOKEN:
        _diarizer_last_error = "missing_token"
        return None
    if _diarizer is not None:
        return _diarizer

    now = time.monotonic()
    if now < _diarizer_next_retry_at:
        return None
    with _diarizer_lock:
        if _diarizer is not None:
            return _diarizer
        now = time.monotonic()
        if now < _diarizer_next_retry_at:
            return None
        try:
            from pyannote.audio import Pipeline

            pipeline = Pipeline.from_pretrained(DIARIZE_MODEL, token=HF_TOKEN)
            if pipeline is None:
                raise RuntimeError("diarization model did not load")
            if WHISPER_DEVICE == "cuda":
                import torch

                pipeline.to(torch.device("cuda"))
            _diarizer = pipeline
            _diarizer_last_error = None
            _diarizer_next_retry_at = 0.0
        except Exception as exc:  # noqa: BLE001 - fail-soft retryable boundary.
            _diarizer_last_error = type(exc).__name__
            _diarizer_next_retry_at = time.monotonic() + DIARIZE_RETRY_SECONDS
            logger.warning(
                "speaker diarizer initialization failed (%s); retrying later",
                _diarizer_last_error,
            )
            return None
    return _diarizer


def _speaker_turns(output) -> list[tuple[float, float, str]]:
    """Normalize current and legacy pyannote pipeline outputs."""
    # pyannote 4's exclusive form removes overlapping turns specifically for
    # downstream transcription. Older outputs expose only speaker_diarization
    # or are already an Annotation.
    annotation = getattr(output, "exclusive_speaker_diarization", None)
    if annotation is None:
        annotation = getattr(output, "speaker_diarization", output)
    turns = [
        (float(turn.start), float(turn.end), str(speaker))
        for turn, _, speaker in annotation.itertracks(yield_label=True)
        if float(turn.end) > float(turn.start)
    ]
    return sorted(turns, key=lambda item: (item[0], item[1], item[2]))


def _best_speaker(
    start: float, end: float, turns: list[tuple[float, float, str]]
) -> str | None:
    overlaps: dict[str, float] = {}
    for turn_start, turn_end, speaker in turns:
        overlap = max(0.0, min(end, turn_end) - max(start, turn_start))
        if overlap:
            overlaps[speaker] = overlaps.get(speaker, 0.0) + overlap
    if overlaps:
        # Preserve the existing stable raw-ID tie-break when a word straddles
        # a speaker boundary with equal overlap.
        return max(overlaps, key=lambda speaker: (overlaps[speaker], speaker))
    if not turns:
        return None
    midpoint = (start + end) / 2.0
    return min(
        turns,
        key=lambda item: (abs(midpoint - ((item[0] + item[1]) / 2.0)), item[2]),
    )[2]


def _apply_speaker_turns(
    segments: list[dict], turns: list[tuple[float, float, str]]
) -> bool:
    if not turns:
        return False
    for segment in segments:
        segment["speaker"] = _best_speaker(segment["start"], segment["end"], turns)
        for word in segment.get("words", []):
            word["speaker"] = _best_speaker(word["start"], word["end"], turns)
    return True


def _decode_diarization_audio(path: str) -> dict:
    """Decode once into pyannote's supported in-memory audio contract.

    The locked torchcodec wheel cannot load its native image library in this
    CPU container, so handing pyannote a path would make it invoke an
    unavailable decoder. faster-whisper's locked PyAV boundary already
    produces the mono float32 samples that pyannote expects.
    """
    import torch
    from faster_whisper.audio import decode_audio

    sample_rate = 16_000
    decoded = decode_audio(path, sampling_rate=sample_rate)
    return {
        "waveform": torch.from_numpy(decoded).unsqueeze(0),
        "sample_rate": sample_rate,
    }


def _diarize(path: str, segments: list[dict]) -> bool:
    pipeline = _load_diarizer()
    if pipeline is None:
        return False
    try:
        with _diarizer_run_lock:
            audio = _decode_diarization_audio(path)
            output = pipeline(audio)
        return _apply_speaker_turns(segments, _speaker_turns(output))
    except Exception as exc:  # noqa: BLE001 - transcription must fail soft.
        logger.warning(
            "speaker diarization failed (%s); using basic labels", type(exc).__name__
        )
        return False


def _format_segment(segment, include_words: bool) -> dict:
    formatted = {
        "start": round(segment.start, 3),
        "end": round(segment.end, 3),
        "text": segment.text.strip(),
        "no_speech_prob": round(segment.no_speech_prob, 4),
        "avg_logprob": round(segment.avg_logprob, 4),
        "compression_ratio": round(segment.compression_ratio, 4),
    }
    if include_words:
        formatted["words"] = [
            {
                "word": word.word.strip(),
                "start": round(word.start, 3),
                "end": round(word.end, 3),
                "probability": round(word.probability, 4),
            }
            for word in (segment.words or [])
        ]
    return formatted


@app.get("/health")
def health():
    if not HF_TOKEN:
        capability = "missing_token"
    elif _diarizer is not None:
        capability = "ready"
    elif time.monotonic() < _diarizer_next_retry_at:
        capability = "retrying"
    else:
        capability = "configured"
    return {
        "status": "ok",
        "model": WHISPER_MODEL,
        "speaker_diarization": {
            "status": capability,
            "model": DIARIZE_MODEL,
            "last_error": _diarizer_last_error,
        },
    }


@app.post("/transcribe")
def transcribe(req: TranscribeRequest):
    path = _resolve_audio_path(req.audio_path)
    model, model_name = _get_model(req.model_size)

    denoised_tmp: str | None = None
    want_labels = req.diarize or req.speaker
    diarization_tier = "none"
    try:
        if req.denoise:
            denoised_tmp = _denoise_to_tmp(path)
            path = denoised_tmp

        kwargs: dict = {"beam_size": 5, "language": req.language}
        if req.word_timestamps:
            kwargs["word_timestamps"] = True
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

        segments = [_format_segment(s, req.word_timestamps) for s in segments_iter]
        if want_labels:
            if req.speaker and _diarize(path, segments):
                diarization_tier = "speaker"
            else:
                segments = _label_speakers(segments)
                diarization_tier = "basic"
    finally:
        if denoised_tmp is not None:
            try:
                os.unlink(denoised_tmp)
            except OSError:
                pass

    text = " ".join(s["text"] for s in segments).strip()

    return {
        "text": text,
        "language": info.language,
        "language_probability": round(info.language_probability, 3),
        "duration": round(info.duration, 3),
        "model": model_name,
        "denoised": bool(req.denoise),
        "segments": segments,
        "speakers": _summarize_speakers(segments) if want_labels else [],
        # Surface the actual tier so the caller can retain and retry a quick
        # transcript when gated speaker diarization was unavailable.
        "diarization": diarization_tier,
    }
