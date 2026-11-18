"""Tests for sandbox_check against a fake /proc tree (no browser needed)."""

import os

import sandbox_check


def _proc(root, pid, ppid, cmd, seccomp="0", userns="user:[1]"):
    d = root / str(pid)
    (d / "ns").mkdir(parents=True)
    (d / "stat").write_text(f"{pid} (chrome (x)) S {ppid} 0 0 0\n")
    (d / "cmdline").write_bytes("\0".join(cmd).encode() + b"\0")
    (d / "status").write_text(f"Name:\tchrome\nSeccomp:\t{seccomp}\n")
    os.symlink(userns, d / "ns" / "user")


def _tree(tmp_path, renderer_seccomp="2", renderer_ns="user:[2]", extra_flag=None):
    _proc(tmp_path, 10, 1, ["/usr/lib/chromium/chromium", *( [extra_flag] if extra_flag else [])])
    _proc(tmp_path, 11, 10, ["chromium", "--type=zygote"], seccomp="2", userns="user:[1]")
    _proc(tmp_path, 12, 11, ["chromium", "--type=renderer"], seccomp=renderer_seccomp, userns=renderer_ns)
    _proc(tmp_path, 99, 1, ["unrelated", "--type=renderer"], seccomp="0", userns="user:[1]")
    return str(tmp_path)


def test_sandboxed_renderer_passes(tmp_path):
    rep = sandbox_check.inspect_tree(10, _tree(tmp_path))
    assert rep.ok and rep.renderers == 1


def test_unrelated_process_is_ignored(tmp_path):
    # pid 99 is a seccomp-less "renderer" outside the browser tree.
    assert sandbox_check.inspect_tree(10, _tree(tmp_path)).ok


def test_no_seccomp_fails(tmp_path):
    assert not sandbox_check.inspect_tree(10, _tree(tmp_path, renderer_seccomp="0")).ok


def test_shared_user_namespace_fails(tmp_path):
    rep = sandbox_check.inspect_tree(10, _tree(tmp_path, renderer_ns="user:[1]"))
    assert not rep.ok and "user namespace" in rep.reason


def test_no_sandbox_flag_fails(tmp_path):
    rep = sandbox_check.inspect_tree(10, _tree(tmp_path, extra_flag="--no-sandbox"))
    assert not rep.ok and "--no-sandbox" in rep.reason


def test_no_renderer_is_not_proof(tmp_path):
    _proc(tmp_path, 10, 1, ["chromium"])
    assert not sandbox_check.inspect_tree(10, str(tmp_path)).ok


def test_wait_returns_violation_immediately(tmp_path):
    root = _tree(tmp_path, renderer_seccomp="0")
    rep = sandbox_check.wait_for_sandbox(10, timeout=5, proc_root=root, sleep=lambda _: 1 / 0)
    assert not rep.ok


def test_wait_times_out_without_renderer(tmp_path):
    _proc(tmp_path, 10, 1, ["chromium"])
    t = iter(range(0, 100))
    rep = sandbox_check.wait_for_sandbox(
        10, timeout=3, proc_root=str(tmp_path), sleep=lambda _: None, clock=lambda: float(next(t))
    )
    assert not rep.ok and rep.reason == "no renderer process yet"
