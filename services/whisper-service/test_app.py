"""Hermetic contract tests for the transcription and diarization boundary."""

import os
import sys
import threading
import time
import types
from types import SimpleNamespace

import pytest
from fastapi import HTTPException

import app


@pytest.fixture(autouse=True)
def reset_state(monkeypatch, tmp_path):
    monkeypatch.setattr(app, "WORKSPACE_ROOT", str(tmp_path))
    monkeypatch.setattr(app, "HF_TOKEN", "test-token")
    monkeypatch.setattr(app, "_diarizer", None)
    monkeypatch.setattr(app, "_diarizer_next_retry_at", 0.0)
    monkeypatch.setattr(app, "_diarizer_last_error", None)
    (tmp_path / "audio.wav").write_bytes(b"audio")


def _segment(text=" hello ", start=0.0, end=2.0):
    word = SimpleNamespace(word=" hello ", start=start, end=end, probability=0.91234)
    return SimpleNamespace(
        start=start,
        end=end,
        text=text,
        no_speech_prob=0.01234,
        avg_logprob=-0.12345,
        compression_ratio=1.23456,
        words=[word],
    )


class FakeModel:
    def __init__(self):
        self.kwargs = None

    def transcribe(self, path, **kwargs):
        self.kwargs = kwargs
        return iter([_segment()]), SimpleNamespace(
            language="en", language_probability=0.98765, duration=2.0
        )


class FakeTurn:
    def __init__(self, start, end):
        self.start = start
        self.end = end


class FakeAnnotation:
    def itertracks(self, *, yield_label):
        assert yield_label
        yield FakeTurn(0.0, 1.5), None, "SPEAKER_00"
        yield FakeTurn(1.5, 2.0), None, "SPEAKER_01"


def test_pyannote_exclusive_turns_are_preferred_for_transcription():
    exclusive = FakeAnnotation()

    class NonExclusiveMustNotRun:
        def itertracks(self, **_kwargs):
            raise AssertionError("non-exclusive annotation was used")

    output = SimpleNamespace(
        exclusive_speaker_diarization=exclusive,
        speaker_diarization=NonExclusiveMustNotRun(),
    )
    assert app._speaker_turns(output) == [
        (0.0, 1.5, "SPEAKER_00"),
        (1.5, 2.0, "SPEAKER_01"),
    ]


def test_speaker_success_preserves_tuning_words_and_confidence(monkeypatch):
    model = FakeModel()
    monkeypatch.setattr(app, "_get_model", lambda _name: (model, "medium"))
    monkeypatch.setattr(
        app,
        "_load_diarizer",
        lambda: lambda _path: SimpleNamespace(speaker_diarization=FakeAnnotation()),
    )

    result = app.transcribe(
        app.TranscribeRequest(
            audio_path="audio.wav",
            language="en",
            diarize=True,
            speaker=True,
            word_timestamps=True,
            model_size="medium",
            vad_filter=True,
            no_speech_threshold=0.6,
            compression_ratio_threshold=2.3,
            logprob_threshold=-0.7,
            condition_on_previous_text=False,
        )
    )

    assert model.kwargs == {
        "beam_size": 5,
        "language": "en",
        "word_timestamps": True,
        "vad_filter": True,
        "no_speech_threshold": 0.6,
        "compression_ratio_threshold": 2.3,
        "log_prob_threshold": -0.7,
        "condition_on_previous_text": False,
    }
    assert result["diarization"] == "speaker"
    assert result["segments"][0] == {
        "start": 0.0,
        "end": 2.0,
        "text": "hello",
        "no_speech_prob": 0.0123,
        "avg_logprob": -0.1235,
        "compression_ratio": 1.2346,
        "words": [
            {
                "word": "hello",
                "start": 0.0,
                "end": 2.0,
                "probability": 0.9123,
                "speaker": "SPEAKER_00",
            }
        ],
        "speaker": "SPEAKER_00",
    }
    assert result["speakers"] == [
        {"id": "SPEAKER_00", "label": "Speaker 1", "talkTimePct": 100.0}
    ]


def test_request_parsing_and_denoised_audio_reach_both_models(monkeypatch, tmp_path):
    model = FakeModel()
    model_paths = []
    diarizer_paths = []
    denoised = tmp_path / "denoised.wav"

    def transcribe(path, **kwargs):
        model_paths.append(path)
        return FakeModel.transcribe(model, path, **kwargs)

    model.transcribe = transcribe
    monkeypatch.setattr(app, "_get_model", lambda _name: (model, "base"))
    monkeypatch.setattr(
        app,
        "_denoise_to_tmp",
        lambda _path: (denoised.write_bytes(b"denoised") and str(denoised)),
    )

    def pipeline(path):
        diarizer_paths.append(path)
        return SimpleNamespace(speaker_diarization=FakeAnnotation())

    monkeypatch.setattr(app, "_load_diarizer", lambda: pipeline)
    request = app.TranscribeRequest.model_validate(
        {
            "audio_path": "audio.wav",
            "diarize": True,
            "speaker": True,
            "denoise": True,
        }
    )

    result = app.transcribe(request)

    assert result["diarization"] == "speaker"
    assert model_paths == [str(denoised)]
    assert diarizer_paths == [str(denoised)]
    assert not denoised.exists()


def test_speaker_failure_falls_back_and_reports_basic(monkeypatch):
    monkeypatch.setattr(app, "_get_model", lambda _name: (FakeModel(), "base"))
    monkeypatch.setattr(app, "_load_diarizer", lambda: None)

    result = app.transcribe(
        app.TranscribeRequest(audio_path="audio.wav", diarize=True, speaker=True)
    )

    assert result["diarization"] == "basic"
    assert result["segments"][0]["speaker"] == "Speaker 1"
    assert result["speakers"][0]["id"] == "Speaker 1"


def test_diarizer_initialization_failure_is_retryable(monkeypatch):
    calls = 0
    pipeline = object()

    class Pipeline:
        @staticmethod
        def from_pretrained(_model, *, token):
            nonlocal calls
            assert token == "test-token"
            calls += 1
            if calls == 1:
                raise OSError("temporary network failure")
            return pipeline

    package = types.ModuleType("pyannote")
    module = types.ModuleType("pyannote.audio")
    module.Pipeline = Pipeline
    monkeypatch.setitem(sys.modules, "pyannote", package)
    monkeypatch.setitem(sys.modules, "pyannote.audio", module)
    clock = iter([100.0, 100.0, 100.0, 100.0, 131.0, 131.0])
    monkeypatch.setattr(app.time, "monotonic", lambda: next(clock))

    assert app._load_diarizer() is None
    assert app._load_diarizer() is None
    assert app._load_diarizer() is pipeline
    assert calls == 2
    assert app._diarizer_last_error is None


def test_concurrent_diarizer_initialization_is_single_flight(monkeypatch):
    calls = 0
    pipeline = object()

    class Pipeline:
        @staticmethod
        def from_pretrained(_model, *, token):
            nonlocal calls
            calls += 1
            time.sleep(0.02)
            return pipeline

    package = types.ModuleType("pyannote")
    module = types.ModuleType("pyannote.audio")
    module.Pipeline = Pipeline
    monkeypatch.setitem(sys.modules, "pyannote", package)
    monkeypatch.setitem(sys.modules, "pyannote.audio", module)

    results = []
    threads = [
        threading.Thread(target=lambda: results.append(app._load_diarizer()))
        for _ in range(8)
    ]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()

    assert calls == 1
    assert results == [pipeline] * 8


def test_path_resolution_rejects_symlink_escape(tmp_path, monkeypatch):
    workspace = tmp_path / "workspace"
    outside = tmp_path / "outside"
    workspace.mkdir()
    outside.mkdir()
    (outside / "secret.wav").write_bytes(b"secret")
    os.symlink(outside, workspace / "escape")
    monkeypatch.setattr(app, "WORKSPACE_ROOT", str(workspace))

    with pytest.raises(HTTPException) as exc:
        app._resolve_audio_path("escape/secret.wav")
    assert exc.value.status_code == 400


def test_health_reports_retryable_capability_without_leaking_token(monkeypatch):
    monkeypatch.setattr(app, "_diarizer_next_retry_at", time.monotonic() + 30)
    monkeypatch.setattr(app, "_diarizer_last_error", "OSError")
    response = app.health()
    assert response["speaker_diarization"]["status"] == "retrying"
    assert "test-token" not in repr(response)
