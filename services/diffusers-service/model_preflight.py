"""Free-space preflight + file-selection guard for diffusers-service's own
model weight pull (citadel #902, the #828/#840 Go-side fix's Python mirror).

Why this exists: `MODEL_CACHE_PULL` for `engine: "diffusers"` has been a
documented no-op since citadel#545 -- "the diffusers compose pins its model
and downloads weights itself on first start" -- so PR #840's Go-side
free-space preflight + allow_patterns/ignore_patterns for MODEL_CACHE_PULL
never protected this service's OWN weight pull, the plain, unguarded
`AutoPipelineForText2Image.from_pretrained(...)` call in app.py. That call is
where a multi-checkpoint repo shaped like Lightricks/LTX-Video (13 sibling
checkpoints, ~161GB total) can fill a node's disk exactly the way the
pre-#840 Go path could.

This module ports the two protections #840 added
(internal/jobs/disk_space.go's planDiskPreflight + hf_repo_size.go's
sumFilteredSize, and model_cache_pull_patterns.go's pattern parsing/matching)
to the Python side, WITHOUT reimplementing #840's auto-derivation heuristic
(deriveDiffusersAllowPatterns) -- per citadel#902's own non-goals, explicit
allow_patterns/ignore_patterns passthrough (env-var driven here, since this
service has no job payload to read one from) is a smaller, sufficient first
cut.

Deliberately import-light: this module pulls in `huggingface_hub` (a small,
metadata-only dependency, NOT `diffusers`/`torch`) so both the pure decision
functions (`plan_disk_preflight`, `patterns_include`, `parse_pattern_list`)
and the one networked function (`default_list_repo_files`) are unit-testable
without installing the multi-GB CUDA/diffusers stack app.py itself needs --
see test_model_preflight.py, which imports only this module.
"""

from __future__ import annotations

import fnmatch
import json
import logging
import os
import shutil
from dataclasses import dataclass
from typing import Callable, Iterable, Sequence

logger = logging.getLogger(__name__)

# Mirrors diskSafetyMarginBytes in internal/jobs/disk_space.go: headroom
# required ABOVE the estimated download size, since a download writes
# partial/resume files during the pull and on-disk layout can exceed the
# summed repo file sizes slightly.
DEFAULT_DISK_SAFETY_MARGIN_BYTES = 2 * 1024**3  # 2 GiB


class InsufficientDiskSpaceError(RuntimeError):
    """Raised when the preflight confirms a download will not fit. Carries a
    human-readable required-vs-available message (never just a bare number)
    so a caller that surfaces the exception message verbatim (as app.py's
    /generate handler does) still fails loud and clear."""


@dataclass(frozen=True)
class RepoFileInfo:
    """The subset of a HuggingFace repo tree entry this module needs. Mirrors
    hfTreeEntry in internal/jobs/hf_repo_size.go -- deliberately a plain
    dataclass, not huggingface_hub's own RepoFile/RepoFolder types, so tests
    can construct fixtures without touching huggingface_hub at all."""

    path: str
    size: int
    is_file: bool = True


def _human_bytes(n: int) -> str:
    """Renders a byte count as a short human-readable string (e.g. "18.7
    GiB"), used only in preflight log/error messages. Mirrors humanBytes in
    internal/jobs/disk_space.go."""
    if n < 0:
        return "-" + _human_bytes(-n)
    if n < 1024:
        return f"{n} B"
    size = float(n)
    for unit in ("KiB", "MiB", "GiB", "TiB", "PiB"):
        size /= 1024
        if size < 1024 or unit == "PiB":
            return f"{size:.1f} {unit}"
    return f"{size:.1f} PiB"  # pragma: no cover -- unreachable, satisfies linters


def hf_auth_token() -> str | None:
    """Best-effort HuggingFace auth token from the environment, mirroring
    huggingface_hub's own precedence (HF_TOKEN first, HUGGING_FACE_HUB_TOKEN
    for backward compat) and Go's hfAuthToken() in hf_repo_size.go. Does NOT
    read the stored token file (~/.cache/huggingface/token) -- a gated repo
    authorized only that way still fails the metadata fetch here, which is
    one more reason the preflight fails OPEN rather than closed on a fetch
    error (see run_preflight)."""
    return os.environ.get("HF_TOKEN") or os.environ.get("HUGGING_FACE_HUB_TOKEN") or None


def parse_pattern_list(raw: str | None) -> list[str] | None:
    """Parses an allow/ignore-pattern env var. Accepts either a JSON array
    (`["transformer/*","vae/*"]`) or a plain comma-separated list
    (`transformer/*,vae/*`) as a lenient fallback -- mirrors
    parsePatternField in internal/jobs/model_cache_pull_patterns.go so the
    same value shape works on both sides of the Go/Python split. Returns
    None (never an empty list) when unset/empty/all-whitespace -- the
    caller's signal to fall back to an unfiltered pull."""
    if raw is None:
        return None
    raw = raw.strip()
    if not raw:
        return None
    try:
        parsed = json.loads(raw)
    except (json.JSONDecodeError, ValueError):
        parsed = None
    if isinstance(parsed, list):
        cleaned = [str(item).strip() for item in parsed if str(item).strip()]
        return cleaned or None
    cleaned = [item.strip() for item in raw.split(",") if item.strip()]
    return cleaned or None


def resolve_allow_ignore_patterns(
    allow_env: str = "DIFFUSERS_ALLOW_PATTERNS",
    ignore_env: str = "DIFFUSERS_IGNORE_PATTERNS",
) -> tuple[list[str] | None, list[str] | None]:
    """Reads the optional allow/ignore pattern env vars. Both are additive
    and optional: a deploy with neither set behaves exactly as before this
    change (from_pretrained pulls the full repo snapshot, unfiltered)."""
    return parse_pattern_list(os.environ.get(allow_env)), parse_pattern_list(os.environ.get(ignore_env))


def patterns_include(
    path: str,
    allow_patterns: Sequence[str] | None,
    ignore_patterns: Sequence[str] | None,
) -> bool:
    """Decides whether `path` survives allow/ignore filtering: ignore wins
    outright, an empty/absent allow list means "everything not ignored", a
    non-empty allow list requires a positive match. Mirrors patternsInclude
    in internal/jobs/model_cache_pull_patterns.go -- but unlike the Go side
    (which had to hand-roll fnmatch semantics to match huggingface_hub's own
    matcher), `fnmatch.fnmatch` here IS that matcher, so no translation layer
    is needed and this can never drift from what `from_pretrained`'s own
    allow_patterns/ignore_patterns kwargs actually select."""
    for pattern in ignore_patterns or ():
        if fnmatch.fnmatch(path, pattern):
            return False
    if not allow_patterns:
        return True
    return any(fnmatch.fnmatch(path, pattern) for pattern in allow_patterns)


def default_list_repo_files(
    repo_id: str,
    *,
    revision: str = "main",
    token: str | None = None,
) -> list[RepoFileInfo]:
    """Production file-listing backend: HuggingFace's own tree API via
    huggingface_hub's `list_repo_tree` -- the same tree API Go's
    fetchHFRepoTree (hf_repo_size.go) calls directly, chosen there (and here)
    because it resolves LFS pointer files to their real byte size inline.
    Kept as a thin, swappable function -- never called directly from
    estimate_repo_size_bytes's signature default in a test -- so the test
    suite injects a fixture-backed `list_repo_files_fn` and touches no
    network."""
    from huggingface_hub import HfApi
    from huggingface_hub.hf_api import RepoFile

    api = HfApi(token=token)
    entries: list[RepoFileInfo] = []
    for item in api.list_repo_tree(repo_id, recursive=True, revision=revision, token=token):
        if isinstance(item, RepoFile):
            entries.append(RepoFileInfo(path=item.path, size=item.size or 0, is_file=True))
    return entries


ListRepoFilesFn = Callable[..., Iterable[RepoFileInfo]]


def estimate_repo_size_bytes(
    repo_id: str,
    *,
    revision: str = "main",
    allow_patterns: Sequence[str] | None = None,
    ignore_patterns: Sequence[str] | None = None,
    token: str | None = None,
    list_repo_files_fn: ListRepoFilesFn = default_list_repo_files,
) -> int:
    """Sums the byte size of every FILE entry `patterns_include` selects --
    mirrors sumFilteredSize in internal/jobs/hf_repo_size.go. Pure given an
    injected list_repo_files_fn; the only I/O is inside the default
    production function (default_list_repo_files), never here."""
    total = 0
    for entry in list_repo_files_fn(repo_id, revision=revision, token=token):
        if not entry.is_file:
            continue
        if not patterns_include(entry.path, allow_patterns, ignore_patterns):
            continue
        total += entry.size
    return total


def _nearest_existing_dir(path: str) -> str:
    """Walks up from `path` until it finds a directory that exists, so a
    free-space check still works before the destination cache dir has been
    created by a first-ever download (statfs-style calls require an existing
    path). Mirrors nearestExistingDir in internal/jobs/disk_space.go.

    Caveat shared with the Go original: if the bind-mounted cache dir itself
    (e.g. /root/.cache/huggingface) doesn't exist yet -- compose normally
    creates the mount point before the container starts, but a manual/
    misconfigured run could skip that -- this walks up onto the container's
    own overlay filesystem (e.g. /root), which is NOT the host disk the mount
    would serve. The free-space number in that edge case describes the wrong
    device. Not treated as a bug to fix here (same shape as the Go side);
    documented so a reader hits this note before rediscovering it.
    """
    path = os.path.abspath(path)
    while not os.path.isdir(path):
        parent = os.path.dirname(path)
        if parent == path:
            return path
        path = parent
    return path


def default_available_disk_bytes(path: str) -> int:
    """Production free-space probe: stdlib `shutil.disk_usage`, no extra
    dependency needed (unlike Go's gopsutil-backed disk_space_probe.go)."""
    return shutil.disk_usage(_nearest_existing_dir(path)).free


AvailableDiskBytesFn = Callable[[str], int]


def plan_disk_preflight(
    dir_: str,
    required_bytes: int,
    available_bytes: int,
    margin_bytes: int = DEFAULT_DISK_SAFETY_MARGIN_BYTES,
) -> None:
    """The pure decision at the heart of this fix: does available_bytes cover
    required_bytes plus margin_bytes? Mirrors planDiskPreflight in
    internal/jobs/disk_space.go byte for byte (including the message shape),
    so it is trivially unit-tested against both outcomes -- returns None
    (proceed) when it fits, raises InsufficientDiskSpaceError with a clear
    required-vs-available message when it doesn't."""
    required_bytes = max(required_bytes, 0)
    margin_bytes = max(margin_bytes, 0)
    needed = required_bytes + margin_bytes
    if available_bytes < needed:
        raise InsufficientDiskSpaceError(
            f"insufficient disk space at {dir_}: need {_human_bytes(needed)} "
            f"({_human_bytes(required_bytes)} estimated download + {_human_bytes(margin_bytes)} safety margin) "
            f"but only {_human_bytes(available_bytes)} free -- downloading nothing"
        )


def default_snapshot_download(
    repo_id: str,
    *,
    cache_dir: str,
    allow_patterns: Sequence[str] | None = None,
    ignore_patterns: Sequence[str] | None = None,
    revision: str = "main",
    token: str | None = None,
) -> str:
    """Production file-fetch backend, used ONLY when the operator has set
    explicit allow_patterns/ignore_patterns (citadel #902) -- calls
    `huggingface_hub.snapshot_download` directly rather than trusting
    `DiffusionPipeline.from_pretrained`'s identically-named kwargs.

    This is NOT interchangeable with just passing allow_patterns/
    ignore_patterns to from_pretrained: verified against the pinned
    diffusers==0.31.0 source (`DiffusionPipeline.download` in
    diffusers/pipelines/pipeline_utils.py) that those kwargs are never read
    there. `download()` computes its OWN allow/ignore patterns from
    `model_index.json`'s component list, and any kwarg on `from_pretrained`
    it doesn't recognize (including `allow_patterns`/`ignore_patterns`) is
    silently dropped with a logged "Keyword arguments {...} are not expected
    ... and will be ignored" -- not an error, so the no-op is easy to miss
    in testing. Passing our patterns straight through would therefore be a
    silent no-op, not the file-selection guard #902 asks for.

    Pre-populating the cache HERE, before `from_pretrained` ever runs, is
    what actually makes the filter apply to what lands on disk:
    `from_pretrained`'s own `download()` then finds its expected files
    already cached (its `pipeline_is_cached` fast path, no further network
    call) or fetches only the small remainder via its own safe,
    component-based selection -- never the full unfiltered repo tree.
    """
    from huggingface_hub import snapshot_download

    return snapshot_download(
        repo_id,
        cache_dir=cache_dir,
        allow_patterns=list(allow_patterns) if allow_patterns else None,
        ignore_patterns=list(ignore_patterns) if ignore_patterns else None,
        revision=revision,
        token=token,
    )


SnapshotDownloadFn = Callable[..., str]


def prefetch_filtered_weights(
    repo_id: str,
    cache_dir: str,
    *,
    allow_patterns: Sequence[str] | None = None,
    ignore_patterns: Sequence[str] | None = None,
    revision: str = "main",
    token: str | None = None,
    snapshot_download_fn: SnapshotDownloadFn = default_snapshot_download,
) -> bool:
    """Pre-populates `cache_dir` with exactly the allow/ignore-filtered
    subset of `repo_id`'s files, when either pattern list is set. Returns
    True if a prefetch ran, False if there was nothing to filter (both
    pattern lists empty) -- in the False case `from_pretrained`'s own
    unfiltered download path is unchanged from before this fix, exactly the
    "additive, optional" contract this module documents throughout.

    Deliberately does its own `snapshot_download` call rather than trusting
    `from_pretrained`'s identically-named kwargs -- see
    `default_snapshot_download`'s docstring for the verified reason those
    kwargs don't work.
    """
    if not allow_patterns and not ignore_patterns:
        return False
    snapshot_download_fn(
        repo_id,
        cache_dir=cache_dir,
        allow_patterns=allow_patterns,
        ignore_patterns=ignore_patterns,
        revision=revision,
        token=token,
    )
    return True


def run_preflight(
    repo_id: str,
    cache_dir: str,
    *,
    allow_patterns: Sequence[str] | None = None,
    ignore_patterns: Sequence[str] | None = None,
    revision: str = "main",
    token: str | None = None,
    margin_bytes: int = DEFAULT_DISK_SAFETY_MARGIN_BYTES,
    list_repo_files_fn: ListRepoFilesFn = default_list_repo_files,
    available_disk_bytes_fn: AvailableDiskBytesFn = default_available_disk_bytes,
) -> None:
    """Runs the free-space preflight for `repo_id` before `from_pretrained`
    is allowed to download anything into `cache_dir`.

    Fail-OPEN on a metadata-fetch or disk-probe error (gated repo, network
    hiccup, HF API shape change, an unreadable cache dir) -- mirrors
    internal/jobs/disk_space.go's contract exactly: a pull proceeding
    un-preflighted is preferable to blocking a legitimate download because
    OUR size estimate couldn't be computed. Fails CLOSED (raises
    InsufficientDiskSpaceError, propagated to the caller) only on a
    CONFIRMED shortfall between a successfully estimated size and
    actually-measured free space -- never on a guess.
    """
    try:
        required_bytes = estimate_repo_size_bytes(
            repo_id,
            revision=revision,
            allow_patterns=allow_patterns,
            ignore_patterns=ignore_patterns,
            token=token,
            list_repo_files_fn=list_repo_files_fn,
        )
    except Exception as exc:  # noqa: BLE001 -- deliberate fail-open, see docstring
        logger.warning(
            "diffusers preflight: could not estimate size for %s (%s) -- proceeding without a disk check",
            repo_id,
            exc,
        )
        return

    try:
        available_bytes = available_disk_bytes_fn(cache_dir)
    except Exception as exc:  # noqa: BLE001 -- same fail-open reasoning
        logger.warning(
            "diffusers preflight: could not determine free disk space at %s (%s) -- proceeding without a disk check",
            cache_dir,
            exc,
        )
        return

    plan_disk_preflight(cache_dir, required_bytes, available_bytes, margin_bytes)
    logger.info(
        "diffusers preflight: %s needs ~%s, %s free at %s -- proceeding",
        repo_id,
        _human_bytes(required_bytes),
        _human_bytes(available_bytes),
        cache_dir,
    )
