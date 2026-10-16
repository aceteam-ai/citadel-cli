"""Unit tests for image_models.py (citadel-cli#1335): allowlist parsing,
request-model resolution, and VRAM-budget variant selection. Pure stdlib; no
torch, diffusers or network."""

from __future__ import annotations

import pytest

import image_models as im

DEFAULT = "stabilityai/sdxl-turbo"


def test_unset_allowlist_is_the_four_verified_models_in_order():
    allowed = im.parse_allowlist(None, DEFAULT)
    assert list(allowed) == [
        "stabilityai/sdxl-turbo",
        "black-forest-labs/FLUX.2-klein-4B",
        "Tongyi-MAI/Z-Image-Turbo",
        "Qwen/Qwen-Image",
    ]


def test_blank_allowlist_env_behaves_like_unset():
    assert list(im.parse_allowlist("  ", DEFAULT)) == list(im.DEFAULT_ALLOWLIST)


def test_env_allowlist_accepts_commas_and_spaces_and_keeps_default():
    allowed = im.parse_allowlist("Qwen/Qwen-Image, Tongyi-MAI/Z-Image-Turbo", DEFAULT)
    # The configured default is always selectable even when not listed.
    assert list(allowed) == ["Qwen/Qwen-Image", "Tongyi-MAI/Z-Image-Turbo", DEFAULT]


def test_unknown_id_in_allowlist_is_skipped_not_fatal():
    allowed = im.parse_allowlist("not/a-real-model Qwen/Qwen-Image", DEFAULT)
    assert "not/a-real-model" not in allowed
    assert "Qwen/Qwen-Image" in allowed


def test_custom_default_model_gets_a_generic_spec():
    allowed = im.parse_allowlist("Qwen/Qwen-Image", "stabilityai/stable-diffusion-3.5-medium", custom_vram_gb=11)
    spec = allowed["stabilityai/stable-diffusion-3.5-medium"]
    assert spec.pipeline_class is None  # AutoPipeline, as before #1335
    assert spec.variants[0].quant == "none"
    assert spec.variants[0].est_vram_gb == 11


def test_catalog_licences_match_the_verification_record():
    assert im.CATALOG["black-forest-labs/FLUX.2-klein-4B"].license == "apache-2.0"
    assert im.CATALOG["Tongyi-MAI/Z-Image-Turbo"].license == "apache-2.0"
    assert im.CATALOG["Qwen/Qwen-Image"].license == "apache-2.0"
    assert not im.CATALOG["stabilityai/sdxl-turbo"].license.startswith("apache")


def test_catalog_pipeline_classes():
    assert im.CATALOG["black-forest-labs/FLUX.2-klein-4B"].pipeline_class == "Flux2KleinPipeline"
    assert im.CATALOG["Tongyi-MAI/Z-Image-Turbo"].pipeline_class == "ZImagePipeline"
    assert im.CATALOG["Qwen/Qwen-Image"].pipeline_class == "QwenImagePipeline"
    assert im.CATALOG["stabilityai/sdxl-turbo"].pipeline_class is None


def test_qwen_image_is_never_offered_unquantized():
    assert all(v.quant != "none" for v in im.CATALOG["Qwen/Qwen-Image"].variants)


def test_resolve_model_none_and_blank_pick_default():
    allowed = im.parse_allowlist(None, DEFAULT)
    assert im.resolve_model(None, allowed, DEFAULT).model_id == DEFAULT
    assert im.resolve_model("   ", allowed, DEFAULT).model_id == DEFAULT


def test_resolve_model_exact_match():
    allowed = im.parse_allowlist(None, DEFAULT)
    assert im.resolve_model("Qwen/Qwen-Image", allowed, DEFAULT).model_id == "Qwen/Qwen-Image"


@pytest.mark.parametrize("bad", ["qwen/qwen-image", "Qwen/Qwen-Image-2512", "gpt-4", "../etc/passwd"])
def test_resolve_model_rejects_everything_off_the_list(bad):
    allowed = im.parse_allowlist(None, DEFAULT)
    with pytest.raises(im.ModelNotAllowedError) as err:
        im.resolve_model(bad, allowed, DEFAULT)
    assert err.value.requested == bad
    assert err.value.allowed == list(allowed)
    assert "Qwen/Qwen-Image" in str(err.value)


def test_choose_variant_takes_best_quality_that_fits():
    flux = im.CATALOG["black-forest-labs/FLUX.2-klein-4B"]
    assert im.choose_variant(flux, 24).quant == "none"
    assert im.choose_variant(flux, 24).offload == "none"
    mid = im.choose_variant(flux, 12)
    assert (mid.quant, mid.offload) == ("none", "model")
    low = im.choose_variant(flux, 7.5)
    assert low.quant == "nf4"


def test_choose_variant_no_budget_takes_first():
    zi = im.CATALOG["Tongyi-MAI/Z-Image-Turbo"]
    assert im.choose_variant(zi, None) == zi.variants[0]


def test_choose_variant_qwen_needs_quantization_on_a_shared_card():
    qwen = im.CATALOG["Qwen/Qwen-Image"]
    assert im.choose_variant(qwen, 24).quant == "int8"
    assert im.choose_variant(qwen, 18).quant == "nf4"
    v = im.choose_variant(qwen, 12)
    assert (v.quant, v.offload) == ("nf4", "model")


def test_choose_variant_over_budget_raises_with_actionable_numbers():
    qwen = im.CATALOG["Qwen/Qwen-Image"]
    with pytest.raises(im.VramBudgetError) as err:
        im.choose_variant(qwen, 8)
    assert err.value.min_required_gb == 12
    assert err.value.budget_gb == 8
    assert "DIFFUSERS_VRAM_BUDGET_GB" in str(err.value)


def test_choose_variant_forced_quant_filters_candidates():
    flux = im.CATALOG["black-forest-labs/FLUX.2-klein-4B"]
    assert im.choose_variant(flux, 24, "nf4").quant == "nf4"
    with pytest.raises(im.VramBudgetError):
        im.choose_variant(flux, 24, "int8")  # klein ships no int8 variant


def test_choose_variant_rejects_bad_mode():
    with pytest.raises(ValueError):
        im.choose_variant(im.CATALOG[DEFAULT], 24, "fp4")


@pytest.mark.parametrize(
    "raw,expected",
    [(None, 12.0), ("", 12.0), ("20", 20.0), ("7.5", 7.5), ("0", None), ("none", None), ("unlimited", None), ("abc", 12.0), ("-3", None)],
)
def test_parse_budget_gb(raw, expected):
    assert im.parse_budget_gb(raw) == expected


def test_env_quant_mode(monkeypatch):
    monkeypatch.delenv("DIFFUSERS_QUANT", raising=False)
    assert im.env_quant_mode() == "auto"
    monkeypatch.setenv("DIFFUSERS_QUANT", "NF4")
    assert im.env_quant_mode() == "nf4"
    monkeypatch.setenv("DIFFUSERS_QUANT", "bogus")
    assert im.env_quant_mode() == "auto"
