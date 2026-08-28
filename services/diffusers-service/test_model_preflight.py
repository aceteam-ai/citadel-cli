"""Unit tests for model_preflight.py's pure logic (citadel #902).

Hermetic: no network, no real HuggingFace calls, no torch/diffusers/fastapi
import. `list_repo_files_fn`/`available_disk_bytes_fn` are always injected
fakes -- this mirrors internal/jobs' TestPlanDiskPreflight /
TestSumFilteredSize / TestModelCachePullPatterns (the Go-side #828/#840 tests
this module is the Python mirror of) case-for-case where the shape carries
over, plus the parse/pattern-matching tests unique to the Python side.

Run:  python3 -m pytest services/diffusers-service/test_model_preflight.py
"""

from __future__ import annotations

import pytest

import model_preflight as mp


# ---------------------------------------------------------------------------
# parse_pattern_list / resolve_allow_ignore_patterns
# ---------------------------------------------------------------------------


def test_parse_pattern_list_json_array():
    assert mp.parse_pattern_list('["transformer/*", "vae/*"]') == ["transformer/*", "vae/*"]


def test_parse_pattern_list_comma_separated_fallback():
    assert mp.parse_pattern_list("transformer/*,vae/*") == ["transformer/*", "vae/*"]


def test_parse_pattern_list_strips_whitespace():
    assert mp.parse_pattern_list(" transformer/* , vae/* ") == ["transformer/*", "vae/*"]


@pytest.mark.parametrize("raw", [None, "", "   ", "[]", ","])
def test_parse_pattern_list_empty_is_none(raw):
    # None (not []) is the signal to fall back to an unfiltered pull --
    # mirrors parsePatternField's nil, nil in model_cache_pull_patterns.go.
    assert mp.parse_pattern_list(raw) is None


def test_resolve_allow_ignore_patterns_reads_env(monkeypatch):
    monkeypatch.setenv("DIFFUSERS_ALLOW_PATTERNS", '["transformer/*"]')
    monkeypatch.setenv("DIFFUSERS_IGNORE_PATTERNS", "*.bin")
    allow, ignore = mp.resolve_allow_ignore_patterns()
    assert allow == ["transformer/*"]
    assert ignore == ["*.bin"]


def test_resolve_allow_ignore_patterns_unset_is_none(monkeypatch):
    monkeypatch.delenv("DIFFUSERS_ALLOW_PATTERNS", raising=False)
    monkeypatch.delenv("DIFFUSERS_IGNORE_PATTERNS", raising=False)
    assert mp.resolve_allow_ignore_patterns() == (None, None)


# ---------------------------------------------------------------------------
# patterns_include
# ---------------------------------------------------------------------------


def test_patterns_include_no_filter_includes_everything():
    assert mp.patterns_include("ltx-video-fp32.safetensors", None, None) is True


def test_patterns_include_allow_list_restricts():
    assert mp.patterns_include("transformer/model.safetensors", ["transformer/*"], None) is True
    assert mp.patterns_include("ltx-video-fp32.safetensors", ["transformer/*"], None) is False


def test_patterns_include_ignore_wins_over_allow():
    assert mp.patterns_include("transformer/model.bin", ["transformer/*"], ["*.bin"]) is False


# ---------------------------------------------------------------------------
# hf_auth_token
# ---------------------------------------------------------------------------


def test_hf_auth_token_prefers_hf_token(monkeypatch):
    monkeypatch.setenv("HF_TOKEN", "primary")
    monkeypatch.setenv("HUGGING_FACE_HUB_TOKEN", "fallback")
    assert mp.hf_auth_token() == "primary"


def test_hf_auth_token_falls_back(monkeypatch):
    monkeypatch.delenv("HF_TOKEN", raising=False)
    monkeypatch.setenv("HUGGING_FACE_HUB_TOKEN", "fallback")
    assert mp.hf_auth_token() == "fallback"


def test_hf_auth_token_absent_is_none(monkeypatch):
    monkeypatch.delenv("HF_TOKEN", raising=False)
    monkeypatch.delenv("HUGGING_FACE_HUB_TOKEN", raising=False)
    assert mp.hf_auth_token() is None


# ---------------------------------------------------------------------------
# estimate_repo_size_bytes (sumFilteredSize's Python mirror)
# ---------------------------------------------------------------------------

# The LTX-Video incident shape: diffusers subfolders (the actual pipeline)
# alongside 13 sibling full-precision checkpoints that are NOT needed to load
# the pipeline via from_pretrained.
_LTX_VIDEO_SHAPED_ENTRIES = [
    mp.RepoFileInfo(path="transformer/diffusion_pytorch_model.safetensors", size=10 << 30),
    mp.RepoFileInfo(path="vae/diffusion_pytorch_model.safetensors", size=2 << 30),
    mp.RepoFileInfo(path="ltx-video-2b-v0.9.safetensors", size=100 << 30),
    mp.RepoFileInfo(path="ltx-video-2b-v0.9-fp8.safetensors", size=50 << 30),
    mp.RepoFileInfo(path="transformer", size=0, is_file=False),  # directories must be skipped
]


def _fake_list_repo_files(entries):
    def fn(repo_id, *, revision="main", token=None):
        return entries

    return fn


def test_estimate_repo_size_bytes_no_filter_sums_everything():
    got = mp.estimate_repo_size_bytes(
        "Lightricks/LTX-Video", list_repo_files_fn=_fake_list_repo_files(_LTX_VIDEO_SHAPED_ENTRIES)
    )
    assert got == 10 * (1 << 30) + 2 * (1 << 30) + 100 * (1 << 30) + 50 * (1 << 30)


def test_estimate_repo_size_bytes_allow_patterns_excludes_sibling_checkpoints():
    got = mp.estimate_repo_size_bytes(
        "Lightricks/LTX-Video",
        allow_patterns=["transformer/*", "vae/*"],
        list_repo_files_fn=_fake_list_repo_files(_LTX_VIDEO_SHAPED_ENTRIES),
    )
    want = 10 * (1 << 30) + 2 * (1 << 30)
    assert got == want, "allow_patterns must exclude the 150GB of sibling checkpoints, not just shrink the total"


# ---------------------------------------------------------------------------
# plan_disk_preflight (planDiskPreflight's Python mirror -- same cases as
# internal/jobs/disk_space_test.go's TestPlanDiskPreflight)
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "required_bytes,available_bytes,margin_bytes,want_err",
    [
        (10 << 30, 100 << 30, 2 << 30, False),  # fits comfortably
        (10 << 30, 12 << 30, 2 << 30, False),  # fits exactly at the margin boundary
        (10 << 30, (12 << 30) - 1, 2 << 30, True),  # one byte short fails closed
        (161 << 30, 50 << 30, 2 << 30, True),  # the 161GB LTX-Video incident shape
        (19 << 30, 50 << 30, 2 << 30, False),  # the fixed ~19GB diffusers-filtered pull fits
        (0, 0, 2 << 30, True),  # zero required bytes does not waive the safety margin
    ],
)
def test_plan_disk_preflight(required_bytes, available_bytes, margin_bytes, want_err):
    if want_err:
        with pytest.raises(mp.InsufficientDiskSpaceError):
            mp.plan_disk_preflight("/citadel-cache/huggingface", required_bytes, available_bytes, margin_bytes)
    else:
        mp.plan_disk_preflight("/citadel-cache/huggingface", required_bytes, available_bytes, margin_bytes)


def test_plan_disk_preflight_error_names_required_and_available():
    with pytest.raises(mp.InsufficientDiskSpaceError) as exc_info:
        mp.plan_disk_preflight("/citadel-cache/huggingface", 161 << 30, 50 << 30, 2 << 30)
    message = str(exc_info.value)
    assert "163.0 GiB" in message  # required + margin
    assert "161.0 GiB" in message  # required alone
    assert "50.0 GiB" in message  # available
    assert "/citadel-cache/huggingface" in message


# ---------------------------------------------------------------------------
# prefetch_filtered_weights -- the real file-selection guard. from_pretrained
# itself does NOT honor allow_patterns/ignore_patterns kwargs (verified
# against the pinned diffusers==0.31.0 source: DiffusionPipeline.download()
# computes its own patterns from model_index.json and silently drops
# anything else passed to from_pretrained), so this direct snapshot_download
# call is what actually makes the filter apply to what lands on disk.
# ---------------------------------------------------------------------------


def test_prefetch_filtered_weights_noop_when_no_patterns():
    calls = []
    ran = mp.prefetch_filtered_weights(
        "stabilityai/sdxl-turbo",
        snapshot_download_fn=lambda *a, **kw: calls.append((a, kw)) or "unused",
    )
    assert ran is False
    assert calls == []  # from_pretrained's own unfiltered path must be untouched


def test_prefetch_filtered_weights_calls_snapshot_download_with_patterns():
    calls = []

    def fake_snapshot_download(repo_id, *, allow_patterns, ignore_patterns, revision, token):
        calls.append(
            dict(
                repo_id=repo_id,
                allow_patterns=allow_patterns,
                ignore_patterns=ignore_patterns,
                revision=revision,
                token=token,
            )
        )
        return "unused"

    ran = mp.prefetch_filtered_weights(
        "Lightricks/LTX-Video",
        allow_patterns=["transformer/*", "vae/*"],
        ignore_patterns=["*.bin"],
        token="tok",
        snapshot_download_fn=fake_snapshot_download,
    )
    assert ran is True
    assert len(calls) == 1
    assert calls[0]["repo_id"] == "Lightricks/LTX-Video"
    assert calls[0]["allow_patterns"] == ["transformer/*", "vae/*"]
    assert calls[0]["ignore_patterns"] == ["*.bin"]
    assert calls[0]["token"] == "tok"


def test_prefetch_filtered_weights_ignore_only_still_runs():
    """A caller may set only ignore_patterns (no allow_patterns) -- still a
    real filter, so this must still prefetch rather than no-op."""
    calls = []
    ran = mp.prefetch_filtered_weights(
        "some-org/some-repo",
        ignore_patterns=["*.onnx"],
        snapshot_download_fn=lambda *a, **kw: calls.append(kw) or "unused",
    )
    assert ran is True
    assert len(calls) == 1


def test_prefetch_filtered_weights_never_passes_cache_dir():
    """Pins the citadel#902 cache-dir-parity fix. An earlier version of this
    function accepted a `cache_dir` parameter and forwarded
    DIFFUSERS_CACHE_DIR (== HF_HOME) straight to snapshot_download. Verified
    against the pinned huggingface_hub==0.26.2 source
    (`_snapshot_download.py`): `if cache_dir is None: cache_dir =
    constants.HF_HUB_CACHE`, and `HF_HUB_CACHE` defaults to `HF_HOME/hub` --
    a DIFFERENT directory than `HF_HOME` itself. Because app.py's
    `from_pretrained` call also never passes `cache_dir`, it always resolves
    to `HF_HUB_CACHE`, so passing `HF_HOME` here wrote the filtered prefetch
    to a directory `from_pretrained` never reads -- from_pretrained's
    pipeline_is_cached check would never find it and would silently
    re-download via its own (possibly still-broad) selection, AND the
    filtered subset would sit duplicated on disk for nothing. Both from
    prefetch_filtered_weights and default_snapshot_download must never pass a
    cache_dir at all, so they always defer to huggingface_hub's own default
    resolution -- the same resolution from_pretrained relies on -- and can
    never diverge from it.

    This test calls the PUBLIC prefetch_filtered_weights (not
    default_snapshot_download directly) so it also catches a regression where
    prefetch_filtered_weights re-adds a cache_dir passthrough even if
    default_snapshot_download's own signature stays clean.
    """
    calls = []

    def fake_snapshot_download(repo_id, **kwargs):
        calls.append(kwargs)
        return "unused"

    mp.prefetch_filtered_weights(
        "Lightricks/LTX-Video",
        allow_patterns=["transformer/*"],
        snapshot_download_fn=fake_snapshot_download,
    )
    assert len(calls) == 1
    assert "cache_dir" not in calls[0], (
        "prefetch_filtered_weights must never pass cache_dir to "
        "snapshot_download -- from_pretrained doesn't either, and both must "
        "resolve to the identical huggingface_hub default (HF_HUB_CACHE) or "
        "the prefetched files land somewhere from_pretrained never looks"
    )


def test_default_snapshot_download_signature_has_no_cache_dir_param():
    """Belt-and-suspenders on the same fix: default_snapshot_download's own
    signature must not accept cache_dir either, so a future caller can't
    reintroduce the bug by passing one explicitly."""
    import inspect

    params = inspect.signature(mp.default_snapshot_download).parameters
    assert "cache_dir" not in params


# ---------------------------------------------------------------------------
# run_preflight -- the end-to-end wiring, still fully hermetic via injected
# fakes for both the network call and the disk probe.
# ---------------------------------------------------------------------------


def test_run_preflight_refuses_on_confirmed_shortfall():
    """The core citadel #902 case: a repo shaped like LTX-Video (161GB
    unfiltered) against a node with only 50GB free must refuse loudly with
    required-vs-available numbers, and must download nothing."""
    with pytest.raises(mp.InsufficientDiskSpaceError) as exc_info:
        mp.run_preflight(
            "Lightricks/LTX-Video",
            "/citadel-cache/huggingface",
            list_repo_files_fn=_fake_list_repo_files(_LTX_VIDEO_SHAPED_ENTRIES),
            available_disk_bytes_fn=lambda _dir: 50 << 30,
        )
    message = str(exc_info.value)
    assert "insufficient disk space" in message
    assert "downloading nothing" in message


def test_run_preflight_allow_patterns_lets_the_filtered_pull_through():
    """The same repo, with allow_patterns scoping the pull to the pipeline
    subfolders (~12GB), fits comfortably on the same 50GB-free node --
    proves the guard is a file-selection guard, not just a blanket refusal."""
    mp.run_preflight(
        "Lightricks/LTX-Video",
        "/citadel-cache/huggingface",
        allow_patterns=["transformer/*", "vae/*"],
        list_repo_files_fn=_fake_list_repo_files(_LTX_VIDEO_SHAPED_ENTRIES),
        available_disk_bytes_fn=lambda _dir: 50 << 30,
    )  # must not raise


def test_run_preflight_proceeds_when_it_fits():
    mp.run_preflight(
        "stabilityai/sdxl-turbo",
        "/citadel-cache/huggingface",
        list_repo_files_fn=_fake_list_repo_files(
            [mp.RepoFileInfo(path="model.safetensors", size=7 << 30)]
        ),
        available_disk_bytes_fn=lambda _dir: 100 << 30,
    )  # must not raise


def test_run_preflight_fails_open_on_metadata_error(caplog):
    """A gated repo (401), a network hiccup, or an HF API shape change must
    never BLOCK a legitimate pull -- mirrors internal/jobs/disk_space.go's
    fail-open contract on a metadata-fetch error."""

    def raising_list_repo_files(repo_id, *, revision="main", token=None):
        raise RuntimeError("401 Unauthorized")

    mp.run_preflight(
        "some-org/gated-repo",
        "/citadel-cache/huggingface",
        list_repo_files_fn=raising_list_repo_files,
        available_disk_bytes_fn=lambda _dir: 1,  # would refuse if this were even consulted
    )  # must not raise -- fails open


def test_run_preflight_fails_open_on_disk_probe_error():
    """An unreadable/nonexistent cache dir must also fail open, not crash the
    model load with an unrelated OSError."""
    mp.run_preflight(
        "stabilityai/sdxl-turbo",
        "/does/not/exist",
        list_repo_files_fn=_fake_list_repo_files(
            [mp.RepoFileInfo(path="model.safetensors", size=1000 << 30)]
        ),
        available_disk_bytes_fn=lambda _dir: (_ for _ in ()).throw(OSError("no such device")),
    )  # must not raise -- fails open despite the huge required size
