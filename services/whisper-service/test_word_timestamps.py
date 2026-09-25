"""Fixture contract for the opt-in faster-whisper word timing response."""

import importlib.util
from pathlib import Path
from types import SimpleNamespace


spec = importlib.util.spec_from_file_location("whisper_service", Path(__file__).with_name("app.py"))
assert spec and spec.loader
service = importlib.util.module_from_spec(spec)
spec.loader.exec_module(service)


def test_word_timestamps_are_opt_in_and_keep_seconds_and_probability():
    word = SimpleNamespace(word=" hello", start=0.12345, end=0.67891, probability=0.98765)
    segment = SimpleNamespace(
        start=0.0,
        end=0.8,
        text=" hello ",
        no_speech_prob=0.01,
        avg_logprob=-0.1,
        compression_ratio=1.2,
        words=[word],
    )

    ordinary = service._format_segment(segment, False)
    assert "words" not in ordinary
    assert ordinary["text"] == "hello"

    captioned = service._format_segment(segment, True)
    assert captioned["words"] == [
        {"word": "hello", "start": 0.123, "end": 0.679, "probability": 0.9877}
    ]
    assert {key: value for key, value in captioned.items() if key != "words"} == ordinary


def test_transcribe_requests_faster_whisper_word_alignment_only_when_enabled(monkeypatch):
    calls = []

    class FakeModel:
        def transcribe(self, path, **kwargs):
            calls.append(kwargs)
            return iter(()), SimpleNamespace(language="en", language_probability=1.0, duration=0.0)

    monkeypatch.setattr(service, "_resolve_audio_path", lambda _: "/workspace/clip.wav")
    monkeypatch.setattr(service, "_get_model", lambda _: (FakeModel(), "base"))

    service.transcribe(service.TranscribeRequest(audio_path="clip.wav"))
    service.transcribe(service.TranscribeRequest(audio_path="clip.wav", word_timestamps=True))

    assert "word_timestamps" not in calls[0]
    assert calls[1]["word_timestamps"] is True
