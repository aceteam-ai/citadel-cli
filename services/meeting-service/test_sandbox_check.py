"""Tests for sandbox_check against a fake /proc tree (no browser needed).

Chromium retitles its processes, so cmdline is ONE NUL-terminated string with
space-joined arguments; the fixtures use that shape.
"""

import os

import sandbox_check


def _proc(root, pid, ppid, title, filters="1", userns="user:[1]"):
    d = root / str(pid)
    (d / "ns").mkdir(parents=True)
    (d / "stat").write_text(f"{pid} (chrome (x)) S {ppid} 0 0 0\n")
    (d / "cmdline").write_bytes(title.encode() + b"\0")
    lines = ["Name:\tchrome", "Seccomp:\t2"]
    if filters is not None:
        lines.append(f"Seccomp_filters:\t{filters}")
    (d / "status").write_text("\n".join(lines) + "\n")
    os.symlink(userns, d / "ns" / "user")


def _tree(tmp_path, browser_flag="", child_flag="", renderer_filters="2", renderer_ns="user:[2]"):
    _proc(tmp_path, 10, 1, f"/usr/lib/chromium/chromium {browser_flag} --user-data-dir=/profile")
    _proc(tmp_path, 11, 10, f"/usr/lib/chromium/chromium --type=zygote {child_flag}", filters="1")
    _proc(tmp_path, 12, 11, f"/usr/lib/chromium/chromium --type=renderer {child_flag}",
          filters=renderer_filters, userns=renderer_ns)
    _proc(tmp_path, 99, 1, "unrelated --type=renderer", filters="1")
    return str(tmp_path)


def test_sandboxed_retitled_tree_passes(tmp_path):
    rep = sandbox_check.inspect_tree(10, _tree(tmp_path))
    assert rep.ok and rep.renderers == 1


def test_unrelated_process_is_ignored(tmp_path):
    assert sandbox_check.inspect_tree(10, _tree(tmp_path)).ok


def test_equal_filter_counts_fail(tmp_path):
    """Both processes show Seccomp: 2 (the engine profile); the renderer has no
    Chromium filter of its own, so this must NOT pass."""
    rep = sandbox_check.inspect_tree(10, _tree(tmp_path, renderer_filters="1"))
    assert not rep.ok and rep.retry


def test_missing_filters_field_fails(tmp_path):
    rep = sandbox_check.inspect_tree(10, _tree(tmp_path, renderer_filters=None))
    assert not rep.ok and not rep.retry


def test_shared_user_namespace_fails(tmp_path):
    rep = sandbox_check.inspect_tree(10, _tree(tmp_path, renderer_ns="user:[1]"))
    assert not rep.ok and "user namespace" in rep.reason


def test_no_sandbox_on_browser_fails(tmp_path):
    rep = sandbox_check.inspect_tree(10, _tree(tmp_path, browser_flag="--no-sandbox"))
    assert not rep.ok and "--no-sandbox" in rep.reason


def test_no_sandbox_on_child_only_fails(tmp_path):
    rep = sandbox_check.inspect_tree(10, _tree(tmp_path, child_flag="--no-sandbox"))
    assert not rep.ok and "--no-sandbox" in rep.reason


def test_no_renderer_is_not_proof(tmp_path):
    _proc(tmp_path, 10, 1, "chromium")
    assert not sandbox_check.inspect_tree(10, str(tmp_path)).ok


def test_wait_returns_violation_immediately(tmp_path):
    root = _tree(tmp_path, browser_flag="--no-sandbox")
    rep = sandbox_check.wait_for_sandbox(10, timeout=5, proc_root=root, sleep=lambda _: 1 / 0)
    assert not rep.ok


def test_wait_keeps_waiting_for_filter_then_times_out(tmp_path):
    root = _tree(tmp_path, renderer_filters="1")
    t = iter(range(0, 100))
    rep = sandbox_check.wait_for_sandbox(
        10, timeout=3, proc_root=root, sleep=lambda _: None, clock=lambda: float(next(t))
    )
    assert not rep.ok and rep.reason == sandbox_check.NO_FILTER


def test_wait_returns_at_once_when_browser_exited(tmp_path):
    root = _tree(tmp_path)
    rep = sandbox_check.wait_for_sandbox(10, proc_root=root, sleep=lambda _: 1 / 0, alive=lambda: False)
    assert not rep.ok and "exited" in rep.reason
