"""Turn-token contract for both hosted agent runtime wrappers.

Run: python3 -m pytest tests/test_hosted_turn_token.py
"""

import asyncio
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace
from urllib.error import HTTPError

import pytest
from fastapi import HTTPException


ROOT = Path(__file__).resolve().parents[1]


class InboundRequest:
    def __init__(self, body):
        self.body = body

    async def json(self):
        return self.body


class DeferredThread:
    def __init__(self, target, args, daemon):
        assert daemon is True
        self.target = target
        self.args = args
        self.started = False

    def start(self):
        self.started = True

    def finish(self):
        assert self.started
        self.target(*self.args)


@pytest.fixture(params=["claudecode", "hermes"])
def runtime(request, monkeypatch):
    name = request.param
    path = ROOT / "services" / f"{name}-service" / "wrapper.py"
    spec = importlib.util.spec_from_file_location(f"{name}_token_test", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    monkeypatch.setattr(module, "INSTANCE_ID", "same-instance")
    monkeypatch.setattr(module, "PLATFORM_URL", "https://platform.example")
    monkeypatch.setattr(module, "GATEWAY_KEY", "gateway-secret-value")
    pending = []

    def make_thread(*, target, args, daemon):
        thread = DeferredThread(target, args, daemon)
        pending.append(thread)
        return thread

    monkeypatch.setattr(module, "threading", SimpleNamespace(Thread=make_thread))
    return module, name, pending


def receive(module, body):
    return asyncio.run(module.hooks_agent(
        InboundRequest(body), authorization="Bearer hooks_gateway-secret-value"
    ))


@pytest.mark.parametrize("outcome", ["reply", "error"])
def test_each_wrapper_posts_exact_token_with_auth(runtime, monkeypatch, outcome):
    module, name, pending = runtime
    calls = []

    class Response:
        def __enter__(self):
            return self

        def __exit__(self, *_):
            pass

        def read(self):
            return b'{}'

    def urlopen(req, timeout):
        calls.append((req, timeout))
        return Response()

    monkeypatch.setattr(module.urllib.request, "urlopen", urlopen)

    def run(_message):
        if outcome == "error":
            raise RuntimeError("model failed")
        return "answer"

    monkeypatch.setattr(module, f"_run_{'claude' if name == 'claudecode' else 'hermes'}_turn", run)
    token = "opaque-turn-A+/="
    assert receive(module, {"message": "hello", "name": "Kickoff", "turnToken": token}) == {"delivered": True}
    assert len(pending) == 1
    pending[0].finish()
    assert len(calls) == 1
    req, timeout = calls[0]
    assert timeout == 30
    assert req.full_url == "https://platform.example/api/instances/same-instance/reply"
    assert req.get_header("Authorization") == "Bearer gateway-secret-value"
    assert json.loads(req.data) == {"turnToken": token, outcome: "answer" if outcome == "reply" else "model failed"}


def test_missing_token_fails_closed_without_callback(runtime, monkeypatch, capsys):
    module, _, pending = runtime
    calls = []
    monkeypatch.setattr(module.urllib.request, "urlopen", lambda *args, **kwargs: calls.append(args))
    for value in (None, "", 7):
        body = {"message": "private-message", "turnToken": value, "other": "private-payload"}
        with pytest.raises(HTTPException) as exc:
            receive(module, body)
        assert exc.value.status_code == 400
        assert exc.value.detail == "missing turn token"
    assert pending == []
    module._post_reply({"reply": "private-payload"}, "")
    assert calls == []
    diagnostics = capsys.readouterr().err
    assert diagnostics == f"{module.app.title.split('-')[1]}: missing turn token; cannot post reply\n"
    for secret in ("private-message", "private-payload", "gateway-secret-value", "same-instance"):
        assert secret not in diagnostics


def test_wrong_bearer_still_rejected_before_turn(runtime):
    module, _, pending = runtime
    with pytest.raises(HTTPException) as exc:
        asyncio.run(module.hooks_agent(
            InboundRequest({"message": "hello", "turnToken": "turn-A"}),
            authorization="Bearer wrong-key",
        ))
    assert exc.value.status_code == 401
    assert pending == []


def test_late_turn_a_never_uses_retry_turn_b_token(runtime, monkeypatch):
    module, name, pending = runtime
    bodies = []

    class Response:
        def __enter__(self):
            return self

        def __exit__(self, *_):
            pass

        def read(self):
            return b'{}'

    def urlopen(req, timeout):
        bodies.append(json.loads(req.data))
        return Response()

    monkeypatch.setattr(module.urllib.request, "urlopen", urlopen)
    monkeypatch.setattr(module, f"_run_{'claude' if name == 'claudecode' else 'hermes'}_turn", lambda message: message)
    receive(module, {"message": "answer A", "turnToken": "turn-A"})
    receive(module, {"message": "answer B", "turnToken": "turn-B"})
    pending[1].finish()
    pending[0].finish()
    assert bodies == [
        {"reply": "answer B", "turnToken": "turn-B"},
        {"reply": "answer A", "turnToken": "turn-A"},
    ]


@pytest.mark.parametrize("failure", ["http", "network"])
def test_callback_failure_diagnostics_redact_sensitive_values(runtime, monkeypatch, capsys, failure):
    module, _, _ = runtime
    secret = "sensitive-error-reason"

    def fail(req, timeout):
        if failure == "http":
            raise HTTPError(req.full_url, 502, secret, {}, None)
        raise RuntimeError(secret)

    monkeypatch.setattr(module.urllib.request, "urlopen", fail)
    module._post_reply({"error": "private-payload"}, "opaque-turn-A")
    diagnostics = capsys.readouterr().err
    assert "reply POST failed" in diagnostics
    for value in (secret, "opaque-turn-A", "private-payload", "gateway-secret-value", "same-instance"):
        assert value not in diagnostics
