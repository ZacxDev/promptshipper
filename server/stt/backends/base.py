"""The contract every transcription backend implements.

A backend turns 16 kHz mono float32 PCM into a `Transcript`. Everything else —
auth, decoding the upload, the dead-capture guard, the response shape, the
reliability verdict — lives in `app.py`, ONCE, so that every backend produces
the same JSON for the same situation. A backend reports SIGNALS; it never
renders the response and never decides `unreliable` itself.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field

import numpy as np

FINISH_REASONS = frozenset({"stop", "length", "unknown"})


def env(name: str, default: str) -> str:
    """`os.environ[name]`, with UNSET and BLANK both meaning "use the default".

    Blank counts as unset because compose passes `VAR: ${VAR:-}` through as an
    empty string when the user left it out of .env.
    """
    raw = os.environ.get(name, "")
    return raw if raw.strip() else default


@dataclass(frozen=True)
class Transcript:
    """What a backend measured about one transcription.

    🔴 None MEANS "THIS BACKEND CANNOT MEASURE IT", NEVER "IT WAS FINE". A
    backend that has no per-token log-probabilities reports `avg_logprob=None`,
    not 0.0, and one that cannot tell whether generation hit a length cap
    reports `finish_reason="unknown"`, not "stop". `app.py` turns those into
    JSON nulls, and the client renders a null as "no signal".
    """

    text: str
    # "stop" (ended on its own) | "length" (cut off by a token cap) | "unknown".
    finish_reason: str = "unknown"
    # Mean per-token log-probability (natural log), or None.
    avg_logprob: float | None = None
    # Probability that the audio holds no speech, or None. Its SCALE is
    # backend-specific, which is why the threshold default is per backend.
    no_speech_prob: float | None = None
    # Structural verdicts that make the transcript unreliable with no threshold
    # involved, e.g. a VAD that found no speech at all. Each is a stable string
    # that lands in `unreliable_reasons`.
    flags: tuple[str, ...] = field(default_factory=tuple)

    def __post_init__(self):
        if self.finish_reason not in FINISH_REASONS:
            raise ValueError(f"finish_reason must be one of {sorted(FINISH_REASONS)}")


class BackendRequestError(Exception):
    """The backend refused THIS request (bad language, input the model rejects).

    Answered 400: retrying the same request reproduces it.
    """


class BackendUnavailable(Exception):
    """The backend could not answer for a reason that is not the caller's fault
    (an upstream is down, a model is not loaded). Answered 503, which clients
    retry. The message must never contain the string STT_API_TOKEN — the client
    treats a 503 carrying it as PERMANENT (see app.py, `require_token`).
    """


class Backend:
    """Base class. Subclasses set the class attributes and implement both methods."""

    #: The STT_BACKEND value that selects this backend.
    name: str = ""
    #: Defaults for the reliability thresholds, as env-var STRINGS (an empty
    #: string means "rule off"). Per backend because `no_speech_prob` has a
    #: different scale on every model family; STT_NO_SPEECH_MAX /
    #: STT_AVG_LOGPROB_MIN override them.
    default_no_speech_max: str = ""
    default_avg_logprob_min: str = ""

    def __init__(self) -> None:
        self.model = ""
        self.device = ""
        self.ready = False
        #: The language codes this backend accepts, or None when the backend (or
        #: its upstream) is the authority and the app should not pre-validate.
        self.languages: frozenset[str] | None = None

    def load(self) -> None:  # pragma: no cover - interface
        raise NotImplementedError

    def transcribe(self, audio: np.ndarray, language: str, punctuation: bool) -> Transcript:  # pragma: no cover
        raise NotImplementedError
