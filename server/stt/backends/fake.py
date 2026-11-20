"""A deterministic backend with no model: for tests, CI and smoke-testing a deploy.

`STT_BACKEND=fake` boots instantly and transcribes every non-silent upload as
`STT_FAKE_TEXT` (default "fake transcript"). Never use it for real dictation —
it does not listen.
"""

from __future__ import annotations


import numpy as np

from .base import Backend, Transcript, env

# The 14 bare codes the Android client is allowed to send (its own allowlist).
CLIENT_LANGUAGES = frozenset(
    {"ar", "de", "el", "en", "es", "fr", "it", "ja", "ko", "nl", "pl", "pt", "vi", "zh"}
)


class FakeBackend(Backend):
    name = "fake"
    default_no_speech_max = "0.5"
    default_avg_logprob_min = ""

    def __init__(self, transcript: Transcript | None = None) -> None:
        super().__init__()
        self.model = "fake"
        self.device = "none"
        self.languages = CLIENT_LANGUAGES
        self._transcript = transcript
        self.calls: list[dict] = []

    def load(self) -> None:
        self.ready = True

    def transcribe(self, audio: np.ndarray, language: str, punctuation: bool) -> Transcript:
        self.calls.append({"samples": int(audio.size), "language": language, "punctuation": punctuation})
        if self._transcript is not None:
            return self._transcript
        return Transcript(
            text=env("STT_FAKE_TEXT", "fake transcript"),
            finish_reason="stop",
            avg_logprob=-0.1,
            no_speech_prob=0.01,
        )
