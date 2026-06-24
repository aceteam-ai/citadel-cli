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
"""

import os

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

# Workspace mount inside the container (see services/compose/transcribe.yml).
WORKSPACE_ROOT = os.environ.get("WORKSPACE_ROOT", "/workspace")

WHISPER_MODEL = os.environ.get("WHISPER_MODEL", "base")
WHISPER_DEVICE = os.environ.get("WHISPER_DEVICE", "cpu")
WHISPER_COMPUTE_TYPE = os.environ.get("WHISPER_COMPUTE_TYPE", "int8")

# A pause longer than this (seconds) between segments is treated as a likely
# speaker change for BASIC diarization. Tuned conservatively; better than
# tagging every line the same.
SPEAKER_GAP_SECONDS = float(os.environ.get("WHISPER_SPEAKER_GAP_SECONDS", "2.0"))

app = FastAPI(title="citadel-whisper-service")

_model = None


def _get_model():
    """Lazy-load the faster-whisper model once on first transcription."""
    global _model
    if _model is None:
        from faster_whisper import WhisperModel

        _model = WhisperModel(
            WHISPER_MODEL,
            device=WHISPER_DEVICE,
            compute_type=WHISPER_COMPUTE_TYPE,
        )
    return _model


class TranscribeRequest(BaseModel):
    # Workspace-relative path to the audio file (the Go handler strips the
    # host workspace prefix before sending). Joined under WORKSPACE_ROOT.
    audio_path: str
    # Optional ISO language hint (e.g. "en"); None = auto-detect.
    language: str | None = None
    # BASIC speaker labelling when True.
    diarize: bool = False


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
    model = _get_model()

    segments_iter, info = model.transcribe(
        path,
        beam_size=5,
        language=req.language,
    )

    segments = [
        {
            "start": round(s.start, 3),
            "end": round(s.end, 3),
            "text": s.text.strip(),
        }
        for s in segments_iter
    ]

    if req.diarize:
        segments = _label_speakers(segments)

    text = " ".join(s["text"] for s in segments).strip()

    return {
        "text": text,
        "language": info.language,
        "language_probability": round(info.language_probability, 3),
        "segments": segments,
        # Surface diarization status so callers know what they got.
        "diarization": "basic" if req.diarize else "none",
    }
