"""Hermetic contract checks; no GPU, model download, or ML dependencies."""

from __future__ import annotations

import contextlib
import io
import json
import os
import tempfile
import unittest
from pathlib import Path

import train


class FakeTokenizer:
    def apply_chat_template(self, messages, tokenize, add_generation_prompt):
        assert tokenize is False and add_generation_prompt is False
        return "\n".join(item["content"] for item in messages)


class TrainingContractTest(unittest.TestCase):
    def test_cached_model_resolves_main_snapshot_without_network(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            repo = home / "hub" / "models--Qwen--Qwen3-0.6B"
            snapshot = repo / "snapshots" / "abc123"
            snapshot.mkdir(parents=True)
            (snapshot / "config.json").write_text("{}", encoding="utf-8")
            (repo / "refs").mkdir()
            (repo / "refs" / "main").write_text("abc123", encoding="utf-8")
            previous = os.environ.get("HF_HOME")
            os.environ["HF_HOME"] = str(home)
            try:
                self.assertEqual(train.resolve_cached_model("Qwen/Qwen3-0.6B"), str(snapshot))
                (repo / "refs" / "main").write_text("../escape", encoding="utf-8")
                with self.assertRaises(ValueError):
                    train.resolve_cached_model("Qwen/Qwen3-0.6B")
            finally:
                if previous is None:
                    os.environ.pop("HF_HOME", None)
                else:
                    os.environ["HF_HOME"] = previous

    def test_progress_fields_are_finite_and_bounded(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            train.emit(120, 2, 0.42)
        event = json.loads(out.getvalue())
        self.assertEqual(
            event,
            {
                "type": "progress",
                "progress_percent": 100.0,
                "current_epoch": 2,
                "current_loss": 0.42,
            },
        )
        with self.assertRaises(ValueError):
            train.emit(50, 1, float("nan"))

    def test_dataset_accepts_text_and_chat_rows_only(self):
        with tempfile.TemporaryDirectory() as directory:
            dataset = Path(directory) / "train.jsonl"
            dataset.write_text(
                '{"text":"example"}\n'
                '{"messages":[{"role":"user","content":"question"}]}\n',
                encoding="utf-8",
            )
            previous = train.DATASET
            train.DATASET = dataset
            try:
                self.assertEqual(
                    train.load_rows(FakeTokenizer()),
                    [{"text": "example"}, {"text": "question"}],
                )
                dataset.write_text('{"url":"https://example.com"}\n', encoding="utf-8")
                with self.assertRaises(ValueError):
                    train.load_rows(FakeTokenizer())
            finally:
                train.DATASET = previous


if __name__ == "__main__":
    unittest.main()
