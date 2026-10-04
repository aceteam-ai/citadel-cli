"""Credential redaction at Claude CLI and callback error boundaries."""

import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace

import pytest


@pytest.fixture
def wrapper(monkeypatch):
    path = Path(__file__).with_name("wrapper.py")
    spec = importlib.util.spec_from_file_location("claudecode_redaction_test", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    monkeypatch.setattr(module, "INSTANCE_ID", "instance-1")
    monkeypatch.setattr(module, "PLATFORM_URL", "https://platform.example")
    monkeypatch.setattr(module, "GATEWAY_KEY", "gateway-secret-value")
    return module


TOKEN = "123e4567-e89b-42d3-a456-426614174000"
SECRET_NAMES = (
    "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN",
    "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "GOOGLE_API_KEY",
    "OPENAI_API_KEY", "ACETEAM_GATEWAY_KEY",
)


def record_callback(wrapper, monkeypatch, fail=False):
    bodies = []

    class Response:
        def __enter__(self):
            return self

        def __exit__(self, *_):
            pass

        def read(self):
            return b"{}"

    def urlopen(req, timeout):
        assert timeout == 30
        assert req.get_header("Authorization") == "Bearer gateway-secret-value"
        bodies.append(json.loads(req.data))
        if fail:
            raise RuntimeError("network unavailable")
        return Response()

    monkeypatch.setattr(wrapper.urllib.request, "urlopen", urlopen)
    return bodies


@pytest.mark.parametrize("secret_name", SECRET_NAMES)
def test_failed_stderr_redacts_live_credentials_before_callback_or_log(
    wrapper, monkeypatch, capsys, secret_name
):
    secret = f"configured-secret-{secret_name.lower()}"
    monkeypatch.setenv(secret_name, secret)
    bodies = record_callback(wrapper, monkeypatch, fail=True)
    monkeypatch.setattr(wrapper.subprocess, "run", lambda *args, **kwargs: SimpleNamespace(
        returncode=7, stderr=f"rate limit from provider; credential={secret}; retry later",
        stdout="unused",
    ))

    wrapper._process_turn("hello", TOKEN)

    assert bodies == [{
        "error": f"Claude Code exited 7: rate limit from provider; "
                 f"credential=[REDACTED_{secret_name}]; retry later",
        "turnToken": TOKEN,
    }]
    assert secret not in json.dumps(bodies)
    diagnostics = capsys.readouterr().err
    assert "reply POST failed" in diagnostics
    assert secret not in diagnostics
    assert TOKEN not in diagnostics


@pytest.mark.parametrize("stream", ["stdout_fallback", "json_error", "plain_reply"])
def test_other_cli_output_paths_redact_credentials(wrapper, monkeypatch, stream):
    secret = "configured-anthropic-secret-123"
    monkeypatch.setenv("ANTHROPIC_AUTH_TOKEN", secret)
    bodies = record_callback(wrapper, monkeypatch)
    if stream == "stdout_fallback":
        proc = SimpleNamespace(returncode=9, stderr="", stdout=f"provider rejected {secret}")
    elif stream == "json_error":
        proc = SimpleNamespace(returncode=0, stderr="", stdout=json.dumps({
            "is_error": True, "result": f"provider rejected {secret}"
        }))
    else:
        proc = SimpleNamespace(returncode=0, stderr="", stdout=json.dumps({
            "result": f"answer with {secret}"
        }))
    monkeypatch.setattr(wrapper.subprocess, "run", lambda *args, **kwargs: proc)

    wrapper._process_turn("hello", TOKEN)

    assert len(bodies) == 1
    assert bodies[0]["turnToken"] == TOKEN
    assert secret not in json.dumps(bodies)
    if stream == "plain_reply":
        assert bodies[0]["reply"] == "answer with [REDACTED_ANTHROPIC_AUTH_TOKEN]"
    else:
        assert "provider rejected [REDACTED_ANTHROPIC_AUTH_TOKEN]" in bodies[0]["error"]


def test_unexpected_subprocess_error_is_scrubbed(wrapper, monkeypatch):
    secret = "configured-anthropic-secret-123"
    monkeypatch.setenv("ANTHROPIC_AUTH_TOKEN", secret)
    bodies = record_callback(wrapper, monkeypatch)

    def fail(*args, **kwargs):
        raise OSError(f"failed to start Claude with {secret}")

    monkeypatch.setattr(wrapper.subprocess, "run", fail)
    wrapper._process_turn("hello", TOKEN)
    assert bodies == [{
        "error": "failed to start Claude with [REDACTED_ANTHROPIC_AUTH_TOKEN]",
        "turnToken": TOKEN,
    }]


def test_plain_diagnostic_and_token_stay_exact(wrapper, monkeypatch):
    monkeypatch.setenv("ANTHROPIC_AUTH_TOKEN", "configured-anthropic-secret-123")
    bodies = record_callback(wrapper, monkeypatch)
    monkeypatch.setattr(wrapper.subprocess, "run", lambda *args, **kwargs: SimpleNamespace(
        returncode=3, stderr="provider rate limited; retry in 5 seconds", stdout="",
    ))
    wrapper._process_turn("hello", TOKEN)
    assert bodies == [{
        "error": "Claude Code exited 3: provider rate limited; retry in 5 seconds",
        "turnToken": TOKEN,
    }]


def test_scrub_occurs_before_stderr_tail_is_taken(wrapper, monkeypatch):
    secret = "configured-anthropic-secret-123"
    monkeypatch.setenv("ANTHROPIC_AUTH_TOKEN", secret)
    bodies = record_callback(wrapper, monkeypatch)
    monkeypatch.setattr(wrapper.subprocess, "run", lambda *args, **kwargs: SimpleNamespace(
        returncode=5, stderr=f"{secret}{'x' * 790}", stdout="",
    ))
    wrapper._process_turn("hello", TOKEN)
    assert secret not in bodies[0]["error"]
    assert secret[-12:] not in bodies[0]["error"]


def test_overlapping_secrets_and_short_placeholders(wrapper, monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "shared-secret")
    monkeypatch.setenv("ANTHROPIC_AUTH_TOKEN", "shared-secret-with-suffix")
    monkeypatch.setenv("GOOGLE_API_KEY", "short")
    assert wrapper._scrub_secrets("shared-secret-with-suffix and short") == (
        "[REDACTED_ANTHROPIC_AUTH_TOKEN] and short"
    )


@pytest.mark.parametrize("secret_name", SECRET_NAMES)
@pytest.mark.parametrize("path", ["structured_success", "structured_error", "raw_success", "unexpected"])
def test_every_credential_stays_out_of_all_callback_paths(
    wrapper, monkeypatch, capsys, secret_name, path
):
    secret = (
        "gateway-secret-value" if secret_name == "ACETEAM_GATEWAY_KEY"
        else f"configured-secret-{secret_name.lower()}"
    )
    monkeypatch.setenv(secret_name, secret)
    bodies = record_callback(wrapper, monkeypatch, fail=True)
    if path.startswith("structured"):
        # Escape one ASCII character so the raw JSON stream does not contain
        # the literal secret. Decoding must scrub the newly materialized value.
        escaped = secret.replace(secret[0], f"\\u{ord(secret[0]):04x}", 1)
        stdout = (
            '{"is_error":true,"result":"provider rejected ' + escaped + '"}'
            if path == "structured_error"
            else '{"result":"answer with ' + escaped + '"}'
        )
        proc = SimpleNamespace(returncode=0, stderr="", stdout=stdout)
        monkeypatch.setattr(wrapper.subprocess, "run", lambda *args, **kwargs: proc)
    elif path == "raw_success":
        proc = SimpleNamespace(returncode=0, stderr="", stdout=f"answer with {secret}")
        monkeypatch.setattr(wrapper.subprocess, "run", lambda *args, **kwargs: proc)
    else:
        def fail(*args, **kwargs):
            raise OSError(f"failed to start Claude with {secret}")
        monkeypatch.setattr(wrapper.subprocess, "run", fail)

    if path == "structured_success":
        assert wrapper._run_claude_turn("hello") == (
            f"answer with [REDACTED_{secret_name}]"
        )
    wrapper._process_turn("hello", TOKEN)

    assert len(bodies) == 1
    assert bodies[0]["turnToken"] == TOKEN
    assert secret not in json.dumps(bodies)
    expected = f"[REDACTED_{secret_name}]"
    if path in ("structured_success", "raw_success"):
        assert bodies[0]["reply"] == f"answer with {expected}"
    elif path == "structured_error":
        assert bodies[0]["error"] == f"Claude Code reported an error: provider rejected {expected}"
    else:
        assert bodies[0]["error"] == f"failed to start Claude with {expected}"
    diagnostics = capsys.readouterr().err
    assert "reply POST failed" in diagnostics
    assert secret not in diagnostics
    assert TOKEN not in diagnostics


@pytest.mark.parametrize("secret_name", SECRET_NAMES)
def test_callback_boundary_scrubs_direct_success_and_error(
    wrapper, monkeypatch, capsys, secret_name
):
    secret = (
        "gateway-secret-value" if secret_name == "ACETEAM_GATEWAY_KEY"
        else f"configured-secret-{secret_name.lower()}"
    )
    monkeypatch.setenv(secret_name, secret)
    bodies = record_callback(wrapper, monkeypatch, fail=True)
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
