"""Finite load diagnostics, without importing ML libraries or formatting errors."""

from __future__ import annotations

import sys
from enum import Enum


class Stage(Enum):
    ESTIMATE = "estimate"
    DISK_PROBE = "disk_probe"
    PREPARE = "prepare"
    CAPACITY = "capacity"
    DRAIN = "drain"
    LOAD = "load"
    UNLOAD = "unload"
    CLEANUP = "cleanup"


_BUILTIN_LABELS = (
    (Exception, "builtins.Exception"),
    (RuntimeError, "builtins.RuntimeError"),
    (NotImplementedError, "builtins.NotImplementedError"),
    (AttributeError, "builtins.AttributeError"),
    (KeyError, "builtins.KeyError"),
    (AssertionError, "builtins.AssertionError"),
    (ImportError, "builtins.ImportError"),
    (ModuleNotFoundError, "builtins.ModuleNotFoundError"),
    (ValueError, "builtins.ValueError"),
    (TypeError, "builtins.TypeError"),
    (MemoryError, "builtins.MemoryError"),
    (OSError, "builtins.OSError"),
    (TimeoutError, "builtins.TimeoutError"),
    (ConnectionError, "builtins.ConnectionError"),
    (FileNotFoundError, "builtins.FileNotFoundError"),
    (PermissionError, "builtins.PermissionError"),
    (BrokenPipeError, "builtins.BrokenPipeError"),
)
_LOADED_LABELS = (
    ("model_preflight", "InsufficientDiskSpaceError", "model_preflight.InsufficientDiskSpaceError"),
    ("image_swap", "ModelLoadFailed", "image_swap.ModelLoadFailed"),
    ("image_swap", "InsufficientVram", "image_swap.InsufficientVram"),
    ("huggingface_hub.errors", "GatedRepoError", "huggingface_hub.errors.GatedRepoError"),
    ("huggingface_hub.errors", "RepositoryNotFoundError", "huggingface_hub.errors.RepositoryNotFoundError"),
    ("huggingface_hub.errors", "LocalEntryNotFoundError", "huggingface_hub.errors.LocalEntryNotFoundError"),
    ("huggingface_hub.errors", "EntryNotFoundError", "huggingface_hub.errors.EntryNotFoundError"),
    ("huggingface_hub.errors", "HfHubHTTPError", "huggingface_hub.errors.HfHubHTTPError"),
    ("torch", "OutOfMemoryError", "torch.OutOfMemoryError"),
    ("torch.cuda", "OutOfMemoryError", "torch.cuda.OutOfMemoryError"),
    ("torch", "AcceleratorError", "torch.AcceleratorError"),
)


def safe_exception_label(exc: BaseException) -> str:
    """Return a literal known-export label, not the exception's reflected name.

    An unknown subclass remains unknown even when its broad failure kind is
    known. Loaded export aliases choose this table's first matching literal;
    the label does not assert the class's actual __module__ or library version.
    """
    actual = type(exc)
    for cls, label in _BUILTIN_LABELS:
        if actual is cls:
            return label
    for module_name, export, label in _LOADED_LABELS:
        module = sys.modules.get(module_name)
        if module is not None and actual is vars(module).get(export):
            return label
    return "unknown_exception"


def bounded_byte_count(value) -> int | None:
    # Avoid bools, custom numeric formatting and unbounded decimal conversion.
    return value if type(value) is int and 0 <= value <= (1 << 63) - 1 else None
