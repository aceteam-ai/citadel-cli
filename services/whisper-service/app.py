"""Node-local speech-to-text + speaker-diarization sidecar for Citadel.

A small FastAPI service that loads its models once and transcribes audio files
placed in the node workspace, keeping audio on the user's own machine. Used by
the citadel-cli TRANSCRIBE_AUDIO job handler, which POSTs a workspace-relative
audio path here.

Audio never leaves the node: the workspace is mounted read-only at /workspace
and the resulting transcript is returned to the local Go worker, which relays
it back over the VPN mesh to the user's own AceTeam org.

Diarization (Slice B, epic aceteam-ai/aceteam#5821): REAL speaker diarization
via whisperx = faster-whisper transcription + wav2vec2 forced alignment (tight
word/segment timestamps) + pyannote speaker diarization (true voice identities,
labelled "SPEAKER_00", "SPEAKER_01", ...).

Fail-soft tiers, reported in the response `diarization` field so an
orchestrator can tell a genuine result from a fallback (never breaks
TRANSCRIBE_AUDIO):
  - "speaker" -- real pyannote diarization. Requires HF_TOKEN (pyannote's
     model is gated) and a `speaker` request. Segments carry true
     "SPEAKER_NN" identities.
  - "basic"   -- silence-gap heuristic ("Speaker 1/2..."). Used for the quick
     path, or when a `speaker` request can't reach pyannote (no token/model).
     It can't actually tell voices apart; it just beats a single unlabelled
     speaker.
  - "none"    -- plain transcription, no speaker labels requested.

Request modes: `diarize` = basic labelling (quick path); `speaker` = attempt
real pyannote (reprocess path), falling back to basic and reporting it.

Alignment (wav2vec2) is ungated and runs whenever the align model is available;
it only tightens timestamps and is skipped silently if it can't load.
"""

import logging
import os

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

logger = logging.getLogger("citadel-whisper-service")

# Workspace mount inside the container (see services/compose/transcribe.yml).
WORKSPACE_ROOT = os.environ.get("WORKSPACE_ROOT", "/workspace")

# faster-whisper model size. "base" is the CPU-friendly default kept for
# backward compatibility; set WHISPER_MODEL=large-v3 (fits a 24GB GPU) or
# "medium" for higher accuracy when a GPU is available.
WHISPER_MODEL = os.environ.get("WHISPER_MODEL", "base")

# Device selection. "auto" picks cuda when a CUDA device is visible, else cpu.
WHISPER_DEVICE = os.environ.get("WHISPER_DEVICE", "auto")

# Compute type. Empty => choose per device (float16 on cuda, int8 on cpu).
WHISPER_COMPUTE_TYPE = os.environ.get("WHISPER_COMPUTE_TYPE", "")

# Batch size for whisperx batched inference.
WHISPER_BATCH_SIZE = int(os.environ.get("WHISPER_BATCH_SIZE", "8"))

# HuggingFace token for the gated pyannote diarization model. Without it we
# fall back to the silence-gap heuristic. Accept either common env name.
HF_TOKEN = os.environ.get("HF_TOKEN") or os.environ.get("HUGGING_FACE_HUB_TOKEN")

# Optional speaker-count hints for pyannote (tighten clustering when known).
DIARIZE_MIN_SPEAKERS = os.environ.get("DIARIZE_MIN_SPEAKERS")
DIARIZE_MAX_SPEAKERS = os.environ.get("DIARIZE_MAX_SPEAKERS")

# Pyannote diarization model. Empty => whisperx's default
# (pyannote/speaker-diarization-community-1). Override to pin a specific gated
# model, e.g. "pyannote/speaker-diarization-3.1". All are gated and require the
# HF token above plus accepting the model's terms on HuggingFace.
DIARIZE_MODEL = os.environ.get("DIARIZE_MODEL") or None

# A pause longer than this (seconds) between segments is treated as a likely
# speaker change for the BASIC (fail-soft) heuristic.
SPEAKER_GAP_SECONDS = float(os.environ.get("WHISPER_SPEAKER_GAP_SECONDS", "2.0"))

app = FastAPI(title="citadel-whisper-service")

# Lazily-initialised, process-global model handles.
_whisper_model = None
_align_cache: dict[str, tuple] = {}
_diarize_pipeline = None
_diarize_unavailable = False  # sticky: don't retry a known-missing pipeline.


def _resolve_device() -> str:
    if WHISPER_DEVICE != "auto":
        return WHISPER_DEVICE
    try:
        import torch

        return "cuda" if torch.cuda.is_available() else "cpu"
    except Exception:
        return "cpu"


def _resolve_compute_type(device: str) -> str:
    if WHISPER_COMPUTE_TYPE:
        return WHISPER_COMPUTE_TYPE
    return "float16" if device == "cuda" else "int8"


def _get_whisper_model():
    """Lazy-load the whisperx (faster-whisper) model once."""
    global _whisper_model
    if _whisper_model is None:
        import whisperx

        device = _resolve_device()
        compute_type = _resolve_compute_type(device)
        logger.info(
            "loading whisper model=%s device=%s compute_type=%s",
            WHISPER_MODEL,
            device,
            compute_type,
        )
        _whisper_model = whisperx.load_model(
            WHISPER_MODEL, device, compute_type=compute_type
        )
    return _whisper_model


def _get_align_model(language_code: str):
    """Lazy-load (and cache per language) the wav2vec2 alignment model.

    Returns (model, metadata) or None if the align model can't be loaded for
    this language -- alignment is a timestamp refinement, never required.
    """
    if language_code in _align_cache:
        return _align_cache[language_code]
    try:
        import whisperx

        device = _resolve_device()
        model, metadata = whisperx.load_align_model(
            language_code=language_code, device=device
        )
        _align_cache[language_code] = (model, metadata)
        return _align_cache[language_code]
    except Exception as exc:  # noqa: BLE001 - alignment is best-effort.
        logger.warning("alignment model unavailable for %s: %s", language_code, exc)
        _align_cache[language_code] = None
        return None


def _get_diarize_pipeline():
    """Lazy-load the pyannote diarization pipeline once.

    Returns the pipeline, or None when it is unavailable (no HF token, gated
    model not accepted, or import/runtime failure). The None result is cached
    so we don't repeatedly pay a failing load on every request.
    """
    global _diarize_pipeline, _diarize_unavailable
    if _diarize_pipeline is not None:
        return _diarize_pipeline
    if _diarize_unavailable:
        return None
    if not HF_TOKEN:
        logger.warning(
            "diarization disabled: no HF_TOKEN/HUGGING_FACE_HUB_TOKEN set; "
            "pyannote's model is gated. Falling back to silence-gap labelling."
        )
        _diarize_unavailable = True
        return None
    try:
        # whisperx moved DiarizationPipeline between minor versions; support both.
        try:
            from whisperx.diarize import DiarizationPipeline
        except ImportError:
            from whisperx import DiarizationPipeline  # type: ignore[attr-defined]

        device = _resolve_device()
        logger.info("loading pyannote diarization pipeline on %s", device)
        # whisperx >=3.8 renamed the auth kwarg to `token` and takes an optional
        # `model_name`. Older releases used `use_auth_token`; support both.
        try:
            _diarize_pipeline = DiarizationPipeline(
                model_name=DIARIZE_MODEL, token=HF_TOKEN, device=device
            )
        except TypeError:
            _diarize_pipeline = DiarizationPipeline(
                use_auth_token=HF_TOKEN, device=device
            )
        return _diarize_pipeline
    except Exception as exc:  # noqa: BLE001 - fail-soft to basic labelling.
        logger.warning(
            "pyannote diarization unavailable (%s); falling back to silence-gap "
            "labelling. With HF_TOKEN set, accept the model terms at "
            "hf.co/pyannote/speaker-diarization-community-1 (whisperx's default "
            "model), or set DIARIZE_MODEL=pyannote/speaker-diarization-3.1 and "
            "accept that model plus hf.co/pyannote/segmentation-3.0.",
            exc,
        )
        _diarize_unavailable = True
        return None


class TranscribeRequest(BaseModel):
    # Workspace-relative path to the audio file (the Go handler strips the
    # host workspace prefix before sending). Joined under WORKSPACE_ROOT.
    audio_path: str
    # Optional ISO language hint (e.g. "en"); None = auto-detect.
    language: str | None = None
    # Produce speaker labels at all. The quick path sets this for BASIC
    # (silence-gap) labelling.
    diarize: bool = False
    # Request REAL pyannote diarization (implies diarize). The reprocess path
    # sets this. Falls back to basic labelling when the token/model is
    # unavailable, which the response reports so callers can stay retryable.
    speaker: bool = False


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


def _label_speakers_basic(segments: list[dict]) -> list[dict]:
    """Assign BASIC speaker labels by silence-gap heuristic (fail-soft tier).

    Increments the speaker number whenever the gap since the previous segment
    exceeds SPEAKER_GAP_SECONDS. Intentionally simple; it cannot actually tell
    voices apart, it just beats a single unlabelled speaker.
    """
    speaker_idx = 1
    prev_end: float | None = None
    for seg in segments:
        if prev_end is not None and (seg["start"] - prev_end) > SPEAKER_GAP_SECONDS:
            speaker_idx += 1
        seg["speaker"] = f"Speaker {speaker_idx}"
        prev_end = seg["end"]
    return segments


def _summarize_speakers(segments: list[dict]) -> list[dict]:
    """Build the speakers[] roster with per-speaker talk-time percentages.

    talkTimePct is the share of total spoken segment duration attributable to
    each speaker. Emitted for BOTH diarization tiers so callers never hit a
    missing field. `label` is a human-friendly name derived from the id.
    """
    totals: dict[str, float] = {}
    order: list[str] = []
    for seg in segments:
        spk = seg.get("speaker")
        if spk is None:
            continue
        if spk not in totals:
            totals[spk] = 0.0
            order.append(spk)
        totals[spk] += max(0.0, float(seg["end"]) - float(seg["start"]))

    total = sum(totals.values())
    speakers = []
    for spk in order:
        pct = round((totals[spk] / total) * 100, 1) if total > 0 else 0.0
        speakers.append({"id": spk, "label": _speaker_label(spk), "talkTimePct": pct})
    return speakers


def _speaker_label(speaker_id: str) -> str:
    """Human-friendly label from a speaker id.

    "SPEAKER_00" -> "Speaker 1"; a basic "Speaker 2" label passes through.
    """
    if speaker_id.startswith("SPEAKER_"):
        try:
            return f"Speaker {int(speaker_id.rsplit('_', 1)[1]) + 1}"
        except (ValueError, IndexError):
            return speaker_id
    return speaker_id


def _diarize_pyannote(audio, aligned_result: dict) -> bool:
    """Run real pyannote diarization and assign speakers onto segments in place.

    Returns True if diarization ran and labelled segments, False otherwise so
    the caller can fall back to the basic heuristic.
    """
    pipeline = _get_diarize_pipeline()
    if pipeline is None:
        return False
    try:
        import whisperx

        kwargs = {}
        if DIARIZE_MIN_SPEAKERS:
            kwargs["min_speakers"] = int(DIARIZE_MIN_SPEAKERS)
        if DIARIZE_MAX_SPEAKERS:
            kwargs["max_speakers"] = int(DIARIZE_MAX_SPEAKERS)
        diarize_segments = pipeline(audio, **kwargs)
        assigned = whisperx.assign_word_speakers(diarize_segments, aligned_result)
        # assign_word_speakers returns the mutated result in recent versions.
        result = assigned if isinstance(assigned, dict) else aligned_result

        # Ensure every segment carries a speaker (assign leaves silences blank).
        last = "SPEAKER_00"
        for seg in result["segments"]:
            spk = seg.get("speaker")
            if spk is None:
                seg["speaker"] = last
            else:
                last = spk
        aligned_result["segments"] = result["segments"]
        return True
    except Exception as exc:  # noqa: BLE001 - fail-soft to basic labelling.
        logger.warning("pyannote diarization run failed (%s); using basic", exc)
        return False


@app.get("/health")
def health():
    """Fast, dependency-free readiness probe.

    Never loads a model, so it answers immediately and keeps the Go handler's
    health poll from tripping its 120s model-load budget. Reports whether a
    diarization token is configured so callers can predict the tier.
    """
    return {
        "status": "ok",
        "model": WHISPER_MODEL,
        "device": WHISPER_DEVICE,
        # Best tier a "speaker" request could achieve given current config.
        "diarization": "speaker" if HF_TOKEN else "basic",
    }


@app.post("/transcribe")
def transcribe(req: TranscribeRequest):
    import whisperx

    path = _resolve_audio_path(req.audio_path)
    model = _get_whisper_model()
    device = _resolve_device()

    audio = whisperx.load_audio(path)
    result = model.transcribe(
        audio, batch_size=WHISPER_BATCH_SIZE, language=req.language
    )
    language = result.get("language") or (req.language or "en")

    # Forced alignment (ungated wav2vec2): tightens word/segment timestamps.
    align = _get_align_model(language)
    if align is not None:
        try:
            align_model, metadata = align
            result = whisperx.align(
                result["segments"],
                align_model,
                metadata,
                audio,
                device,
                return_char_alignments=False,
            )
        except Exception as exc:  # noqa: BLE001 - alignment is best-effort.
            logger.warning("alignment failed (%s); using unaligned timestamps", exc)

    # Diarization tier reported to callers so an orchestrator can distinguish a
    # genuine diarized result (safe to REPLACE the quick transcript) from a
    # fallback (keep the quick transcript, stay retryable):
    #   "speaker" - real pyannote ran, segments carry true SPEAKER_NN ids.
    #   "basic"   - silence-gap fallback (no token/model, or basic requested).
    #   "none"    - plain transcription, no speaker labels.
    want_labels = req.diarize or req.speaker
    diarization_tier = "none"
    if want_labels:
        if req.speaker and _diarize_pyannote(audio, result):
            diarization_tier = "speaker"
        else:
            _label_speakers_basic(result["segments"])
            diarization_tier = "basic"

    segments = []
    for s in result["segments"]:
        seg = {
            "start": round(float(s["start"]), 3),
            "end": round(float(s["end"]), 3),
            "text": s["text"].strip(),
        }
        if "speaker" in s:
            seg["speaker"] = s["speaker"]
        segments.append(seg)

    text = " ".join(s["text"] for s in segments).strip()
    speakers = _summarize_speakers(segments) if want_labels else []

    return {
        "text": text,
        "language": language,
        "segments": segments,
        "speakers": speakers,
        # Surface which fail-soft tier produced the labels.
        "diarization": diarization_tier,
    }
