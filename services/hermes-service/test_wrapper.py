"""Unit tests for wrapper.py's secret-scrubbing helper (citadel#898).

Hermetic: no real Hermes CLI, no subprocess, no network. Pins the exact-literal-
value scrub contract described in wrapper.py's `_scrub_secrets` docstring --
redact a live secret's value if it's echoed verbatim, but never touch ordinary
text that merely happens to look secret-shaped.

Run:  python3 -m pytest services/hermes-service/test_wrapper.py
"""

from __future__ import annotations

import json
from types import SimpleNamespace

import pytest
import wrapper

TOKEN = "123e4567-e89b-42d3-a456-426614174000"
SECRET_NAMES = (
    "OPENROUTER_API_KEY", "OPENAI_API_KEY", "FIREWORKS_API_KEY",
    "GOOGLE_API_KEY", "GEMINI_API_KEY", "GLM_API_KEY", "KIMI_API_KEY",
    "MINIMAX_API_KEY", "ACETEAM_GATEWAY_KEY",
)


def test_scrub_secrets_redacts_known_leaked_value(monkeypatch):
    monkeypatch.setenv("OPENAI_API_KEY", "sk-testsecretvalue1234567890")
    text = "boom: auth failed for key sk-testsecretvalue1234567890 while calling provider"
    scrubbed = wrapper._scrub_secrets(text)
    assert "sk-testsecretvalue1234567890" not in scrubbed
    assert "[REDACTED_OPENAI_API_KEY]" in scrubbed


def test_scrub_secrets_redacts_every_configured_credential(monkeypatch):
    assert set(wrapper._SECRET_ENV_NAMES) == set(SECRET_NAMES)
    values = {}
    for name in SECRET_NAMES:
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
    """Health lists providers while the scrub also protects gateway auth."""
    for name in wrapper._SECRET_ENV_NAMES:
        monkeypatch.setenv(name, f"secretvalue-{name.lower()}")

    health = wrapper.health()

    assert health["provider_keys_configured"] == sorted(wrapper._PROVIDER_ENV_NAMES)
    assert "ACETEAM_GATEWAY_KEY" not in health["provider_keys_configured"]


@pytest.mark.parametrize("secret_name", SECRET_NAMES)
@pytest.mark.parametrize("path", ["raw_success", "stderr_failure", "stdout_failure", "unexpected"])
def test_every_credential_stays_out_of_all_callback_paths(
    monkeypatch, capsys, secret_name, path
):
    secret = (
        "gateway-secret-value" if secret_name == "ACETEAM_GATEWAY_KEY"
        else f"configured-secret-{secret_name.lower()}"
    )
    monkeypatch.setenv(secret_name, secret)
    monkeypatch.setattr(wrapper, "GATEWAY_KEY", "gateway-secret-value")
    monkeypatch.setattr(wrapper, "PLATFORM_URL", "https://platform.example")
    monkeypatch.setattr(wrapper, "INSTANCE_ID", "instance-1")
    bodies = []

    def urlopen(req, timeout):
        assert timeout == 30
        assert req.get_header("Authorization") == "Bearer gateway-secret-value"
        bodies.append(json.loads(req.data))
        raise RuntimeError("network unavailable")

    monkeypatch.setattr(wrapper.urllib.request, "urlopen", urlopen)
    if path == "unexpected":
        def fail(*args, **kwargs):
            raise OSError(f"failed to start Hermes with {secret}")
        monkeypatch.setattr(wrapper.subprocess, "run", fail)
    else:
        proc = SimpleNamespace(
            returncode=0 if path == "raw_success" else 7,
            stdout=f"answer with {secret}" if path in ("raw_success", "stdout_failure") else "",
            stderr=f"provider rejected {secret}" if path == "stderr_failure" else "",
        )
        monkeypatch.setattr(wrapper.subprocess, "run", lambda *args, **kwargs: proc)

    wrapper._process_turn("hello", TOKEN)

    assert len(bodies) == 1
    assert bodies[0]["turnToken"] == TOKEN
    assert secret not in json.dumps(bodies)
    expected = f"[REDACTED_{secret_name}]"
    if path == "raw_success":
        assert bodies[0]["reply"] == f"answer with {expected}"
    elif path == "stderr_failure":
        assert bodies[0]["error"] == f"Hermes exited 7: provider rejected {expected}"
    elif path == "stdout_failure":
        assert bodies[0]["error"] == f"Hermes exited 7: answer with {expected}"
    else:
        assert bodies[0]["error"] == f"failed to start Hermes with {expected}"
    diagnostics = capsys.readouterr().err
    assert "reply POST failed" in diagnostics
    assert secret not in diagnostics
    assert TOKEN not in diagnostics


def test_overlapping_credentials_redact_longer_value_first(monkeypatch):
    monkeypatch.setenv("OPENAI_API_KEY", "shared-secret")
    monkeypatch.setenv("OPENROUTER_API_KEY", "shared-secret-with-suffix")
    assert wrapper._scrub_secrets("shared-secret-with-suffix") == (
        "[REDACTED_OPENROUTER_API_KEY]"
    )


@pytest.mark.parametrize("secret_name", SECRET_NAMES)
def test_callback_boundary_scrubs_direct_success_and_error(monkeypatch, capsys, secret_name):
    secret = (
        "gateway-secret-value" if secret_name == "ACETEAM_GATEWAY_KEY"
        else f"configured-secret-{secret_name.lower()}"
    )
    monkeypatch.setenv(secret_name, secret)
    monkeypatch.setattr(wrapper, "GATEWAY_KEY", "gateway-secret-value")
    monkeypatch.setattr(wrapper, "PLATFORM_URL", "https://platform.example")
    monkeypatch.setattr(wrapper, "INSTANCE_ID", "instance-1")
    bodies = []

    def urlopen(req, timeout):
        assert timeout == 30
        assert req.get_header("Authorization") == "Bearer gateway-secret-value"
        bodies.append(json.loads(req.data))
        raise RuntimeError("network unavailable")

    monkeypatch.setattr(wrapper.urllib.request, "urlopen", urlopen)
    wrapper._post_reply({"reply": f"answer with {secret}"}, TOKEN)
    wrapper._post_reply({"error": f"retry after {secret}"}, TOKEN)

    assert bodies == [
        {"reply": f"answer with [REDACTED_{secret_name}]", "turnToken": TOKEN},
        {"error": f"retry after [REDACTED_{secret_name}]", "turnToken": TOKEN},
    ]
    diagnostics = capsys.readouterr().err
    assert diagnostics.count("reply POST failed") == 2
    assert secret not in diagnostics
    assert TOKEN not in diagnostics
