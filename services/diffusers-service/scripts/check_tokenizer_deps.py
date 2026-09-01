#!/usr/bin/env python3
"""CI smoke test: verify the diffusers sidecar's text-encoder tokenizer deps.

Real incident (2026-08-25, citadel-cli#829): LTX-Video's T5 tokenizer failed
to construct on a live node, mid-provision -- first
`ModuleNotFoundError: No module named 'tiktoken'`, then, once someone hand-
installed tiktoken, `ValueError: Error parsing tokenizer/spiece.model`
(missing sentencepiece/protobuf). The diffusers sidecar image had nothing that
exercised tokenizer construction before shipping, so the failure only
surfaced on a user's own hardware with a cryptic diffusers error -- long
after `docker build` and `docker compose up` had both reported success.

This script closes that gap. It is deliberately NOT a GPU test and does NOT
download full model weights (multi-GB shards, no GPU on CI runners). It runs
two tiers:

  1. Direct import of every pinned tokenizer dependency
     (services/diffusers-service/requirements.txt: tiktoken, sentencepiece,
     protobuf, ftfy). No network. This alone would have caught the
     ModuleNotFoundError half of the incident. HARD, deterministic failure on
     any miss -- this tier never depends on HF availability and is the
     primary gate.

  2. Tokenizer *construction* (not full pipeline/model load) for the model
     families the sidecar actually serves, downloading only the small
     tokenizer/config files (a few KB -- never the multi-GB weight shards):
       - `stabilityai/sdxl-turbo` (DIFFUSERS_MODEL default): CLIP tokenizer.
       - `Wan-AI/Wan2.1-T2V-1.3B-Diffusers` (DIFFUSERS_VIDEO_MODEL default,
         citadel #958 -- the actual video sidecar model app.py serves via
         `/generate/video`): UMT5 tokenizer, the same sentencepiece-backed T5
         family as the LTX-Video probe below. Public, ungated repo (verified
         via the HF API's file listing, no auth needed). Unlike LTX-Video,
         its `tokenizer/` subfolder ships a pre-converted `tokenizer.json`
         alongside `spiece.model`, so `AutoTokenizer` takes the fast path
         rather than the slow-to-fast conversion the incident hit -- this is
         real end-to-end coverage of the model app.py actually loads, not a
         reproduction of the incident's specific code path (the LTX-Video
         probe below still covers that).
       - `Lightricks/LTX-Video` (the model literally named in citadel-cli#829;
         no longer the served video sidecar model as of citadel #958, which
         picked Wan2.1 instead -- kept here purely as the T5/sentencepiece
         regression probe that reproduces the ORIGINAL incident's exact code
         path): T5 tokenizer. Public, ungated repo -- and, verified locally,
         its `tokenizer/` subfolder ships ONLY `spiece.model` (no
         pre-converted `tokenizer.json`), so `AutoTokenizer.from_pretrained`
         is forced through the same slow-to-fast conversion path that
         actually broke in the incident, with no `use_fast` override needed
         to prove it: with sentencepiece uninstalled this raises
         `ValueError: Cannot instantiate this tokenizer from a slow version.
         If it's based on sentencepiece, make sure you have sentencepiece
         installed.` -- the same failure class (root file: spiece.model) as
         the incident's second error.
       - `stabilityai/stable-diffusion-3.5-medium` (the documented
         DIFFUSERS_MODEL alternative) uses the identical T5 tokenizer
         machinery (its `tokenizer_3/spiece.model` is a second instance of
         the same file shape), but the repo is HF-gated (auto-approval,
         still requires an authenticated, license-accepted token) --
         downloading its tokenizer files anonymously 401s. Rather than depend
         on a citadel-cli repo secret that may not exist, this probe runs
         (and is real signal) whenever an `HF_TOKEN` env var is available,
         and is skipped otherwise. The Wan2.1 and LTX-Video probes above are
         what keep tier 2 a hard, token-free gate on the actual named models.

  ANY network/access failure in tier 2 (HF unreachable, gated-repo 401, rate
  limiting, DNS) is logged as a SKIP, never a failure -- an HF outage is not a
  citadel-cli regression, and this job runs on every PR (not just ones that
  touch the sidecar), so it must not redden the build on an external service
  hiccup. This is a real trade-off: it means tier 2 cannot promise "ran
  successfully in this CI run" the way tier 1 can. Tier 1 (fully offline) is
  what makes the ModuleNotFoundError class of failure a deterministic gate
  regardless of HF/network state; tier 2 adds strictly-better signal on top
  when HF is reachable, and degrades to "not proven this run" rather than a
  false-positive failure when it isn't. A dependency-shaped failure raised
  *during* a successful download (ImportError, or a tokenizer library raising
  because a backend is missing/broken -- e.g. the sentencepiece-uninstalled
  cases verified locally) is always a HARD failure: those exceptions are not
  OSError subclasses, so they are never mistaken for a network issue.

Usage:
    python3 services/diffusers-service/scripts/check_tokenizer_deps.py

Exit code 0 on success (including soft-skips), non-zero on any hard failure.
"""

from __future__ import annotations

import sys
import traceback

# Network/access-related failures we always treat as soft skips in tier 2.
# HF Hub's gating/auth/HTTP errors (GatedRepoError, RepositoryNotFoundError,
# HfHubHTTPError, requests' ConnectionError/Timeout, plain urllib errors) all
# ultimately subclass OSError -- which the tokenizer-construction failures we
# actually care about (ModuleNotFoundError, ImportError, ValueError parsing a
# vocab/model file, RuntimeError) do NOT. That split is what lets this script
# shrug off an HF outage while still failing hard on a broken dependency,
# verified locally: with sentencepiece uninstalled, both the LTX-Video and
# gated SD-3.5 probes raise ValueError/ImportError (hard fail), never OSError.
NETWORK_SKIP_EXCEPTIONS = (OSError,)

FAILURES: list[str] = []
SKIPS: list[str] = []


def _fail(label: str, exc: BaseException) -> None:
    FAILURES.append(label)
    print(f"[FAIL] {label}")
    traceback.print_exception(type(exc), exc, exc.__traceback__)
    print()


def _skip(label: str, reason: str) -> None:
    SKIPS.append(label)
    print(f"[SKIP] {label}: {reason}")


def _ok(label: str) -> None:
    print(f"[ OK ] {label}")


# ---------------------------------------------------------------------------
# Tier 1: direct import of every pinned tokenizer dependency. No network.
# This is the hard, deterministic gate -- see module docstring for why tier 2
# cannot be one.
# ---------------------------------------------------------------------------

def check_pinned_imports() -> None:
    pinned_modules = {
        "tiktoken": "tiktoken",
        "sentencepiece": "sentencepiece",
        "protobuf": "google.protobuf",
        "ftfy": "ftfy",
        "transformers": "transformers",
    }
    for package_name, module_name in pinned_modules.items():
        label = f"import {module_name} (pip package {package_name})"
        try:
            __import__(module_name)
        except Exception as exc:  # ModuleNotFoundError is the common case
            _fail(label, exc)
        else:
            _ok(label)


# ---------------------------------------------------------------------------
# Tier 2: tokenizer construction for the sidecar's served model families.
# ---------------------------------------------------------------------------

def _construct_tokenizer(label: str, *, repo_id: str, subfolder: str | None = None,
                          token: str | None = None) -> None:
    from transformers import AutoTokenizer

    try:
        tok = AutoTokenizer.from_pretrained(repo_id, subfolder=subfolder, token=token)
    except NETWORK_SKIP_EXCEPTIONS as exc:
        # Always a skip, never a failure -- see module docstring. Whether this
        # probe is "expected" to need a token or not, an OSError here means we
        # learned nothing about our own dependency pins.
        _skip(label, f"{type(exc).__name__}: {exc}")
        return
    except Exception as exc:  # dependency-shaped failure: always hard
        _fail(label, exc)
        return

    if tok is None:  # pragma: no cover - defensive, from_pretrained shouldn't return None
        _fail(label, RuntimeError("AutoTokenizer.from_pretrained returned None"))
        return

    # A real, minimal exercise of the tokenizer -- not just "an object came
    # back". This is exactly what failed at generation time in the incident.
    try:
        tok("citadel diffusers sidecar tokenizer smoke test")
    except Exception as exc:
        _fail(f"{label} (encode)", exc)
        return

    _ok(label)


def check_served_model_tokenizers() -> None:
    import os

    # Empty string (e.g. an unset GitHub Actions secret still resolves the
    # env var, just to "") must read as "no token", not as a token literal.
    hf_token = os.environ.get("HF_TOKEN") or None

    # SDXL-Turbo: the sidecar's DIFFUSERS_MODEL default
    # (services/diffusers-service/app.py). Public repo, CLIP tokenizer -- end
    # to end construction + encode for the model the sidecar actually ships
    # with by default.
    _construct_tokenizer(
        "stabilityai/sdxl-turbo tokenizer (CLIP, subfolder=tokenizer)",
        repo_id="stabilityai/sdxl-turbo",
        subfolder="tokenizer",
    )

    # Wan-AI/Wan2.1-T2V-1.3B-Diffusers: the sidecar's DIFFUSERS_VIDEO_MODEL
    # default (citadel #958) -- the model actually served by /generate/video.
    # Public/ungated (verified via the HF API's file listing, no auth
    # needed). UMT5 tokenizer, same sentencepiece-backed T5 family as the
    # LTX-Video probe below; its tokenizer/ subfolder ships a pre-converted
    # tokenizer.json alongside spiece.model, so AutoTokenizer takes the fast
    # path here rather than the slow-to-fast conversion the LTX-Video probe
    # exercises -- this probe's job is proving the ACTUAL served video model
    # loads end to end, not reproducing the incident's specific code path.
    _construct_tokenizer(
        "Wan-AI/Wan2.1-T2V-1.3B-Diffusers tokenizer (UMT5, subfolder=tokenizer)",
        repo_id="Wan-AI/Wan2.1-T2V-1.3B-Diffusers",
        subfolder="tokenizer",
    )

    # Lightricks/LTX-Video: the model citadel-cli#829 is literally about. No
    # longer the served video sidecar model as of citadel #958 (which picked
    # Wan2.1 instead, probed above) -- kept here purely as the regression
    # probe for the incident's exact code path. Public/ungated. Its
    # tokenizer/ subfolder ships only spiece.model (verified locally via the
    # HF API's file listing -- no tokenizer.json), so this reproduces the
    # incident's actual code path with no options needed: AutoTokenizer is
    # forced through the slow-to-fast sentencepiece conversion regardless.
    _construct_tokenizer(
        "Lightricks/LTX-Video tokenizer (T5, subfolder=tokenizer)",
        repo_id="Lightricks/LTX-Video",
        subfolder="tokenizer",
    )

    # The documented DIFFUSERS_MODEL alternative (Dockerfile/app.py/citadel.yaml
    # comments). Its tokenizer_3/spiece.model is a second instance of the same
    # file shape as LTX-Video's. The repo is HF-gated (auto-approval, still
    # needs an authenticated + license-accepted token), so anonymous access
    # 401s -- this probe is real signal whenever HF_TOKEN is configured for
    # this repo, and a clearly-logged skip otherwise. The Wan2.1 and
    # LTX-Video probes above are what keep tier 2 a hard, token-free gate.
    _construct_tokenizer(
        "stabilityai/stable-diffusion-3.5-medium tokenizer "
        "(T5, subfolder=tokenizer_3, gated -- requires HF_TOKEN)",
        repo_id="stabilityai/stable-diffusion-3.5-medium",
        subfolder="tokenizer_3",
        token=hf_token,
    )


def main() -> int:
    print("== Tier 1: pinned tokenizer dependency imports (no network) ==")
    check_pinned_imports()

    print()
    print("== Tier 2: tokenizer construction for served model families ==")
    check_served_model_tokenizers()

    print()
    if FAILURES:
        print(f"RESULT: FAIL ({len(FAILURES)} failure(s), {len(SKIPS)} skip(s))")
        for label in FAILURES:
            print(f"  - {label}")
        return 1

    print(f"RESULT: PASS ({len(SKIPS)} skip(s))")
    for label in SKIPS:
        print(f"  - skipped: {label}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
