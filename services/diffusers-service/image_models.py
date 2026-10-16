"""Selectable text-to-image models for the diffusers sidecar (citadel-cli#1335).

Pure, import-light module: no torch, no diffusers, no network. It owns three
decisions so app.py and the swap manager (image_swap.py) stay mechanical and
this logic is unit-testable with nothing but the standard library:

  1. WHICH models a request may name (the allowlist, ``parse_allowlist``).
  2. HOW each one is loaded: pipeline class, dtype, and an ordered list of
     load variants (full precision, 8-bit, nf4, with or without CPU offload),
     each carrying an estimated resident-VRAM figure (``ImageModelSpec``).
  3. WHICH variant fits a VRAM budget (``choose_variant``).

Verification record (model cards and pipeline sources read 2026-10-16; see the
PR body for the full table). Every catalog entry below was checked against the
Hugging Face API for existence, ``license`` card metadata, and
``model_index.json`` (the ``_class_name`` is the pipeline class), and against
the diffusers v0.37.1 source for the pipeline's ``__call__`` signature:

  - black-forest-labs/FLUX.2-klein-4B  apache-2.0  Flux2KleinPipeline
  - Tongyi-MAI/Z-Image-Turbo           apache-2.0  ZImagePipeline
  - Qwen/Qwen-Image                    apache-2.0  QwenImagePipeline
  - stabilityai/sdxl-turbo             "other", license_name sai-nc-community
    (Stability AI Community License: research/non-commercial free; commercial
    use needs registration and ends above USD 1M annual revenue; requires
    "Powered by Stability AI" attribution). NOT permissive. It is kept only
    as the long-standing DIFFUSERS_MODEL default, not on the default
    allowlist; see DEFAULT_ALLOWLIST.

The ``est_vram_gb`` figures are ESTIMATES derived from the checkpoint sizes
(bf16 = 2 bytes/param, nf4 ~ 0.55 byte/param incl. quantization constants,
plus text encoder and activations). They gate which variant the budget allows;
they are not measurements. The swap manager records the measured delta on every
real load (``last_load`` in /health) so the first GPU acceptance run replaces
these guesses with numbers.
"""

from __future__ import annotations

import logging
import os
from dataclasses import dataclass

logger = logging.getLogger(__name__)

GIB = 1024**3

QUANT_NONE = "none"
QUANT_INT8 = "int8"
QUANT_NF4 = "nf4"
QUANT_MODES = ("auto", QUANT_NONE, QUANT_INT8, QUANT_NF4)

OFFLOAD_NONE = "none"
OFFLOAD_MODEL = "model"  # diffusers enable_model_cpu_offload()


@dataclass(frozen=True)
class LoadVariant:
    """One way to load a model. ``est_vram_gb`` is the estimated GPU memory the
    pipeline holds (peak, for offloaded variants) once loaded."""

    quant: str
    offload: str
    est_vram_gb: float

    def label(self) -> str:
        return self.quant if self.offload == OFFLOAD_NONE else f"{self.quant}+cpu-offload"


@dataclass(frozen=True)
class ImageModelSpec:
    model_id: str
    license: str
    # True only for permissive licences (Apache-2.0 here). Surfaced in /health
    # so the advertised allowlist never implies more than the cards say.
    permissive: bool
    # diffusers class name, or None to use AutoPipelineForText2Image.
    pipeline_class: str | None
    # "bfloat16" / "float16"; None means "use DIFFUSERS_DTYPE" (legacy knob).
    torch_dtype: str | None
    # Ordered best-quality-first; choose_variant takes the first that fits.
    variants: tuple[LoadVariant, ...]
    # Pipeline components bitsandbytes quantizes when a variant asks for it.
    quantize_components: tuple[str, ...] = ("transformer", "text_encoder")
    # Per-model request defaults (a request that omits the field gets these).
    default_steps: int = 4
    default_guidance: float = 0.0
    default_size: int = 512
    # Which pipeline kwarg carries the guidance strength. Qwen-Image uses
    # true_cfg_scale (guidance_scale there is for guidance-distilled variants).
    cfg_param: str = "guidance_scale"
    supports_negative_prompt: bool = True
    # Qwen-Image's true CFG needs a negative prompt to be present at all.
    requires_negative_prompt: bool = False
    # Optional file filters so the free-space preflight (model_preflight.py)
    # estimates what will actually land on disk.
    allow_patterns: tuple[str, ...] | None = None
    ignore_patterns: tuple[str, ...] | None = None
    est_disk_gb: float = 0.0
    # Coarse cold-load ETA surfaced in the warming response.
    est_load_seconds: int = 120
    notes: str = ""

    def min_vram_gb(self) -> float:
        return min(v.est_vram_gb for v in self.variants)


# ---------------------------------------------------------------------------
# Catalog
# ---------------------------------------------------------------------------

_V = LoadVariant

CATALOG: dict[str, ImageModelSpec] = {
    "stabilityai/sdxl-turbo": ImageModelSpec(
        model_id="stabilityai/sdxl-turbo",
        license="sai-nc-community (Stability AI Community License)",
        permissive=False,
        pipeline_class=None,  # AutoPipelineForText2Image, exactly as before
        torch_dtype=None,  # honours DIFFUSERS_DTYPE (default float16)
        variants=(_V(QUANT_NONE, OFFLOAD_NONE, 7.0),),
        default_steps=4,
        default_guidance=0.0,
        default_size=512,
        est_disk_gb=14.0,
        est_load_seconds=45,
        notes="Fast fallback. Legacy default; unchanged load path.",
    ),
    "black-forest-labs/FLUX.2-klein-4B": ImageModelSpec(
        model_id="black-forest-labs/FLUX.2-klein-4B",
        license="apache-2.0",
        permissive=True,
        pipeline_class="Flux2KleinPipeline",
        torch_dtype="bfloat16",
        variants=(
            _V(QUANT_NONE, OFFLOAD_NONE, 16.5),
            _V(QUANT_NONE, OFFLOAD_MODEL, 9.0),
            _V(QUANT_INT8, OFFLOAD_NONE, 9.5),
            _V(QUANT_NF4, OFFLOAD_NONE, 7.0),
        ),
        # Distilled, 4 steps. Flux2KleinPipeline takes no negative_prompt.
        default_steps=4,
        default_guidance=1.0,
        default_size=1024,
        supports_negative_prompt=False,
        # The repo also ships a 7.75 GB single-file checkpoint that duplicates
        # transformer/; the diffusers layout never reads it.
        ignore_patterns=("flux-2-klein-4b.safetensors",),
        est_disk_gb=16.0,
        est_load_seconds=90,
    ),
    "Tongyi-MAI/Z-Image-Turbo": ImageModelSpec(
        model_id="Tongyi-MAI/Z-Image-Turbo",
        license="apache-2.0",
        permissive=True,
        pipeline_class="ZImagePipeline",
        torch_dtype="bfloat16",
        variants=(
            _V(QUANT_NONE, OFFLOAD_NONE, 21.0),
            _V(QUANT_NONE, OFFLOAD_MODEL, 16.0),
            _V(QUANT_INT8, OFFLOAD_NONE, 11.5),
            _V(QUANT_NF4, OFFLOAD_NONE, 8.5),
        ),
        # Few-step distilled model: ~8 DiT forwards, guidance disabled.
        default_steps=9,
        default_guidance=0.0,
        default_size=1024,
        est_disk_gb=33.0,
        est_load_seconds=150,
    ),
    "Qwen/Qwen-Image": ImageModelSpec(
        model_id="Qwen/Qwen-Image",
        license="apache-2.0",
        permissive=True,
        pipeline_class="QwenImagePipeline",
        torch_dtype="bfloat16",
        # 20B transformer + Qwen2.5-VL-7B text encoder: ~58 GB of bf16 files,
        # so a 24 GB card shared with other services always needs nf4. There is
        # deliberately NO 8-bit variant: 8-bit would be ~28 GB resident, and
        # diffusers 0.37.1 skips CPU-offload hooks for 8-bit text encoders on
        # transformers < 4.58 (pipeline_utils.enable_model_cpu_offload), so
        # offload cannot rescue it. nf4+offload keeps one component on the GPU
        # at a time; its load-time peak is unmeasured (see PR, GPU acceptance).
        variants=(
            _V(QUANT_NF4, OFFLOAD_NONE, 17.0),
            _V(QUANT_NF4, OFFLOAD_MODEL, 12.0),
        ),
        default_steps=30,
        default_guidance=4.0,
        default_size=1024,
        cfg_param="true_cfg_scale",
        requires_negative_prompt=True,
        est_disk_gb=58.0,
        est_load_seconds=300,
        notes="Needs 8-bit or nf4; quantized at load time from the bf16 files.",
    ),
}

# Used when DIFFUSERS_ALLOWED_MODELS is unset: the permissively licensed
# (Apache-2.0) models only. sdxl-turbo (Stability AI Community Licence:
# registration, attribution, and a USD 1M annual revenue cap for commercial use)
# is NOT on it. It stays selectable only because it is the long-standing
# DIFFUSERS_MODEL default, which parse_allowlist always includes; an operator
# who wants it gone sets DIFFUSERS_MODEL to a permissive model. Open question
# for the founder (see the PR).
DEFAULT_ALLOWLIST: tuple[str, ...] = (
    "black-forest-labs/FLUX.2-klein-4B",
    "Tongyi-MAI/Z-Image-Turbo",
    "Qwen/Qwen-Image",
)


def custom_spec(model_id: str, est_vram_gb: float) -> ImageModelSpec:
    """Spec for an operator-chosen DIFFUSERS_MODEL that is not in the catalog
    (for example stabilityai/stable-diffusion-3.5-medium, documented in the
    compose file). Preserves the pre-#1335 behavior: AutoPipeline, legacy
    DIFFUSERS_DTYPE, no quantization, turbo-style defaults."""
    return ImageModelSpec(
        model_id=model_id,
        license="unverified (operator-supplied DIFFUSERS_MODEL)",
        permissive=False,
        pipeline_class=None,
        torch_dtype=None,
        variants=(_V(QUANT_NONE, OFFLOAD_NONE, est_vram_gb),),
        notes="Operator-supplied default; not in the verified catalog.",
    )


# ---------------------------------------------------------------------------
# Allowlist
# ---------------------------------------------------------------------------


def parse_allowlist(
    raw: str | None,
    default_model: str,
    *,
    custom_vram_gb: float = 8.0,
) -> dict[str, ImageModelSpec]:
    """Builds the ordered ``model_id -> spec`` allowlist.

    ``raw`` is DIFFUSERS_ALLOWED_MODELS (comma/whitespace separated). Unset or
    blank selects ``DEFAULT_ALLOWLIST``. An id that is not in ``CATALOG`` is
    skipped with a warning rather than crashing the container on a typo: only
    verified models can be served, which is the point of an allowlist.
    ``default_model`` (DIFFUSERS_MODEL) is always present: it is the operator's
    own explicit choice, so a custom id there gets a generic spec.
    """
    ids: list[str]
    if raw is None or not raw.strip():
        ids = list(DEFAULT_ALLOWLIST)
    else:
        ids = [p for p in raw.replace(",", " ").split() if p]

    allowed: dict[str, ImageModelSpec] = {}
    for model_id in ids:
        spec = CATALOG.get(model_id)
        if spec is None:
            logger.warning(
                "diffusers allowlist: %r is not in the verified catalog, skipping it. "
                "Known ids: %s",
                model_id,
                ", ".join(sorted(CATALOG)),
            )
            continue
        allowed.setdefault(model_id, spec)

    if default_model not in allowed:
        allowed[default_model] = CATALOG.get(default_model) or custom_spec(default_model, custom_vram_gb)
    return allowed


class ModelNotAllowedError(ValueError):
    """The requested model is not on the allowlist (HTTP 400)."""

    def __init__(self, requested: str, allowed: list[str]):
        self.requested = requested
        self.allowed = allowed
        super().__init__(
            f"model {requested!r} is not allowed; choose one of: {', '.join(allowed)}"
        )


def resolve_model(
    requested: str | None,
    allowed: dict[str, ImageModelSpec],
    default_model: str,
) -> ImageModelSpec:
    """Maps a request's optional ``model`` to a spec. None or blank selects the
    configured default; anything else must match an allowlisted id exactly."""
    name = (requested or "").strip()
    if not name:
        return allowed[default_model]
    spec = allowed.get(name)
    if spec is None:
        raise ModelNotAllowedError(name, list(allowed))
    return spec


# ---------------------------------------------------------------------------
# VRAM budget
# ---------------------------------------------------------------------------


class VramBudgetError(RuntimeError):
    """No load variant of the model fits the configured VRAM budget (HTTP 422).
    Not retryable until the operator raises DIFFUSERS_VRAM_BUDGET_GB or
    changes DIFFUSERS_QUANT."""

    def __init__(self, model_id: str, budget_gb: float | None, min_required_gb: float, quant_mode: str):
        self.model_id = model_id
        self.budget_gb = budget_gb
        self.min_required_gb = min_required_gb
        self.quant_mode = quant_mode
        super().__init__(
            f"{model_id} needs at least ~{min_required_gb:g} GB of VRAM with DIFFUSERS_QUANT="
            f"{quant_mode}, over the DIFFUSERS_VRAM_BUDGET_GB budget of {budget_gb:g} GB"
        )


def choose_variant(
    spec: ImageModelSpec,
    budget_gb: float | None,
    quant_mode: str = "auto",
) -> LoadVariant:
    """Picks the highest-quality variant that fits ``budget_gb``.

    Variants are declared best-quality-first, so the first fit wins: full
    precision when the budget allows, then 8-bit, then nf4 (with CPU offload as
    the last resort for the largest models). ``budget_gb=None`` means no
    budget: the first (best) variant is taken. ``quant_mode`` other than
    "auto" restricts the candidates to that quantization.
    """
    if quant_mode not in QUANT_MODES:
        raise ValueError(f"DIFFUSERS_QUANT must be one of {', '.join(QUANT_MODES)}; got {quant_mode!r}")
    candidates = [v for v in spec.variants if quant_mode == "auto" or v.quant == quant_mode]
    if not candidates:
        raise VramBudgetError(spec.model_id, budget_gb, spec.min_vram_gb(), quant_mode)
    for variant in candidates:
        if budget_gb is None or variant.est_vram_gb <= budget_gb:
            return variant
    raise VramBudgetError(
        spec.model_id, budget_gb, min(v.est_vram_gb for v in candidates), quant_mode
    )


# ---------------------------------------------------------------------------
# Env parsing
# ---------------------------------------------------------------------------


def parse_budget_gb(raw: str | None, default: float | None = 12.0) -> float | None:
    """DIFFUSERS_VRAM_BUDGET_GB. Unset -> ``default``; "0", "none" or
    "unlimited" -> no budget; otherwise a positive number of GB."""
    if raw is None or not raw.strip():
        return default
    text = raw.strip().lower()
    if text in ("0", "none", "unlimited", "off"):
        return None
    try:
        value = float(text)
    except ValueError:
        logger.warning("DIFFUSERS_VRAM_BUDGET_GB=%r is not a number, using default %s", raw, default)
        return default
    return value if value > 0 else None


def env_quant_mode() -> str:
    mode = os.environ.get("DIFFUSERS_QUANT", "auto").strip().lower() or "auto"
    if mode not in QUANT_MODES:
        logger.warning("DIFFUSERS_QUANT=%r is not one of %s, using auto", mode, ", ".join(QUANT_MODES))
        return "auto"
    return mode

