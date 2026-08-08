"""Unit tests for the whisper-service diarization helpers.

Covers the pure post-processing logic (speaker labelling, roster summarization,
label derivation) without loading any ML model. Run with:

    cd services/whisper-service && pip install pytest && pytest
"""

import app


def test_speaker_label_pyannote_ids_are_one_indexed():
    assert app._speaker_label("SPEAKER_00") == "Speaker 1"
    assert app._speaker_label("SPEAKER_01") == "Speaker 2"
    assert app._speaker_label("SPEAKER_12") == "Speaker 13"


def test_speaker_label_passthrough_for_basic_labels():
    # Basic-tier labels ("Speaker N") and anything unrecognised pass through.
    assert app._speaker_label("Speaker 2") == "Speaker 2"
    assert app._speaker_label("weird") == "weird"


def test_label_speakers_basic_increments_on_gap():
    segments = [
        {"start": 0.0, "end": 2.0, "text": "a"},
        {"start": 2.5, "end": 4.0, "text": "b"},  # gap 0.5 < 2.0 -> same
        {"start": 7.0, "end": 8.0, "text": "c"},  # gap 3.0 > 2.0 -> new
    ]
    out = app._label_speakers_basic(segments)
    assert [s["speaker"] for s in out] == ["Speaker 1", "Speaker 1", "Speaker 2"]


def test_summarize_speakers_join_key_and_talktime():
    # id MUST equal the raw segment.speaker label (the roster join key), with a
    # human-friendly name in `label`. talkTimePct is duration-weighted.
    segments = [
        {"start": 0.0, "end": 3.0, "text": "x", "speaker": "SPEAKER_00"},
        {"start": 3.0, "end": 4.0, "text": "y", "speaker": "SPEAKER_01"},
        {"start": 4.0, "end": 6.0, "text": "z", "speaker": "SPEAKER_00"},
    ]
    roster = app._summarize_speakers(segments)
    by_id = {s["id"]: s for s in roster}
    assert set(by_id) == {"SPEAKER_00", "SPEAKER_01"}
    # SPEAKER_00 spoke 5s of 6s total, SPEAKER_01 spoke 1s.
    assert by_id["SPEAKER_00"]["talkTimePct"] == round(5 / 6 * 100, 1)
    assert by_id["SPEAKER_01"]["talkTimePct"] == round(1 / 6 * 100, 1)
    # Human-friendly label is derived, id stays the raw join key.
    assert by_id["SPEAKER_00"]["label"] == "Speaker 1"
    assert by_id["SPEAKER_01"]["label"] == "Speaker 2"


def test_summarize_speakers_preserves_first_seen_order():
    segments = [
        {"start": 0.0, "end": 1.0, "text": "a", "speaker": "SPEAKER_01"},
        {"start": 1.0, "end": 2.0, "text": "b", "speaker": "SPEAKER_00"},
    ]
    roster = app._summarize_speakers(segments)
    assert [s["id"] for s in roster] == ["SPEAKER_01", "SPEAKER_00"]


def test_summarize_speakers_ignores_unlabelled_segments():
    segments = [
        {"start": 0.0, "end": 1.0, "text": "a"},
        {"start": 1.0, "end": 2.0, "text": "b", "speaker": "SPEAKER_00"},
    ]
    roster = app._summarize_speakers(segments)
    assert len(roster) == 1
    assert roster[0]["id"] == "SPEAKER_00"
    assert roster[0]["talkTimePct"] == 100.0


def test_resolve_compute_type_defaults_by_device():
    assert app._resolve_compute_type("cuda") == "float16"
    assert app._resolve_compute_type("cpu") == "int8"
