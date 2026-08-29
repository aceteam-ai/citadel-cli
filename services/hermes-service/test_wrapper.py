"""Unit tests for wrapper.py's secret-scrubbing helper (citadel#898).

Hermetic: no real Hermes CLI, no subprocess, no network. Pins the exact-literal-
value scrub contract described in wrapper.py's `_scrub_secrets` docstring --
redact a live secret's value if it's echoed verbatim, but never touch ordinary
text that merely happens to look secret-shaped.

Run:  python3 -m pytest services/hermes-service/test_wrapper.py
"""

from __future__ import annotations

from fastapi.testclient import TestClient

import wrapper


def test_scrub_secrets_redacts_known_leaked_value(monkeypatch):
    monkeypatch.setenv("OPENAI_API_KEY", "sk-testsecretvalue1234567890")
    text = "boom: auth failed for key sk-testsecretvalue1234567890 while calling provider"
    scrubbed = wrapper._scrub_secrets(text)
    assert "sk-testsecretvalue1234567890" not in scrubbed
    assert "[REDACTED_OPENAI_API_KEY]" in scrubbed


def test_scrub_secrets_redacts_every_configured_provider_key(monkeypatch):
    values = {}
    for name in wrapper._SECRET_ENV_NAMES:
        value = f"secretvalue-{name.lower()}"
        monkeypatch.setenv(name, value)
        values[name] = value

    text = " ".join(f"leaked={v}" for v in values.values())
    scrubbed = wrapper._scrub_secrets(text)

    for name, value in values.items():
        assert value not in scrubbed
        assert f"[REDACTED_{name}]" in scrubbed


def test_scrub_secrets_no_false_positive_on_ordinary_text(monkeypatch):
    """Ordinary reply text that does not contain any live secret value must
    pass through completely unchanged -- this is the whole point of an
    exact-literal-value scrub over a secret-shaped regex."""
    monkeypatch.setenv("OPENAI_API_KEY", "sk-testsecretvalue1234567890")
    monkeypatch.delenv("OPENROUTER_API_KEY", raising=False)

    text = (
        "Sure! Here's a summary: the API key format for many providers looks "
        "like `sk-...` followed by random characters, but this response does "
        "not contain any actual configured secret."
    )
    assert wrapper._scrub_secrets(text) == text


def test_scrub_secrets_ignores_unset_env_vars(monkeypatch):
    for name in wrapper._SECRET_ENV_NAMES:
        monkeypatch.delenv(name, raising=False)
    text = "nothing to scrub here, no provider keys are configured"
    assert wrapper._scrub_secrets(text) == text


def test_scrub_secrets_floor_skips_short_values(monkeypatch):
    """A value under the 8-char floor is not treated as a real secret -- avoids
    redacting trivially-short/incidental values (e.g. a key left as a short
    placeholder like "test" or "xxx" during local dev)."""
    monkeypatch.setenv("GLM_API_KEY", "short1")
    text = "the value short1 appears here"
    assert wrapper._scrub_secrets(text) == text


def test_scrub_secrets_handles_empty_string(monkeypatch):
    monkeypatch.setenv("OPENAI_API_KEY", "sk-testsecretvalue1234567890")
    assert wrapper._scrub_secrets("") == ""


def test_health_provider_list_stays_in_sync_with_scrub_list(monkeypatch):
    """/health's `provider_keys_configured` is built from the SAME
    `_SECRET_ENV_NAMES` tuple `_scrub_secrets` scrubs against -- pin that they
    can't silently drift apart (the module docstring claims this; nothing else
    enforces it)."""
    for name in wrapper._SECRET_ENV_NAMES:
        monkeypatch.setenv(name, f"secretvalue-{name.lower()}")

    with TestClient(wrapper.app) as client:
        resp = client.get("/health")

    assert resp.status_code == 200
    assert resp.json()["provider_keys_configured"] == sorted(wrapper._SECRET_ENV_NAMES)
