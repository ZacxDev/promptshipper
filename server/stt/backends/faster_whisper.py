"""The default backend: faster-whisper (CTranslate2 Whisper), CPU by default.

Runs on any x86-64 or arm64 box with no GPU. The model is downloaded from the
Hugging Face Hub on FIRST START into `HF_HOME` (compose mounts a volume there),
so `/healthz` answers 503 until the download and load finish.

Environment:

  STT_MODEL          Whisper size or HF repo id. Default `small` (~0.5 GB).
                     `base` / `tiny` for weak machines, `large-v3` for a GPU.
  STT_DEVICE         `cpu` (default) | `cuda` | `auto`
  STT_COMPUTE_TYPE   CTranslate2 compute type. Default `int8` (CPU-friendly).
  STT_BEAM_SIZE      Default 5.
  STT_VAD            Silero VAD pre-filter, default on. Off: `0`/`false`/`off`.
  STT_CPU_THREADS    0 = CTranslate2's default.

How the response's signals are filled, per segment-level output:

  text            the segments joined, stripped
  avg_logprob     token-weighted mean of the segments' `avg_logprob`
  no_speech_prob  the MAXIMUM segment `no_speech_prob` (Whisper scale, 0..1)
  finish_reason   always "unknown" -> `truncated: null`. Whisper reports no
                  token-cap truncation per request, and "stop" would be a claim
                  this backend cannot make.
  flags           ("vad_no_speech",) when the VAD removed ALL audio, so the
                  transcript is empty because nothing was heard
"""

from __future__ import annotations

import logging
import time

import numpy as np

from .base import Backend, BackendRequestError, Transcript, env

log = logging.getLogger("stt.faster_whisper")

VAD_NO_SPEECH = "vad_no_speech"


def _on(raw: str) -> bool:
    return raw.strip().lower() not in {"0", "false", "no", "off", ""}


def summarize(segments) -> Transcript:
    """Reduce faster-whisper segments to a Transcript. Pure, so it is testable
    without the library: any object with `.text`, `.avg_logprob`,
    `.no_speech_prob` and `.tokens` will do."""
    segments = list(segments)
    text = "".join(s.text for s in segments).strip()
    weighted, n_tokens = 0.0, 0
    for s in segments:
        n = len(getattr(s, "tokens", None) or ()) or 1
        weighted += float(s.avg_logprob) * n
        n_tokens += n
    avg = weighted / n_tokens if n_tokens else None
    nsp = max((float(s.no_speech_prob) for s in segments), default=None)
    return Transcript(text=text, finish_reason="unknown", avg_logprob=avg, no_speech_prob=nsp)


class FasterWhisperBackend(Backend):
    name = "faster-whisper"
    # Whisper's own `no_speech_threshold` default. ⚠ BORROWED, NOT MEASURED for
    # this service: Whisper uses it in a conjunction with avg_logprob < -1 to
    # SKIP a segment, while here it only LABELS a transcript. Tune it with
    # STT_NO_SPEECH_MAX against your own recordings.
    default_no_speech_max = "0.6"
    default_avg_logprob_min = ""

    def __init__(self) -> None:
        super().__init__()
        self.model = env("STT_MODEL", "small")
        self.device = env("STT_DEVICE", "cpu")
        self.compute_type = env("STT_COMPUTE_TYPE", "int8")
        self.beam_size = int(env("STT_BEAM_SIZE", "5"))
        self.vad = _on(env("STT_VAD", "1"))
        self.cpu_threads = int(env("STT_CPU_THREADS", "0"))
        self._model = None

    def load(self) -> None:
        from faster_whisper import WhisperModel  # lazy: not needed by tests

        t0 = time.time()
        log.info("loading faster-whisper model %r on %s (%s)", self.model, self.device, self.compute_type)
        self._model = WhisperModel(
            self.model,
            device=self.device,
            compute_type=self.compute_type,
            cpu_threads=self.cpu_threads,
        )
        langs = getattr(self._model, "supported_languages", None)
        self.languages = frozenset(langs) if langs else None
        self.ready = True
        log.info("model loaded in %.1fs (%s languages)", time.time() - t0,
                 "?" if self.languages is None else len(self.languages))

    def transcribe(self, audio: np.ndarray, language: str, punctuation: bool) -> Transcript:
        # `punctuation` is accepted for contract compatibility and ignored:
        # Whisper always punctuates.
        try:
            segments, _info = self._model.transcribe(
                audio,
                language=language,
                beam_size=self.beam_size,
                vad_filter=self.vad,
                condition_on_previous_text=False,
            )
            t = summarize(segments)
        except ValueError as e:
            raise BackendRequestError(f"the model refused the request: {e}") from e
        if self.vad and not t.text and t.no_speech_prob is None:
            # Zero segments: the VAD heard nothing at all. A confident,
            # structural "no speech", reported as such rather than as a silent
            # empty 200 with `unreliable: null`.
            return Transcript(text="", finish_reason="unknown", flags=(VAD_NO_SPEECH,))
        return t
