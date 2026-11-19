"""Renderer-sandbox verification for the browser-runtime image (citadel-cli#1788).

Stdlib only. The check reads /proc for the browser's process tree and refuses
(fails closed) unless Chromium is demonstrably sandboxed:

  * no process in the tree carries --no-sandbox,
  * at least one renderer exists (an empty tree proves nothing), and
  * every renderer runs in a user namespace different from the browser's AND
    carries strictly more seccomp filters (Seccomp_filters in
    /proc/<pid>/status) than the browser.

Seccomp mode 2 alone proves nothing inside a container: the engine's own profile
already puts every process, the browser included, in mode 2. Chromium's
seccomp-BPF sandbox stacks an extra filter on the renderer, so a renderer whose
filter count does not exceed the browser's has no Chromium filter (for example
--disable-seccomp-filter-sandbox). A missing Seccomp_filters field (kernel too
old to report it) fails the check rather than passing it.

Chromium retitles its processes, so /proc/<pid>/cmdline is one string with the
arguments joined by spaces; it is tokenized on whitespace.

Chromium installs both layers itself when its sandbox starts: the namespace
sandbox (clone/unshare of CLONE_NEWUSER, needs the pinned seccomp profile the run
contract passes) and the seccomp-BPF filter. If the container's own seccomp
profile blocks the namespace calls, Chromium refuses to start a renderer or, with
--no-sandbox, runs one without either layer; both are caught here.
"""

from __future__ import annotations

import os
import re
import time
from dataclasses import dataclass, field


@dataclass
class SandboxReport:
    ok: bool
    reason: str
    renderers: int = 0
    details: list[str] = field(default_factory=list)
    # retry: the verdict can still change (a process or filter is not there yet).
    retry: bool = False


def _read(path: str) -> str:
    try:
        with open(path, "rb") as fh:
            return fh.read().decode("utf-8", "replace")
    except OSError:
        return ""


def parse_status(text: str) -> dict[str, str]:
    out: dict[str, str] = {}
    for line in text.splitlines():
        key, sep, val = line.partition(":")
        if sep:
            out[key.strip()] = val.strip()
    return out


def _ppid(proc_root: str, pid: int) -> int | None:
    stat = _read(f"{proc_root}/{pid}/stat")
    # comm may contain spaces and parentheses; the fields after the LAST ')' are
    # state, ppid, ...
    _, sep, rest = stat.rpartition(")")
    fields = rest.split()
    if not sep or len(fields) < 2 or not fields[1].isdigit():
        return None
    return int(fields[1])


def _cmdline(proc_root: str, pid: int) -> list[str]:
    raw = _read(f"{proc_root}/{pid}/cmdline")
    return [a for a in re.split(r"\s+", raw.replace("\0", " ")) if a]


def _userns(proc_root: str, pid: int) -> str | None:
    try:
        return os.readlink(f"{proc_root}/{pid}/ns/user")
    except OSError:
        return None


def _filters(status: dict[str, str]) -> int | None:
    val = status.get("Seccomp_filters", "")
    return int(val) if val.isdigit() else None


def descendants(root_pid: int, proc_root: str = "/proc") -> list[int]:
    children: dict[int, list[int]] = {}
    for name in os.listdir(proc_root):
        if not name.isdigit():
            continue
        pp = _ppid(proc_root, int(name))
        if pp is not None:
            children.setdefault(pp, []).append(int(name))
    out: list[int] = []
    stack = [root_pid]
    while stack:
        for child in children.get(stack.pop(), []):
            out.append(child)
            stack.append(child)
    return out


NO_RENDERER = "no renderer process yet"
NO_FILTER = "renderer has no seccomp filter beyond the container profile"


def inspect_tree(browser_pid: int, proc_root: str = "/proc") -> SandboxReport:
    """One-shot verdict for the process tree rooted at browser_pid."""
    tree = [browser_pid, *descendants(browser_pid, proc_root)]
    browser_ns = _userns(proc_root, browser_pid)
    if browser_ns is None:
        return SandboxReport(False, "browser process not readable", retry=True)
    browser_filters = _filters(parse_status(_read(f"{proc_root}/{browser_pid}/status")))
    if browser_filters is None:
        return SandboxReport(False, "Seccomp_filters not reported for the browser process")
    renderers = 0
    waiting = False
    details: list[str] = []
    for pid in tree:
        cmd = _cmdline(proc_root, pid)
        if "--no-sandbox" in cmd:
            return SandboxReport(False, "a browser process runs with --no-sandbox")
        if "--type=renderer" not in cmd:
            continue
        renderers += 1
        ns = _userns(proc_root, pid)
        if ns is None:
            return SandboxReport(False, "renderer user namespace not readable", renderers)
        if ns == browser_ns:
            return SandboxReport(False, "renderer shares the browser's user namespace", renderers)
        rf = _filters(parse_status(_read(f"{proc_root}/{pid}/status")))
        if rf is None:
            return SandboxReport(False, "Seccomp_filters not reported for a renderer", renderers)
        if rf <= browser_filters:
            waiting = True  # the Chromium filter may not be installed yet
            continue
        details.append(f"renderer {pid}: filters {rf} > browser {browser_filters}, own userns")
    if renderers == 0:
        return SandboxReport(False, NO_RENDERER, retry=True)
    if waiting:
        return SandboxReport(False, NO_FILTER, renderers, retry=True)
    return SandboxReport(True, "sandboxed", renderers, details)


def wait_for_sandbox(
    browser_pid: int,
    timeout: float = 20.0,
    proc_root: str = "/proc",
    sleep=time.sleep,
    clock=time.monotonic,
    alive=None,
) -> SandboxReport:
    """Poll until the tree is verified sandboxed or the deadline passes. A definite
    violation (--no-sandbox, shared namespace, missing field) returns at once;
    only retryable verdicts keep waiting. `alive` (a callable) lets the caller
    report that the browser has exited, which returns at once as well."""
    deadline = clock() + timeout
    while True:
        if alive is not None and not alive():
            return SandboxReport(False, "browser exited before the sandbox was verified")
        last = inspect_tree(browser_pid, proc_root)
        if last.ok or not last.retry or clock() >= deadline:
            return last
        sleep(0.25)
