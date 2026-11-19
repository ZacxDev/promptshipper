"""Backend registry. `STT_BACKEND` picks one; an unknown name refuses boot.

Each backend module is imported LAZILY, only when selected, so the default
image never needs torch and the test suite never needs a model library.
"""

from __future__ import annotations

import importlib

from .base import Backend, BackendRequestError, BackendUnavailable, Transcript

DEFAULT_BACKEND = "faster-whisper"

# name -> "module:Class". The ledger of every selectable backend; the README's
# backend table and tests/test_backends.py both pin it.
REGISTRY = {
    "faster-whisper": "backends.faster_whisper:FasterWhisperBackend",
    "cohere": "backends.cohere:CohereBackend",
    "openai": "backends.openai_compat:OpenAICompatBackend",
    "fake": "backends.fake:FakeBackend",
}


class UnknownBackend(ValueError):
    pass


def create(name: str) -> Backend:
    """Instantiate (but do not load) the backend called `name`."""
    target = REGISTRY.get(name.strip().lower())
    if target is None:
        raise UnknownBackend(
            f"STT_BACKEND={name!r} is not a known backend; choose one of "
            f"{', '.join(sorted(REGISTRY))}"
        )
    module_name, class_name = target.split(":")
    cls = getattr(importlib.import_module(module_name), class_name)
    return cls()


__all__ = [
    "Backend",
    "BackendRequestError",
    "BackendUnavailable",
    "DEFAULT_BACKEND",
    "REGISTRY",
    "Transcript",
    "UnknownBackend",
    "create",
]
